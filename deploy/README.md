# Production deployment: maxposty.ru

Backend разворачивается из GitHub Actions в GHCR и затем на один VPS. Основной
production-домен — `https://maxposty.ru`. Frontend-репозиторий владеет Caddy,
портами `80/443` и внешней Docker-сетью `maxposty-edge`. Caddy направляет
`/api/*` и `/media/*` на `backend:8080`; порт Go API на хост не публикуется.

До настройки DNS workflow по умолчанию использует `DEPLOY_STAGE=bootstrap` и
показывает публичную часть через `http://178.159.94.83`. Это строго
fail-closed режим: Яндекс OAuth, MAX и OpenAI обязаны быть пустыми, метод входа
недоступен, а все кабинеты и приватные API по-прежнему требуют серверную сессию
и отвечают `401`. Bootstrap не является production и не подходит для реальных
пользователей или данных.

После готовности DNS/HTTPS переключите GitHub variable
`DEPLOY_STAGE=production`. Тогда валидатор потребует реальные секреты Яндекс
OAuth и MAX и зафиксирует публичные адреса:

- Яндекс OAuth callback:
  `https://maxposty.ru/api/v1/auth/yandex/callback`;
- webhook общего бота MAX:
  `https://maxposty.ru/api/v1/webhooks/max`;
- frontend origin и canonical API origin: `https://maxposty.ru`.

## Одноразовая подготовка VPS

На сервере нужны Docker Engine, Docker Compose v2, `bash`, `tar`, `flock`,
`readlink`, `sha256sum`, `curl`, `jq` и OpenSSL 3, а также отдельный SSH-пользователь
`maxposty-deploy`. Пользователь должен владеть
`/opt/maxposty/backend` и иметь доступ к Docker. Членство в группе `docker`
эквивалентно root-доступу, поэтому ключ этого пользователя должен применяться
только из защищённого GitHub Environment.

Используйте отдельную пару SSH-ключей для CI, отключите вход по паролю после
проверки ключа и сохраните фактический host key сервера в секрете
`VPS_SSH_KNOWN_HOSTS`. Не используйте `StrictHostKeyChecking=no` и не получайте
host key внутри workflow: это лишает проверку сервера смысла.

Frontend и backend могут стартовать в любом порядке: оба deploy-скрипта под
общей host-блокировкой идемпотентно создают либо проверяют локальную bridge-сеть
`maxposty-edge` с меткой MaxPosty. Go API при этом не публикует порт на хост и
до запуска Caddy остаётся недоступен извне.

Релизы хранятся раздельно:

- `/opt/maxposty/backend/releases/<commit-sha>` — immutable bundle конкретного
  коммита, его compose-файл и закрытые env/release metadata;
- `/opt/maxposty/backend/current` — атомарно заменяемая ссылка на принятый релиз;
- `/opt/maxposty/backend/certs`, `backups` и `hooks` — общие
  операционные данные, которые не удаляются вместе с релизом.

Compose и оба helper-скрипта PostgreSQL/PgBouncer берутся непосредственно из
каталога конкретного релиза. Workflow не перезаписывает общий runtime-код до
health-check, поэтому предыдущий bundle остаётся самодостаточным для rollback.

Не удаляйте каталог, на который указывает `current`, и предыдущий релиз,
необходимый для rollback. Старые более ранние каталоги можно удалять только
отдельной операторской процедурой после проверки текущего релиза и backup.

## GitHub Environments `bootstrap` и `production`

Создайте repository variable `DEPLOY_ENABLED=false`. Переключайте её на `true`
только после подготовки VPS, установки deploy-ключа и сохранения проверенного
`known_hosts`; так первый push выполнит проверки и сборку образа, но не запустит
заведомо неуспешный SSH-деплой.

`DEPLOY_STAGE` также обязан быть repository variable: именно он до старта job
выбирает Environment. Значение `bootstrap` выбирает Environment `bootstrap`, а
`production` — отдельный Environment `production`. Не храните `DEPLOY_STAGE`
только как environment variable: она становится доступной слишком поздно для
безопасного выбора набора секретов.

Создайте оба Environment и запретите deployment из веток, отличных от `main`.
Для полностью автоматического деплоя не включайте required reviewers или wait
timer: тесты и security-gates остаются обязательными внутри workflow. Если вам
нужно ручное подтверждение каждого релиза, required reviewers можно включить,
но тогда это будет автоматический запуск с ручным approve, а не автодеплой.
В Environment `bootstrap` добавьте только:

- `VPS_SSH_PRIVATE_KEY` — отдельный приватный deploy-ключ;
- `VPS_SSH_KNOWN_HOSTS` — заранее проверенная строка host key для VPS;
- `POSTGRES_OWNER_PASSWORD` и `POSTGRES_APP_PASSWORD` — разные значения из
  `openssl rand -hex 32`.
- `POSTGRES_MONITOR_PASSWORD`, `GRAFANA_ADMIN_PASSWORD` и
  `GRAFANA_SECRET_KEY` — ещё три независимых значения из
  `openssl rand -hex 32`.

В `bootstrap` не должно быть Yandex, MAX или OpenAI secrets. В Environment
`production` добавьте те же SSH/DB secrets с теми же значениями и дополнительно:

- `YANDEX_CLIENT_ID` и `YANDEX_CLIENT_SECRET`;
- `MAX_BOT_TOKEN` — новый токен одного общего бота;
- `MAX_WEBHOOK_SECRET` — значение из `openssl rand -hex 32`;
- `S3_HOST`, `S3_ACCESS_KEY` и `S3_SECRET_KEY` — значения
  приватного S3-compatible хранилища. Они обязательны только для production и
  передаются только backend-контейнеру.

Опциональные Environment variables `S3_BUCKET` и `S3_REGION` задавайте только
если провайдер не может определить bucket или регион автоматически. Без
`S3_BUCKET` ключ должен иметь `ListBuckets` и видеть ровно один bucket.
Bucket должен быть приватным, а ключ — ограничен нужным bucket и минимальным
набором операций: `HeadBucket`, `PutObject`, `GetObject` и `DeleteObject`;
`ListBuckets` нужен только при пустом `S3_BUCKET`. Backend проверяет эти
операции служебным объектом при запуске и удаляет его сразу после чтения. В bootstrap все пять S3
переменных рендерятся пустыми, даже если одноимённые значения случайно попали в
окружение runner.

Добавьте production variable `OBSERVABILITY_ADMIN_USERS` — непустой CSV точных
логинов или effective ID (PSUID) операторов Яндекса без пробелов, например
`makolov99,123456789`. Только эти аккаунты проходят `forward_auth` к Grafana;
в `bootstrap` значение обязано оставаться пустым.

В `production` также обязательны те же три monitoring secrets:
`POSTGRES_MONITOR_PASSWORD`, `GRAFANA_ADMIN_PASSWORD` и
`GRAFANA_SECRET_KEY`. Имена и значения должны совпадать с `bootstrap`, если
оба Environment используются для одного PostgreSQL volume. Подробная схема
доступа и список дашбордов описаны в `monitoring/README.md`.

`OPENAI_API_KEY` можно добавить в тот же Environment позже. Без него production
безопасно запускает Яндекс-вход и интеграцию MAX, но не создаёт OpenAI-клиенты:
health endpoint показывает `openai_configured=false` и
`research_configured=false`, а AI-маршруты явно отвечают `503` с кодами
`openai_not_configured` или `openai_research_not_configured`. После добавления
ключа достаточно повторного deployment; хранить пустой secret в GitHub не
требуется.

Добавьте repository variables:

- `VPS_HOST=178.159.94.83` (это значение уже используется как безопасный
  fallback);
- `VPS_PORT=22`;
- `VPS_USER=maxposty-deploy`;
- `DEPLOY_STAGE=bootstrap` до готовности DNS/HTTPS, затем `production`;
- опционально `BACKUP_RETENTION_DAYS`.

`YANDEX_ALLOWED_USERS`, `MAX_CA_CERT_FILE`, `S3_BUCKET` и `S3_REGION`, если они
нужны, добавляйте только как variables Environment `production`. Workflow передаёт production
интеграционные secrets исключительно шагу рендера production-конфига; bootstrap
job их не получает.

`MAX_CA_CERT_FILE`, если нужен, должен указывать только внутрь `/app/certs`,
например `/app/certs/max-official-chain.pem`. Сам проверенный PEM размещается на
VPS вручную в `/opt/maxposty/backend/certs`; сертификаты и ключи не передаются
через репозиторий.

Пароль VPS, токены и ключи нельзя добавлять в repository variables, workflow
или `.env` в git. Любой пароль или токен, ранее отправленный в чат, issue или
лог, перед production следует заменить.

## Как проходит деплой

`Backend production deploy` автоматически стартует для каждого push в `main`.
Первый job ждёт и проверяет через Actions API успешные push-запуски `Backend CI`
и `Backend Security` для того же commit SHA; при красной проверке deployment не
начинается. Прямой push-trigger исключает гонку, при которой один workflow уже
завершился, а второй ещё стоял в очереди и нового события запуска деплоя больше
не возникало. Ручной запуск на `main` проходит ту же exact-SHA проверку. Workflow:

1. требует зелёные exact-SHA CI, CodeQL, reachable vulnerability scan и Trivy
   filesystem scan на secrets и `HIGH`/`CRITICAL` misconfiguration;
2. повторно проверяет целостность Go-модулей и reachable-уязвимости;
3. блокирует не-expand SQL-миграции статическим compatibility guard;
4. собирает multi-arch Linux-образ (`amd64` и `arm64`) с SBOM и provenance;
5. публикует SHA-тег и `latest` в GHCR, получает digest manifest list;
6. Trivy `v0.72.0` сканирует именно опубликованный digest отдельно для
   `linux/amd64` и `linux/arm64` и блокирует deploy при любой `HIGH`/`CRITICAL`
   OS или library уязвимости;
7. создаёт валидированный `.env.production.next` из выбранного Environment и
   выбранного `DEPLOY_STAGE`;
8. передаёт versioned bundle и конфиг по SSH с обязательной проверкой host key;
9. подключается к GHCR временным токеном в отдельном `DOCKER_CONFIG`;
10. проверяет PostgreSQL/PgBouncer, останавливает старый backend и создаёт
    согласованный `pg_dump`, архив media volume и SHA-256 manifest; dump и архив
    проверяются на читаемость до продолжения;
11. шифрует snapshot публичным recovery-сертификатом, загружает его в draft
    GitHub Release, сверяет GitHub digest, повторно скачивает и проверяет SHA-256,
    публикует release и требует `immutable=true`;
12. применяет только additive-миграции отдельной owner-ролью;
13. запускает новый backend по проверенному digest и после health-check атомарно
    запускает exporter'ы, Prometheus и Grafana, проверяет их health и только
    затем атомарно переключает `current`;
14. при ошибке восстанавливает PostgreSQL, PgBouncer и backend из предыдущего
    versioned compose/env/image bundle. При неудаче первого deploy удаляет
    непринятые контейнеры, сохраняя data volumes. Схема БД не откатывается вниз
    и обязана быть совместимой назад.

GitHub concurrency и серверный `flock` не допускают параллельные выкладки.
Образ всегда разворачивается по неизменяемому `ghcr.io/...@sha256:<digest>`, а
не по mutable tag. Токен GHCR после команды удаляется вместе с временным Docker
config. Во время согласованного backup, offsite hook и миграции backend находится
в maintenance window; длительность в основном зависит от размера media и hook.

Именованный `media-data` volume временно сохраняется для совместимости и
rollback старых релизов. После S3 cutover архив из deployment snapshot содержит
только legacy-файлы из этого volume и не является резервной копией S3 bucket.
Для bucket отдельно включите versioning/retention или резервное копирование у
провайдера и регулярно проверяйте восстановление объектов.

## Backup hook и восстановление

Локальные pre-migration DB/media snapshots и checksum manifest хранятся в
`/opt/maxposty/backend/backups` с правами `0600`. В bootstrap отсутствие offsite
hook даёт предупреждение. Production deploy fail-closed требует исполняемый файл
`/opt/maxposty/backend/hooks/after-backup` и не запускает миграцию, если файла нет
или он вернул ненулевой код.

Версионированный hook `deploy/backup/after-backup-github-release.sh` и публичный
RSA-3072 сертификат `deploy/backup/recipient-cert.pem` устанавливаются workflow
атомарно. Приватный recovery-ключ никогда не хранится в git, GitHub Secrets или
на VPS. Потеря этого ключа делает зашифрованные копии невосстановимыми, поэтому
его нужно отдельно сохранить в password manager или на зашифрованном offline
носителе. Перед первым production deploy в настройках репозитория обязательно
включается **Release immutability**.

Hook вызывается от пользователя deploy с четырьмя позиционными аргументами:

1. абсолютный путь к PostgreSQL custom dump;
2. абсолютный путь к `media.tar.gz`;
3. абсолютный путь к SHA-256 manifest;
4. immutable GHCR digest готовящегося релиза.

Короткоживущий `GITHUB_TOKEN` с `contents: write` доступен только deploy job. На
VPS он записывается во временный curl config с правами `0600`, не передаётся в
argv и удаляется вместе с временным Docker config. Hook проверяет имена, права и
manifest локальных файлов, создаёт потоковый CMS AES-256-GCM контейнер, загружает
его как единственный release asset и обязательно скачивает обратно до миграции.
Неверный remote digest, размер, повторный hash или отсутствие immutable-флага
останавливает deployment и запускает предыдущий backend.

Один asset ограничен операционным порогом 512 MiB (ниже лимита GitHub 2 GiB),
а upload и контрольное скачивание имеют отдельные десятиминутные таймауты.
Encrypted immutable Release является проверяемой off-host копией и безопасно
разблокирует текущий небольшой проект, но тот же GitHub account всё ещё может
удалить release целиком. При приближении к порогу или росте требований эту схему
нужно заменить на полноценную независимую 3-2-1 копию через restic и отдельное
S3-compatible хранилище.

Restore drill выполняйте только в изолированном окружении с OpenSSL 3,
`sha256sum`, `jq` и совместимым `pg_restore`. Скачайте `.tar.cms` asset и
запустите:

```bash
OPENSSL_BIN=/opt/homebrew/opt/openssl@3/bin/openssl \
  ./deploy/backup/restore-verify.sh \
  maxposty-backup-YYYYMMDDTHHMMSSZ.tar.cms \
  ~/.maxposty/recovery/backup-private-key.pem \
  /tmp/maxposty-restore
```

Скрипт проверяет AEAD, allowlist путей, metadata, оба слоя SHA-256,
`pg_restore --list` и media archive. Затем создайте чистую PostgreSQL 18 с
owner/app-ролями, восстановите custom dump, распакуйте media в пустой volume и
запустите сохранённый immutable digest. До признания drill успешным проверьте
health endpoint, вход, tenant isolation, загрузку media и планировщик. Никогда не
проверяйте restore поверх рабочей production БД или production media volume.

## Правило expand-contract

Обычный CI/deploy допускает только additive expand-миграции. Скрипт
`validate-expand-contract-migrations.sh` консервативно запрещает `DROP`,
`TRUNCATE`, `DELETE FROM`, rename/type/not-null изменения и другие очевидно
несовместимые операции. Это guardrail, а не доказательство совместимости: автор
обязан проверить, что предыдущая версия backend продолжит читать и писать схему
после миграции.

Удаление колонок/таблиц, rename и прочая contract-фаза выполняются отдельной
ручной процедурой только после того, как новый код стабильно развернут, старые
релизы выведены из эксплуатации и offsite backup проверен. Уже применённые SQL
файлы никогда не редактируются. Application rollback возвращает versioned bundle,
но намеренно не делает автоматический restore/down-migration БД, чтобы не
уничтожить записи, созданные после backup.

Смена паролей PostgreSQL заблокирована обычным deploy-скриптом: простой перезапуск
контейнеров не меняет пароль уже созданной роли. Для ротации нужен отдельный
регламент: сначала `ALTER ROLE`, затем синхронное обновление GitHub secrets и
контролируемый перезапуск PgBouncer/backend.

## Первичная настройка общего бота MAX

После того как DNS, HTTPS и Caddy уже работают, установите
`DEPLOY_STAGE=production` и запустите workflow для `main`. Операторская команда
сначала проверит публичный endpoint без redirect и только затем обновит
существующую подписку общего бота штатным `POST /subscriptions` на события
`bot_started`, `message_callback`, `message_created`, `bot_added`, `bot_removed`.
Удалять подписку перед обновлением не нужно: так не возникает разрыва доставки.
После обновления команда сверяет URL и обязательные события через
`GET /subscriptions`.

Каждый успешный автоматический production deploy повторяет эту сверку уже после
активации здорового релиза. При ручном запуске её можно явно отключить input-ом
`configure_max_webhook`; bootstrap никогда не изменяет внешнюю подписку MAX.
