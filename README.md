# Backend MaxPosty

Это сервер MaxPosty. Он хранит пользователей, каналы и публикации, выполняет
исследования и генерацию изображений, запускает календарь публикаций и работает
с одним общим ботом MAX. Токены MAX и OpenAI остаются только на сервере.

Фронтенд находится в отдельном репозитории. Для обычного локального запуска
backend не требует установленного Go: достаточно Docker Engine с Compose.
Команды `make compose-*` автоматически поддерживают как современный
`docker compose`, так и старое имя `docker-compose`.

## Быстрый запуск

1. Создайте файл настроек:

   ```sh
   cp .env.example .env
   ```

2. Сгенерируйте два разных пароля PostgreSQL:

   ```sh
   openssl rand -hex 32
   openssl rand -hex 32
   ```

   Запишите их в `POSTGRES_OWNER_PASSWORD` и `POSTGRES_APP_PASSWORD`. Используйте
   только сгенерированные URL-safe значения: Compose собирает из них адреса
   подключения.

3. Создайте web-приложение в Яндекс OAuth и заполните:

   ```dotenv
   YANDEX_CLIENT_ID=...
   YANDEX_CLIENT_SECRET=...
   YANDEX_REDIRECT_URI=http://localhost:8080/api/v1/auth/yandex/callback
   ```

   Redirect URI должен в точности совпадать с адресом, указанным в настройках
   приложения Яндекса. Авторизация через Яндекс ID обязательна: режима без входа
   и пользовательского ключа администратора нет.

   Для закрытой беты можно задать `YANDEX_ALLOWED_USERS` — список точных ID,
   логинов или email через запятую. Пустое значение разрешает вход любому
   пользователю, который успешно прошёл Яндекс OAuth.

4. Добавьте новый токен общего бота MAX и секрет webhook. Username сервер
   получает через MAX API автоматически. Если токен когда-либо отправлялся в
   чат, задачу или issue, сначала перевыпустите его в MAX:

   ```dotenv
   MAX_BOT_TOKEN=...
   MAX_WEBHOOK_SECRET=...
   MAX_WEBHOOK_URL=https://maxposty.ru/api/v1/webhooks/max
   ```

   Секрет webhook удобно создать командой `openssl rand -hex 32`.

5. Запустите сервисы:

   ```sh
   make compose-up
   make compose-logs
   ```

API откроется на [http://localhost:8080/api/v1/health](http://localhost:8080/api/v1/health).
Одноразовый контейнер `migrate` сначала применит миграции напрямую к PostgreSQL,
и только после его успешного завершения запустится backend.

Остановить сервисы:

```sh
make compose-down
```

Команда сохраняет базу и изображения. Полное удаление локальных данных требует
явного подтверждения действием `docker compose down -v` (или
`docker-compose down -v` в старой установке).

## Как устроена база данных

Локальный Compose использует:

- PostgreSQL `18.4` для постоянного хранения;
- PgBouncer `1.25.2` в режиме `transaction` для запросов приложения;
- отдельную роль владельца для миграций;
- отдельную runtime-роль без прав DDL для backend.

Порты PostgreSQL `5432` и PgBouncer `6432` не публикуются на компьютере. Они
доступны только контейнерам во внутренних Docker-сетях. Backend видит PgBouncer,
но не подключён к сети PostgreSQL и не может обойти пулер. Наружу публикуется
только HTTP-порт backend.

`DATABASE_URL` ведёт через PgBouncer и доступен только backend. Секретный
`DIRECT_DATABASE_URL` ведёт прямо в PostgreSQL и передаётся только контейнеру
миграций. Не добавляйте owner URL в окружение основного приложения.

PgBouncer использует SCRAM-SHA-256 в обе стороны. В локальном Compose трафик
идёт по закрытой Docker-сети с `sslmode=disable`. В production используйте TLS с
проверкой сертификата (`verify-full`) между приложением, пулером и внешней БД.

Режим transaction pooling не сохраняет состояние сессии. Код приложения не
должен полагаться на session-level `SET`, временные таблицы, `LISTEN/NOTIFY` или
session advisory locks. Миграции используют direct-соединение.

Именованный volume PostgreSQL смонтирован в `/var/lib/postgresql`, как требует
официальный образ PostgreSQL 18. Именованный volume — не резервная копия:
настройте регулярный `pg_dump` или резервные копии провайдера до production.

## Обязательный вход через Яндекс

Публичными остаются только технические маршруты входа, минимальная проверка
состояния и webhook MAX. Все кабинеты, каналы, черновики, изображения и действия
публикации требуют серверную сессию Яндекс ID.

В production используйте один публичный HTTPS-домен: frontend обслуживает сайт
и проксирует `/api/v1` и `/media` на backend. Укажите этот origin в
`FRONTEND_ORIGIN`, `PUBLIC_BASE_URL` и в зарегистрированном callback Яндекса:

```dotenv
FRONTEND_ORIGIN=https://maxposty.ru
PUBLIC_BASE_URL=https://maxposty.ru
YANDEX_REDIRECT_URI=https://maxposty.ru/api/v1/auth/yandex/callback
```

Не включайте `OAUTH_TRUST_X_REAL_IP`, пока прямой доступ к Go-порту не закрыт, а
доверенный reverse proxy не перезаписывает `X-Real-IP` сам.

## Общий бот MAX и подтверждение канала

Пользователям не нужно создавать собственных ботов. Сначала они добавляют
общего бота администратором канала, затем запускают в кабинете персональное
одноразовое подтверждение и подтверждают свой MAX-профиль. Сервер связывает
событие `bot_started` только с авторизованным кабинетом и затем проверяет
владельца канала и права бота через MAX API.

Для production укажите публичный HTTPS webhook на порту 443 без номера порта:

```text
POST https://maxposty.ru/api/v1/webhooks/max
```

Настройте подписку общего бота один раз от имени оператора:

```sh
make setup-max-webhook
# либо тем же собранным образом:
docker compose --profile ops run --rm --build setup-max-webhook
```

Команда создаёт или обновляет подписку через `platform-api2.max.ru`, передаёт
значение `MAX_WEBHOOK_SECRET` в поле `secret` и подписывается на события:

```text
bot_started, message_callback, bot_added, bot_removed
```

MAX отправляет секрет в `X-Max-Bot-Api-Secret`. Webhook без правильного секрета
отклоняется. Токен общего бота нельзя передавать браузеру или пользователям.

Перед успешным завершением команда сама отправляет безопасное контрольное
событие на публичный endpoint и требует точный HTTP `200` без redirect. До
запуска проверьте, что домен доступен по HTTPS на неявном порту 443, сертификат
выдан доверенным центром, его CN/SAN совпадает с доменом и сервер отдаёт полную
цепочку. MAX ждёт ответ не дольше 30 секунд; если ни одна доставка не была
успешной в течение 8 часов, подписка автоматически отключается. Поэтому в
production проверяйте `GET /subscriptions` и настройте оповещение.

API MAX работает только через `https://platform-api2.max.ru`. До 19 июля 2026
проверьте, что trust store окружения доверяет новой цепочке MAX. Если системных
корней недостаточно, укажите проверенный официальный PEM Минцифры в
`MAX_CA_CERT_FILE`; он добавится к системным корням, а TLS-проверка останется
включённой. Не заменяйте весь trust store случайным файлом: берите PEM только из
официального источника и сверяйте отпечаток перед размещением в `./certs`.

## Защита от лишних расходов на ИИ

У всех кабинетов один серверный OpenAI-аккаунт, поэтому backend ограничивает
генерацию до обращения к OpenAI. По умолчанию один пользователь может выполнять
только один AI-запрос одновременно, создать до 2 изображений и до 2 исследований
в минуту, а дневной предел каждой операции — 20. Оба способа создать картинку —
отдельная генерация и картинка для поста — расходуют один общий image-лимит.

Лимиты хранятся в PostgreSQL отдельно для каждого пользователя и операции, поэтому
перезапуск backend или несколько его реплик не обнуляют счётчики. Короткий
transaction-scoped advisory lock безопасен с PgBouncer в режиме `transaction`.
Активный запрос держит lease; backend удаляет его после ответа, а после аварии он
истекает автоматически. Общий неблокирующий предел не позволяет очереди дорогих
запросов занять все ресурсы процесса. При превышении API отвечает `429` и передаёт
`Retry-After`; запрос к OpenAI при этом не выполняется.

Безопасные значения можно уменьшить или осознанно увеличить в `.env`:

```dotenv
AI_GLOBAL_MAX_CONCURRENT=4
AI_USER_MAX_CONCURRENT=1
AI_IMAGE_PER_MINUTE=2
AI_IMAGE_PER_DAY=20
AI_RESEARCH_PER_MINUTE=2
AI_RESEARCH_PER_DAY=20
AI_LEASE_TTL=4m
```

`AI_LEASE_TTL` должен быть больше таймаута AI-обработчика `3m`. Нулевые,
отрицательные и чрезмерные значения отклоняются при запуске.

`OPENAI_API_KEY` не обязателен для запуска production. Пока ключ не добавлен,
Яндекс-вход, кабинеты и MAX продолжают работать, health endpoint показывает
`openai_configured=false` и `research_configured=false`, а маршруты генерации
изображений и исследования отвечают `503` с явным кодом `*_not_configured`.

## Разработка без Docker для backend

Нужны Go 1.25.12+ и доступный PostgreSQL. Укажите адреса вручную:

```dotenv
DATABASE_URL=postgresql://maxstudio_app:...@localhost:6432/maxstudio?sslmode=disable
DIRECT_DATABASE_URL=postgresql://maxstudio_owner:...@localhost:5432/maxstudio?sslmode=disable
TEST_DATABASE_URL=postgresql://maxstudio_test:...@localhost:5432/maxstudio_test?sslmode=disable
```

Затем доступны команды:

```sh
make migrate      # применить миграции через direct URL
make dev          # запустить API через PgBouncer
make setup-max-webhook # один раз настроить production webhook общего бота
make test         # тесты на PostgreSQL
make test-race    # тесты с race detector
make vet
make lint-install
make lint
make build        # server + migration + операторская настройка webhook
make docker-build
make compose-config
```

Тесты намеренно не переходят на SQLite и не пропускаются без базы. Каждый тест
создаёт изолированную PostgreSQL-схему через `TEST_DATABASE_URL`. GitHub Actions
запускает PostgreSQL 18.4, миграции, все race-тесты, `go vet`, сборку
исполняемых файлов и golangci-lint.

Production-деплой на один VPS, GHCR, PostgreSQL/PgBouncer, временный
fail-closed запуск через `http://178.159.94.83` и последующее переключение на
`https://maxposty.ru` описаны в [`deploy/README.md`](deploy/README.md). В
bootstrap-режиме провайдер входа отключён, но авторизация не обходится: все
приватные маршруты продолжают отвечать `401`.

## Секреты и production

- Не коммитьте `.env` и дампы базы.
- Храните DB, Yandex, MAX и, если он подключён, OpenAI credentials в secret
  manager платформы.
- Используйте разные owner/runtime-пароли и регулярно их меняйте.
- Не публикуйте `5432` и `6432` в интернет.
- Ограничьте доступ к административной консоли PgBouncer; в этой конфигурации
  admin users намеренно не назначены.
- Настройте TLS, резервные копии, мониторинг миграций и оповещение об отписке
  webhook до публичного запуска.
