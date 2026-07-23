# Активация YooKassa в MaxPosty

Статус на 22 июля 2026 года: код биллинга можно выпускать, но реальные платежи и продления должны оставаться выключенными до завершения двух внешних согласований.

## Обязательные условия

1. Менеджер YooKassa подключил автоплатежи по банковским картам для production ShopID. По умолчанию автоплатежи доступны только тестовому магазину.
2. YooKassa приняла полноэкранные скриншоты сценария самостоятельной отвязки карты на `https://maxposty.ru` с полностью видимой адресной строкой.
3. Для владельца магазина на НПД документирован и проверен действующий процесс регистрации каждого первого и повторного дохода в «Мой налог» и доставки чека покупателю.

Пункт 3 нельзя считать выполненным по старой настройке «чеки для самозанятых»: в истории изменений API YooKassa указано, что поддержка этого сервиса прекращена с 29 декабря 2025 года. До письменного подтверждения нового процесса от YooKassa и налогового специалиста production-списания не включаются.

Официальные источники:

- https://yookassa.ru/developers/payment-acceptance/scenario-extensions/recurring-payments/basics
- https://yookassa.ru/developers/using-api/changelog

## Защитные настройки

Секреты хранятся только в GitHub Environment `production`:

- `YOOKASSA_SHOP_ID`
- `YOOKASSA_SECRET_KEY`
- `YOOKASSA_DATA_KEY` — 32 случайных байта в standard Base64

До выполнения обязательных условий переменные должны иметь такие значения:

```text
BILLING_LIVE_ENABLED=false
YOOKASSA_RECEIPTS_CONFIRMED=false
BILLING_ENFORCEMENT_ENABLED=false
```

Наличие production-ключа само по себе не разрешает checkout, повторные списания или worker продлений. Приложение обязано запускаться fail-closed и разрешать реальные платежи только при одновременном выполнении всех условий:

```text
BILLING_LIVE_ENABLED=true
YOOKASSA_RECEIPTS_CONFIRMED=true
BILLING_ENFORCEMENT_ENABLED=true
```

## Порядок включения

1. Получить письменное подтверждение YooKassa по автоплатежам и сохранить его в операционном журнале.
2. Зафиксировать выбранный процесс чеков для НПД и проверить его на первой и повторной оплате.
3. Настроить HTTP-уведомления YooKassa на `https://maxposty.ru/api/v1/webhooks/yookassa` для событий, поддерживаемых текущей версией приложения.
4. Включить три production-переменные одним изменением и запустить обычный защищённый deploy.
5. С отдельного согласия владельца выполнить реальную минимальную проверку: первая оплата, сохранение способа, повторное списание, отмена, отвязка карты и чек.
6. Убедиться, что повторная доставка одного webhook не меняет баланс или период второй раз.
7. Убедиться, что пользователь перед checkout повторно принял версии `terms` и `personal_data` от `2026-07-23`. Backend проверяет обе записи `user_consents` и возвращает `billing_legal_consent_required`, поэтому старой сессии потребуется повторный вход.

## Согласие и повторяемость запроса

`GET /api/v1/plans` возвращает для каждого платного тарифа точные поля
`recurring_consent_text`, `recurring_consent_version`,
`recurring_consent_terms_version` и `recurring_consent_terms_url`. Клиент обязан
показать текст дословно и вернуть при checkout весь показанный snapshot:
`plan_code`, `plan_version`, `monthly_price_minor`, `currency_code`,
`recurring_consent_version` и `recurring_consent_terms_version`. Несовпадение
возвращает `billing_checkout_snapshot_mismatch` до внешнего POST. В журнал
согласий записываются ровно показанный текст, версия, ссылка, цена, валюта и
версия тарифа.

Тело внешнего POST фиксируется на попытке до первого вызова YooKassa:
описание, return URL и зашифрованная ссылка на сохранённый способ оплаты больше
не читаются из изменяемой конфигурации или контракта при retry. Повторный POST
допустим только worker-у с тем же idempotence key и этим неизменным snapshot.

## Runbook `manual_review`

Алерт `MaxPostyBillingManualReview` срабатывает при первой такой попытке.

1. Немедленно установить `BILLING_LIVE_ENABLED=false` и развернуть конфигурацию. Это запрещает новые checkout и списания, но оставляет canonical GET для сверки.
2. Найти попытку по `billing_payment_attempts.id` или `provider_payment_id`. Не копировать `payment_method_snapshot` в тикеты или чаты.
3. Через кабинет YooKassa или авторизованный `GET /v3/payments/{payment_id}` проверить ID, финальный статус, `test=false`, сумму, валюту и metadata `attempt_id`/`workspace_id`. Webhook сам по себе доказательством не является.
4. Для `provider_*_outcome_unknown`, `canceled_with_provider_outcome_unknown` и `provider_pending_horizon_exceeded` не выполнять новый POST. Если payment ID неизвестен, эскалировать в поддержку YooKassa с idempotence key.
5. Для `stale_renewal_paid_manual_refund` оформить полный возврат в YooKassa: этот платёж намеренно не меняет текущий тариф или период.
6. Любое закрытие `manual_review`, активация периода или фиксация возврата выполняется только проверенной одноразовой миграцией в incident PR с приложенным canonical-ответом и ID возврата. Поддерживаемого admin endpoint/CLI пока нет; прямое ad-hoc изменение production-БД запрещено.
7. После сверки проверить отсутствие других `manual_review`, дождаться исчезновения алерта и только затем рассматривать повторное включение live-флагов.

Provider `pending` переводится в `manual_review` через 7 суток; бесконечного polling нет. Worker обрабатывает не более четырёх внешних запросов за цикл, чтобы 15-секундный timeout YooKassa не задерживал общий scheduler дольше минуты.

## Экстренное отключение

При любом расхождении сначала установить `BILLING_LIVE_ENABLED=false` и повторно развернуть текущий commit. Это блокирует новый checkout и worker списаний; уже полученные уведомления сохраняются для сверки, но не должны инициировать новый платёж.
