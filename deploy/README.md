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
`DEPLOY_STAGE=production`. Тогда валидатор потребует реальные интеграционные
secrets и зафиксирует публичные адреса:

- Яндекс OAuth callback:
  `https://maxposty.ru/api/v1/auth/yandex/callback`;
- webhook общего бота MAX:
  `https://maxposty.ru/api/v1/webhooks/max`;
- frontend origin и canonical API origin: `https://maxposty.ru`.

## Одноразовая подготовка VPS

На сервере нужны Docker Engine, Docker Compose v2, `bash`, `tar`, `flock`,
`readlink` и `sha256sum`, а также отдельный SSH-пользователь
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

Создайте оба Environment, включите required reviewers и запретите не-main
ветки. В Environment `bootstrap` добавьте только:

- `VPS_SSH_PRIVATE_KEY` — отдельный приватный deploy-ключ;
- `VPS_SSH_KNOWN_HOSTS` — заранее проверенная строка host key для VPS;
- `POSTGRES_OWNER_PASSWORD` и `POSTGRES_APP_PASSWORD` — разные значения из
  `openssl rand -hex 32`.

В `bootstrap` не должно быть Yandex, MAX или OpenAI secrets. В Environment
`production` добавьте те же SSH/DB secrets с теми же значениями и дополнительно:

- `YANDEX_CLIENT_ID` и `YANDEX_CLIENT_SECRET`;
- `MAX_BOT_TOKEN` — новый токен одного общего бота;
- `MAX_WEBHOOK_SECRET` — значение из `openssl rand -hex 32`;
- `OPENAI_API_KEY`.

Добавьте repository variables:

- `VPS_HOST=178.159.94.83` (это значение уже используется как безопасный
  fallback);
- `VPS_PORT=22`;
- `VPS_USER=maxposty-deploy`;
- `DEPLOY_STAGE=bootstrap` до готовности DNS/HTTPS, затем `production`;
- опционально `BACKUP_RETENTION_DAYS`.

`YANDEX_ALLOWED_USERS` и `MAX_CA_CERT_FILE`, если они нужны, добавляйте только
как variables Environment `production`. Workflow передаёт production
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

`Backend production deploy` автоматически стартует только после успешного
`Backend Security` для доверенного push в `main`. GitHub трактует список
`workflow_run.workflows` как OR, поэтому отдельный release gate через Actions
API проверяет для того же commit SHA успешные push-запуски и `Backend CI`, и
`Backend Security`. Ручной запуск на `main` проходит ту же проверку. Workflow:

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
11. в production требует успешный offsite backup hook;
12. применяет только additive-миграции отдельной owner-ролью;
13. запускает новый backend по проверенному digest и после health-check атомарно
    переключает `current`;
14. при ошибке восстанавливает PostgreSQL, PgBouncer и backend из предыдущего
    versioned compose/env/image bundle. При неудаче первого deploy удаляет
    непринятые контейнеры, сохраняя data volumes. Схема БД не откатывается вниз
    и обязана быть совместимой назад.

GitHub concurrency и серверный `flock` не допускают параллельные выкладки.
Образ всегда разворачивается по неизменяемому `ghcr.io/...@sha256:<digest>`, а
не по mutable tag. Токен GHCR после команды удаляется вместе с временным Docker
config. Во время согласованного backup, offsite hook и миграции backend находится
в maintenance window; длительность в основном зависит от размера media и hook.

## Backup hook и восстановление

Локальные pre-migration DB/media snapshots и checksum manifest хранятся в
`/opt/maxposty/backend/backups` с правами `0600`. В bootstrap отсутствие offsite
hook даёт предупреждение. Production deploy fail-closed требует исполняемый файл
`/opt/maxposty/backend/hooks/after-backup` и не запускает миграцию, если файла нет
или он вернул ненулевой код.

Hook не поставляется проектом и не привязан к облачному провайдеру. Он вызывается
от пользователя deploy с четырьмя позиционными аргументами:

1. абсолютный путь к PostgreSQL custom dump;
2. абсолютный путь к `media.tar.gz`;
3. абсолютный путь к SHA-256 manifest;
4. immutable GHCR digest готовящегося релиза.

Hook обязан синхронно скопировать все три файла в зашифрованное независимое
хранилище, проверить результат и только после этого вернуть `0`. Сам скрипт и
его конфигурация должны устанавливаться оператором вне git; не передавайте через
него токены в аргументах командной строки. Локальная retention не заменяет
offsite backup.

Restore drill выполняйте только в изолированном окружении: получите из offsite
один согласованный комплект из трёх файлов, запустите `sha256sum -c` из каталога
с dump/media, проверьте dump через `pg_restore --list`, а media через
`tar -tzf`. Затем создайте чистую PostgreSQL 18 с owner/app-ролями, восстановите
custom dump, распакуйте media в пустой volume и запустите сохранённый immutable
digest либо заведомо совместимый релиз. До признания drill успешным проверьте
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
`DEPLOY_STAGE=production`, вручную запустите workflow для `main` и включите input
`configure_max_webhook`. Операторская команда сначала
проверит публичный endpoint без redirect и только затем обновит подписку общего
бота на события `bot_started`, `message_callback`, `bot_added`, `bot_removed`.

Обычный автоматический deploy не перенастраивает внешнюю подписку MAX.
