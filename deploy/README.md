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
- `/opt/maxposty/backend/runtime/metrics` — атомарные textfile-метрики
  ежедневных backup/restore/PITR проверок для `node_exporter`.

Compose и оба helper-скрипта PostgreSQL/PgBouncer берутся непосредственно из
каталога конкретного релиза. Workflow не перезаписывает общий runtime-код до
health-check. До запуска миграций предыдущий bundle остаётся самодостаточным для
rollback. После запуска миграций применяется только roll-forward: staged env и
immutable image metadata сохраняются, а старый backend автоматически не
запускается против уже изменённой схемы и S3-семантики.

Не удаляйте каталог, на который указывает `current`, и предыдущий релиз,
необходимый для pre-migration rollback и восстановления мониторинга. Старые
более ранние каталоги можно удалять только отдельной операторской процедурой
после проверки текущего релиза и backup.

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
- `ALERTMANAGER_WEBHOOK_URL` — опциональный приватный HTTPS webhook
  операторского relay для внешней доставки firing/resolved алертов. Это
  secret, а не variable; без него Alertmanager работает локально и не блокирует
  production deploy;
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

Квота медиа резервируется атомарно до записи объекта. По умолчанию одному
пользователю доступны `500` файлов и `1 GiB`; оставленные загрузки удаляются
через `24h` фоновым заданием пачками по `50`. Один файл изображения ограничен
`50 MiB`, видео — `250 MiB`; эти прикладные пределы не настраиваются через
deployment environment. При необходимости задайте
production variables `MEDIA_USER_MAX_FILES`, `MEDIA_USER_MAX_BYTES`,
`MEDIA_ORPHAN_GRACE_PERIOD`, `MEDIA_CLEANUP_INTERVAL` и
`MEDIA_CLEANUP_BATCH_SIZE`. Старые локальные изображения при этом переходе не
переносятся — это подтверждённая одноразовая очистка legacy-данных.
Количество сохраняемых командных workspace на владельца ограничивает
`WORKSPACE_MAX_OWNED_TEAM_WORKSPACES` (по умолчанию `5`, максимум `1000`). Архивные
workspace остаются в лимите, потому что их media-данные не удаляются при архивации.
Для повышения лимита измените variable и повторите deployment; миграция не требуется.

После S3 cutover проверьте три сценария: загрузку галереи из нескольких
изображений, загрузку видео близкого к ожидаемому рабочему размеру и удаление
одного вложения из черновика. Затем убедитесь, что в Grafana появились
`maxposty_attachment_uploads_total` и
`maxposty_attachment_upload_ready_duration_seconds`, а спустя grace period
orphan-cleanup уменьшает зарезервированную quota. Документы и аудио в текущий
release не входят: штатный attachment API должен отклонять их как
неподдерживаемый тип, а обходной upload endpoint создавать нельзя.

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
- опционально `BACKUP_RETENTION_DAYS`;
- опционально `PITR_RETENTION_DAYS` (по умолчанию `7`, максимум `90`).
- опционально `MEDIA_USER_MAX_FILES`, `MEDIA_USER_MAX_BYTES`,
  `MEDIA_ORPHAN_GRACE_PERIOD`, `MEDIA_CLEANUP_INTERVAL` и
  `MEDIA_CLEANUP_BATCH_SIZE` для изменения безопасных лимитов медиа.
- опционально `WORKSPACE_MAX_OWNED_TEAM_WORKSPACES` (по умолчанию `5`,
  допустимо от `1` до `1000`).

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
    согласованный локальный `pg_dump`; dump проверяется через `pg_restore --list`
    до продолжения, а media из S3 во время deploy не копируются;
11. применяет только additive-миграции отдельной owner-ролью;
12. запускает новый backend по проверенному digest и после health-check атомарно
    запускает exporter'ы, Alertmanager, Prometheus и Grafana, проверяет их
    health, Prometheus targets и активный Alertmanager endpoint и только затем
    атомарно переключает `current`;
13. при ошибке до запуска миграций восстанавливает PostgreSQL, PgBouncer,
    monitoring и backend из предыдущего versioned compose/env/image bundle;
14. после запуска миграций запрещает автоматический запуск старого backend. Если
    новый backend уже прошёл health-check, он остаётся активным, а при сбое
    monitoring восстанавливаются только предыдущие monitoring-сервисы. Если
    новый backend не прошёл health-check, сервис остаётся fail-closed, а
    `.env.production.next` и `.release.next` сохраняются для обязательного
    roll-forward. На первом deploy действует то же правило: post-migration
    bundle сохраняется для продолжения, а не удаляется.

GitHub concurrency и серверный `flock` не допускают параллельные выкладки.
Образ всегда разворачивается по неизменяемому `ghcr.io/...@sha256:<digest>`, а
не по mutable tag. Токен GHCR после команды удаляется вместе с временным Docker
config. Во время локального `pg_dump` и миграции backend находится в коротком
maintenance window; размер S3-медиа больше не влияет на длительность deploy.

Именованный `media-data` volume временно сохраняется для совместимости старых
релизов, но production deploy его не архивирует. После S3 cutover в нём остаются
только legacy-файлы, которые не являются резервной копией S3 bucket.
Для bucket отдельно включите versioning/retention или резервное копирование у
провайдера и регулярно проверяйте восстановление объектов.

## Backup hook и восстановление

Локальный pre-migration PostgreSQL snapshot хранится в
`/opt/maxposty/backend/backups` с правами `0600`. Он создаётся и проверяется при
deploy, но публикация offsite-копии вынесена из критического пути релиза.

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

Короткоживущий `GITHUB_TOKEN` с `contents: write` доступен только отдельному
scheduled backup job. На VPS он записывается во временный curl config с правами
`0600`, не передаётся в argv и удаляется после операции. Hook проверяет имена,
права и manifest локальных файлов, создаёт потоковый CMS AES-256-GCM контейнер,
загружает его как единственный release asset и обязательно скачивает обратно.

### Ежедневная копия, автоматическая проверка и PITR

Workflow `Backend production backup` работает отдельно от deploy каждый день в
`01:17 UTC` (`04:17 Europe/Moscow`) и доступен для ручного запуска. Он всегда
исполняет скрипт из `/opt/maxposty/backend/current`, проверяет accepted release
и immutable image, делит с deployment общий GitHub concurrency group и VPS
`flock`, а короткоживущий `GITHUB_TOKEN` передаёт только через stdin. Поэтому
непринятый bundle не может сделать копию, а PostgreSQL не перезапустится в
середине проверки.

Каждый успешный запуск выполняет все проверки до публикации success-метрики:

1. создаёт PostgreSQL custom dump;
2. восстанавливает его в новую временную БД, выполняет контрольный запрос и
   удаляет временную БД;
3. формирует manifest и публикует зашифрованный immutable off-host asset тем же
   проверенным hook;
4. принудительно переключает WAL и ждёт подтверждения точного сегмента через
   `pg_stat_archiver`;
5. создаёт streaming `pg_basebackup`, проверяет `pg_verifybackup` и только затем
   делает каталог base backup видимым;
6. удаляет локальные logical/base/WAL копии старше настроенной retention и
   атомарно обновляет Prometheus textfile-метрики.

PostgreSQL запускается с `archive_mode=on`, `wal_level=replica` и архивирует WAL
в отдельный Docker volume. Base backups и WAL хранятся раздельно от основного
data volume и обеспечивают локальное point-in-time recovery в пределах
`PITR_RETENTION_DAYS`. Это не замена off-host копии: потеря всего VPS уничтожит
локальную PITR-цепочку, но ежедневный encrypted GitHub Release останется для
полного восстановления. Для более строгого RPO перенесите WAL archive в
отдельное versioned object storage и проверьте восстановление по целевому
timestamp.

После первого релиза с этой схемой сразу вручную запустите workflow `Backend
production backup`: до первой успешной проверки Prometheus будет корректно
считать backup/restore/PITR состояние просроченным. Ошибка любого шага оставляет
attempt metric в состоянии failed и отправляет операторский alert; success не
может быть записан по одному лишь `pg_restore --list`.

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
обязан проверить совместимость развёртывания. S3-cutover является сознательным
roll-forward-only исключением: после старта миграций предыдущая версия backend
не запускается автоматически.

Удаление колонок/таблиц, rename и прочая contract-фаза выполняются отдельной
ручной процедурой только после того, как новый код стабильно развернут, старые
релизы выведены из эксплуатации и offsite backup проверен. Уже применённые SQL
файлы никогда не редактируются. До миграции application rollback возвращает
versioned bundle. После миграции deployment сохраняет staged bundle для
roll-forward и намеренно не делает автоматический restore/down-migration БД,
чтобы не уничтожить записи, созданные после backup.

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

## Billing enforcement rollout

Production rendering always writes `BILLING_ENFORCEMENT_ENABLED`, defaulting
to `false`. The switch controls monthly AI plan limits only; minute/day safety
limits remain active, and usage telemetry is recorded in both modes. Keep the
switch disabled until the observed cost model has been reviewed. Internal paid
plans are not available for purchase, and channel/seat/storage entitlements
remain display-only until their write paths are integrated with the catalog.

## Yandex Direct rollout

The Yandex OAuth application and the connected advertiser account must have
approved access to the Yandex Direct API before this integration is enabled.
Register this exact production callback in the OAuth application:

`https://maxposty.ru/api/v1/advertising/direct/oauth/callback`

Store `DIRECT_OAUTH_CLIENT_ID`, `DIRECT_OAUTH_CLIENT_SECRET`, and
`DIRECT_TOKEN_DATA_KEY` as production Environment secrets. The token data key
must be a stable standard-base64 encoding of exactly 32 random bytes (for
example, generated once with `openssl rand -base64 32`); rotating or losing it
without a token re-encryption procedure makes existing connections unreadable.
Set these production Environment variables:

- `DIRECT_OAUTH_REDIRECT_URI=https://maxposty.ru/api/v1/advertising/direct/oauth/callback`
- `DIRECT_SANDBOX=true`
- `DIRECT_API_BASE_URL=https://api-sandbox.direct.yandex.com/json/v5`
- `DIRECT_WRITES_ENABLED=false`
- `DIRECT_AUTO_LAUNCH_ENABLED=false`

Use this staged rollout:

1. Deploy the fail-closed sandbox configuration above. Verify OAuth, advertiser
   identity, existing-campaign reads, and logs; campaign creation is
   intentionally unavailable while writes are disabled.
2. In a separate sandbox deployment set `DIRECT_WRITES_ENABLED=true`, keep
   auto-launch disabled, and verify campaign creation, manual launch, provider
   status polling, and ambiguous-launch recovery.
3. Switch together to `DIRECT_SANDBOX=false` and
   `DIRECT_API_BASE_URL=https://api.direct.yandex.com/json/v501`, initially
   keeping auto-launch disabled while the live account and manual workflow are
   verified.
4. Enable `DIRECT_AUTO_LAUNCH_ENABLED=true` only in another deliberate
   deployment after the recovery test has passed; auto-launch requires writes.

Do not point a production OAuth credential at an unapproved custom API origin.
Disabling write flags is an emergency kill switch for provider mutations;
read-only status reconciliation remains active so an ambiguous launch can
still be observed and recorded safely.

Auto-launch consent snapshots the provider campaign ID, campaign name, weekly
budget, dates, account, and local version, but it cannot snapshot every ad and
creative edited directly in Yandex Direct. The owner must review the actual
provider-side ads immediately before consent. Keep
`DIRECT_AUTO_LAUNCH_ENABLED=false` until creative-level verification is
implemented and separately approved.

Revoking the OAuth connection does not stop, pause, delete, or rebind campaigns
that already exist in Yandex Direct. The UI must require an explicit second
confirmation with that warning, and the operator should stop spend-capable
campaigns in Yandex before disconnecting. A later connection is stored as a new
account connection; historical campaigns retain their old connection and their
last confirmed provider status.

A launch in local `failed` state is still treated as potentially spend-capable:
workspace archival and Direct credential revoke/replacement remain blocked
without a time-based unlock. The current operator recovery procedure is to
retain the credential, perform repeated authoritative provider reads, stop the
campaign in Yandex if it is or may be running, and use an explicit audited
recovery action once quiescence is proven. That reset action is not part of
this rollout, so support must not delete the token or bypass the database
guard.

Provider token revocation/401 does not currently have a refresh-token flow or
token-expiry metadata: the current OAuth response handler stores only the access
token actually returned by Yandex. On an HTTP 401 or the documented Direct API
invalid-token error 53, the backend marks the connection `error` with the safe
code `authorization_required`, invalidates outstanding auto-launch consent, and
stops scheduling work for that connection.
The owner must use the explicit disconnect confirmation and complete OAuth
again. There is no background token refresh. Keep production writes and
auto-launch disabled until this reconnect path has been verified with a live
token expiry/revocation. Do not invent, synthesize, or persist a refresh token
or expiry that Yandex did not return. If an authorization error occurs while
reconciling an earlier ambiguous launch, the launch remains blocked for
operator recovery because the earlier provider write may still have succeeded.
Campaigns remain tied to the old connection and are not silently rebound after
OAuth; an unlaunched campaign must be recreated or handled directly in Yandex
after reconnecting.

This MVP supports direct advertiser accounts only; it does not implement
`AgencyClients` enumeration or client selection. Agency accounts, unknown
account types, non-chief representatives, and accounts without an explicit
`EDIT_CAMPAIGNS=YES` grant are connected read-only and must never enable
campaign writes or auto-launch. Add a separately reviewed agency-client
selection flow before claiming agency support.
