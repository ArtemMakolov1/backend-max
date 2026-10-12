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

Образы экспортёров, Alertmanager и Prometheus собираются из фиксированных
коммитов официальных релизов с исправленными Go и зависимостями в
`docker/monitoring/`. Экспортёры и Alertmanager используют Go 1.26.9,
Prometheus — Go 1.27.2; HTTP/2-зависимость `golang.org/x/net` обновлена до
v0.60.0 вместе с требуемыми совместимыми зависимостями. Компилятор и исходники
закреплены по digest и SHA256. UI Prometheus берётся из отдельного официального
release-архива, проверяется по SHA256 и встраивается в пересобранный сервер.
Multi-architecture образы GHCR закреплены по digest, содержат SBOM и provenance
и проходят тот же Trivy-контроль, что образ приложения. Это относится и к
Node Exporter.

В тесте гонки CAS у Alertmanager явный барьер через существующий интерфейс
`Limits` устраняет зависимость от планировщика CI. Production-код и проверки
доставки не изменены; перед патчем проверяется SHA256 исходного теста, а при
сборке выполняется полный набор upstream-тестов.

Grafana использует официальный slim core с фиксированным digest. В этой сборке
Prometheus включён как внутренний core datasource: backend пересобран из
фиксированного коммита официального релиза с Go 1.27.2 и `x/net` v0.60.0,
официальные frontend assets сохранены. CI проверяет загрузку внутреннего
источника, доступность его frontend module и реальный запрос к Prometheus.
Установка внешних плагинов и автоматические обновления отключены; исключения
для unsigned plugins не включаются. Файлы datasource принадлежат root,
а корневая файловая система доступна только для чтения.
Внешние плагины ищутся в пустом неизменяемом каталоге:
старые плагины в постоянном томе данных не загружаются. Prometheus использует
пересобранный образ с фиксированным digest и встроенным официальным UI.

Security workflow проверяет все шесть образов мониторинга на обеих архитектурах,
включая Alertmanager, без исключений для уязвимостей. Monitoring policy проверяет,
что digest каждого развёрнутого образа совпадает с образом в соответствующей
проверке безопасности.

Workflow `Build patched monitoring images` собирает отдельный компонент или
все шесть образов, публикует обе архитектуры с SBOM и provenance и сканирует
именно полученные digest. Артефакт `monitoring-image-references` содержит
проверяемые ссылки `repository:tag@sha256:...` для последующего обновления
Compose и security matrix. Новые deployment pins устанавливаются только
после успешной сборки и проверок, без исключений для уязвимостей.
При отдельной сборке Grafana параметр `prometheus_image` обязателен: это
`ghcr.io/<owner>/maxposty-prometheus@sha256:...` проверенного пересобранного
Prometheus. Runtime-проверка выполняется на той же паре образов, что deployment.
Если образ Grafana уже собран, параметр `grafana_image` позволяет повторить
runtime-проверку и оба Trivy-сканирования того же digest без пересборки.
Workflow сверяет опубликованный tag, обе архитектуры и attestation manifests;
обычная сборка из закреплённых исходников остаётся вариантом по умолчанию.
Полный набор upstream-тестов Prometheus запускается без унаследованных OTEL
настроек BuildKit: его Unix tracing endpoint конфликтует с собственным TLS
HTTP fixture. Production-код и проверки upstream-тестов не изменяются.

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

Только в Environment `production` при необходимости внешних уведомлений добавьте
`ALERTMANAGER_WEBHOOK_URL` — полный HTTPS URL приватного операторского
receiver'а (например, relay, который доставляет уведомления в MAX, Telegram или
почту). URL является секретом: его нельзя добавлять в repository variables,
compose или документацию. Deployment рендерит его в файл `0600`, копирует в
закрытый Docker volume и проверяет health Alertmanager и его регистрацию в
Prometheus. Пока URL не задан, метрики, правила и Alertmanager продолжают
работать локально, но outbound-доставка намеренно отключена; это не блокирует
релиз.

Пароль monitoring-роли и `GRAFANA_SECRET_KEY` нельзя менять обычным application
deploy: pre-migration rollback и fallback monitoring должны оставаться
работоспособными. Для ротации сначала обновите роль/конфигурацию на VPS, затем
синхронно замените GitHub secret и выполните контролируемый restart.

## Что собирается

- HTTP RPS, 4xx/5xx, p95 и in-flight запросы;
- ошибки, p95 и медленные запросы приложения к БД;
- состояние `database/sql` pool;
- операции публикации MAX и планировщик;
- попытки загрузки вложений по типу и результату, их размер и время до готового
  preview;
- доля опубликованных постов с медиа, несколькими вложениями, видео и смешанным
  набором изображений/видео;
- DAU/WAU/MAU и снимок текущего состояния воронки без PII в метриках;
- подключения, транзакции, deadlock, cache hit и `pg_stat_statements`;
- занятые/ожидающие соединения PgBouncer;
- CPU, RAM и filesystem VPS;
- результат последней ежедневной зашифрованной копии, реального restore drill,
  физического PITR base backup и состояние PostgreSQL WAL archiver.

Метрики вложений используют только ограниченные labels и не содержат имён
файлов, MIME-строк, ID пользователей или ключей S3:

- `maxposty_attachment_uploads_total{type,outcome}` — все попытки загрузки;
- `maxposty_attachment_upload_ready_duration_seconds{type,outcome}` — время от
  начала HTTP-загрузки до готовности объекта к preview;
- `maxposty_attachment_upload_size_bytes{type}` — распределение размера;
- `maxposty_media_operations_total{operation,outcome}` — запись и orphan-cleanup;
- `maxposty_product_published_posts{kind}` — снимок опубликованных постов, где
  `kind` принимает `total`, `media`, `multiple`, `video` или `mixed`.

Для процента успешных загрузок используйте отношение `outcome="success"` ко
всем попыткам. Отказы `unsupported`, `too_large`, `quota_exceeded`, `busy` и
`canceled` считаются отдельно. Время до preview измеряется сервером и не
включает последующую пользовательскую редактуру или публикацию в MAX.

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
