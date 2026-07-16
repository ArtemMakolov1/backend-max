# Мониторинг MaxPosty

Стек включает Prometheus, Alertmanager, Grafana, `postgres_exporter`,
`pgbouncer_exporter` и `node_exporter`. В production наружу не публикуется ни
один порт мониторинга:

- Prometheus и exporter'ы находятся только во внутренней сети `monitoring`;
- Alertmanager также доступен только Prometheus во внутренней сети и отправляет
  сгруппированные срабатывания во внешний операторский HTTPS webhook;
- `postgres_exporter` подключается к PostgreSQL отдельной read-only ролью
  `maxstudio_monitor` с системной ролью `pg_monitor`;
- `pgbouncer_exporter` имеет только право чтения служебной БД `pgbouncer`;
- Grafana доступна Caddy только по отдельной сети `maxposty-monitoring-edge`;
  её auth-proxy принимает identity-заголовки лишь от фиксированного адреса
  Caddy, а backend/frontend не имеют маршрута к Grafana;
- публичный адрес — `https://maxposty.ru/monitoring/`, перед ним Caddy обязан
  проверять обычную серверную сессию Яндекс ID, удалять входящие
  `X-WEBAUTH-*` и самостоятельно устанавливать `X-WEBAUTH-USER` и
`X-WEBAUTH-ROLE`.

Production exporter images are rebuilt from the exact upstream release
commits with the patched Go toolchain and dependencies in
`docker/monitoring/`. The resulting multi-architecture GHCR manifests are
pinned by digest, carry SBOM/provenance attestations and pass the same Trivy
gate as the application image. Grafana is also pinned to the exact clean
official image digest selected by that gate.

`OBSERVABILITY_ADMIN_USERS` — comma-separated allowlist идентификаторов
операторов. Backend endpoint `/api/v1/observability/auth` разрешает доступ
только совпавшим Яндекс ID или логинам и возвращает Caddy
`X-WEBAUTH-USER`/`X-WEBAUTH-ROLE: Viewer`; остальные аккаунты получают `403`.
Значение не является Grafana secret и не передаётся exporter'ам или Prometheus.

Локально Grafana привязана только к `127.0.0.1:3000` и использует обычный
логин `admin` с `GRAFANA_ADMIN_PASSWORD`. Production Basic Auth и anonymous
access отключены, а auth proxy включён.

Если backend или основная БД недоступны и Яндекс-проверка не может выполниться,
Prometheus остаётся доступен только через SSH-туннель с VPS:

```bash
ssh -L 19090:127.0.0.1:19090 <пользователь>@178.159.94.83
```

После этого аварийный интерфейс открывается на `http://127.0.0.1:19090`.
Порт VPS слушает только loopback; без SSH он недоступен из интернета.

## Обязательные секреты deployment

Добавьте в GitHub Environment `production` (и в `bootstrap`, если он ещё
используется) три независимых значения:

```bash
openssl rand -hex 32 # POSTGRES_MONITOR_PASSWORD
openssl rand -hex 32 # GRAFANA_ADMIN_PASSWORD
openssl rand -hex 32 # GRAFANA_SECRET_KEY
```

Секреты называются строго:

- `POSTGRES_MONITOR_PASSWORD`;
- `GRAFANA_ADMIN_PASSWORD`;
- `GRAFANA_SECRET_KEY`.

Только в Environment `production` добавьте
`ALERTMANAGER_WEBHOOK_URL` — полный HTTPS URL приватного операторского
receiver'а (например, relay, который доставляет уведомления в MAX, Telegram или
почту). URL является секретом: его нельзя добавлять в repository variables,
compose или документацию. Deployment рендерит его в файл `0600`, копирует в
закрытый Docker volume и проверяет health Alertmanager и его регистрацию в
Prometheus. В `bootstrap` outbound receiver намеренно отключён.

Пароль monitoring-роли и `GRAFANA_SECRET_KEY` нельзя менять обычным application
deploy: pre-migration rollback и fallback monitoring должны оставаться
работоспособными. Для ротации сначала обновите роль/конфигурацию на VPS, затем
синхронно замените GitHub secret и выполните контролируемый restart.

## Что собирается

- HTTP RPS, 4xx/5xx, p95 и in-flight запросы;
- ошибки, p95 и медленные запросы приложения к БД;
- состояние `database/sql` pool;
- операции публикации MAX и планировщик;
- DAU/WAU/MAU и снимок текущего состояния воронки без PII в метриках;
- подключения, транзакции, deadlock, cache hit и `pg_stat_statements`;
- занятые/ожидающие соединения PgBouncer;
- CPU, RAM и filesystem VPS;
- результат последней ежедневной зашифрованной копии, реального restore drill,
  физического PITR base backup и состояние PostgreSQL WAL archiver.

Панель долгих запросов показывает нормализованные SQL-шаблоны из
`pg_stat_statements` длиной до 300 символов. PostgreSQL заменяет значения
параметров на `$1`, `$2` и т. д.; exporter дополнительно исключает запросы
владельца БД и monitoring-роли. В Prometheus попадают не более 25 актуальных
шаблонов, поэтому секреты настройки ролей не экспортируются, а кардинальность
метрик остаётся ограниченной.
Utility-команды отключены в `pg_stat_statements`, чтобы операционные команды и
SQL-комментарии с литералами не попадали в панель.

Prometheus хранит до 30 дней данных, но ограничивает TSDB размером 5 GB.
Дашборды provisioned как read-only:

- `MaxPosty — продукт и надёжность`;
- `MaxPosty — API и приложение`;
- `MaxPosty — PostgreSQL, PgBouncer и VPS`.

Правила Prometheus покрывают target down, 5xx, p95, SQL errors/slow queries,
долгие транзакции, ошибки и зависание планировщика, насыщение пулов,
PostgreSQL connections/deadlocks, очередь PgBouncer, диск и память, а также
просроченную/неуспешную копию, restore drill, PITR base backup и WAL archiving.
Alertmanager отправляет firing и resolved события во внешний операторский
receiver. Receiver должен отвечать `2xx`; собственные retry и группировка
Alertmanager защищают оператора от одиночных кратковременных сбоев.

## Локальный запуск и проверка

Заполните `.env` по `.env.example`, затем:

```bash
docker compose up --build -d
docker compose ps
```

Grafana: `http://127.0.0.1:3000`. Prometheus и exporter'ы намеренно нельзя
открыть с host network; их состояние видно в Grafana или через
`docker compose exec`.

Проверка конфигурации без запуска всего приложения:

```bash
./deploy/tests/monitoring-policy.sh
```
