# Архитектура и путь запроса

## Три явные границы

В системе есть внешний protocol adapter, инфраструктурный Core и backend connectors. Adapter понимает клиентский контракт OpenAI Responses, извлекает минимальные metadata и формирует ошибки самого gateway. Core знает только execution envelope, политики доступа и состояние исполнения. Connector понимает upstream и при необходимости переводит его протокол.

Так разрешается важное противоречие: шлюз предоставляет OpenAI-compatible API, но его routing, limits и runtime не зависят от OpenAI JSON. Northbound adapter — отдельный пакет, а не набор условных веток внутри Core.

```text
HTTP client
    │
    ▼
Northbound adapter ── original body + metadata ──► Core executor
    ▲                                                 │
    │ status / headers / bytes                        │ attempt
    │                                                 ▼
    └────────────────────────────────────────── Connector runtime
                                                      │
                                                      ▼
                                                   Backend
```

## Предлагаемая структура проекта

```text
cmd/gateway/                 composition root и запуск
internal/northbound/openai/  Responses endpoint и ошибки внешнего API
internal/core/               request lifecycle и orchestration
internal/routing/            маршруты, eligibility и account selection
internal/limits/             admission и reconciliation
internal/auth/               virtual keys и общий auth runtime
internal/storage/sqlite/     repositories и migrations
internal/runtime/            registry и исполнение connectors
internal/config/             загрузка и валидация конфигурации
internal/observability/      логи и метрики
connector/                  общий контракт и runtime services
connectors/                 backend implementations
conformance/                общий набор проверок
testdata/                   fixtures без secrets
docs/                       проектная документация
```

Папки появляются по мере реализации. Core и routing не импортируют `connectors/*` или `internal/northbound/openai`. Concrete implementations связываются только в composition root. Общие helpers для OpenAI JSON и SSE допустимы в connector-слое, но не становятся универсальной моделью LLM.

## Lifecycle запроса

Northbound adapter проверяет virtual key, ограничивает размер тела и извлекает `model`, `stream` и явно распознаваемые требования. Исходные bytes сохраняются. Неизвестные поля остаются в payload; adapter не пытается полностью описать Responses API собственной схемой.

Core применяет ограничения ключа и находит подходящие route targets по protocol, model, capabilities и состоянию accounts. Connector оценивает usage для выбранного target; limits атомарно резервируют доступный бюджет. Создаётся attempt, после чего runtime вызывает `Execute`.

До отправки response headers connector сообщает status и безопасный набор headers. Затем Core передаёт chunks с backpressure. При завершении usage и outcome записываются, а reservation корректируется. Client disconnect отменяет upstream, закрывает stream и завершает attempt ровно один раз.

## Native passthrough и model mapping

В native mode тело проходит побайтно неизменным. Допустимы замена gateway Authorization на upstream credential, удаление hop-by-hop headers и пересчёт transport headers. Обещание passthrough не означает передачу всех входящих headers провайдеру.

Model mapping имеет два разных смысла. **Выбор маршрута** по исходному имени модели не меняет payload и совместим с passthrough. **Замена имени модели** в JSON уже является преобразованием; её выполняет connector в явно включённом режиме translation/rewrite. Конфигурация native mode с несовпадающим upstream model отклоняется при старте.

Общий `openai-compatible` connector не может считать, что наличие Chat Completions означает поддержку Responses. Его instance явно объявляет upstream protocol. Responses-native backend допускает passthrough; Chat-only backend требует отдельного translator внутри connector и собственной матрицы поддерживаемых возможностей.

## Streaming и backpressure

После первого response head status и headers считаются committed. Core не накапливает полный ответ и не переставляет chunks. Очереди bounded; медленный клиент замедляет чтение upstream вместо неограниченного роста памяти. Completion control message содержит outcome и usage отдельно от пользовательских bytes.

Ошибка до commit может быть превращена в HTTP error. После commit нельзя заменить ответ новым JSON с другим status. Connector может выдать корректное для внешнего протокола terminal error event; при повреждённом потоке или падении процесса transport закрывается, а attempt фиксируется как incomplete. Core не синтезирует provider-specific SSE events.

## Fallback и повторные попытки

Fallback допустим только до commit и при подтверждённой возможности безопасного повтора. Ошибка соединения до отправки запроса, локальная недоступность connector и явно классифицированный отказ upstream — разные случаи. Потеря соединения после отправки тела имеет неизвестный результат и по умолчанию не повторяется автоматически, даже если клиент ещё не получил bytes.

Connector сообщает категорию ошибки и retry disposition: `safe`, `unsafe` или `unknown`. Core применяет policy с ограничением числа attempts и общим deadline. Провайдерские error codes и `Retry-After` интерпретирует connector. После commit fallback запрещён: склейка ответов разных попыток нарушает tool IDs, порядок событий и usage.

## Stateful Responses

`previous_response_id`, session resume, stored responses и background execution не становятся автоматически переносимыми между бэкендами. В первом native slice они могут проходить к тому же upstream как opaque fields, но это не обещание полной реализации Responses resource API.

Для session-bound запросов нужна affinity к исходным connector instance, account и backend. До реализации affinity конфигурация использует один target для таких сценариев, а cross-account fallback отключён. Translation connector явно отклоняет неподдерживаемые stateful features. Retrieval, deletion, cancellation по response ID и background execution получают отдельный scope после базового POST и streaming.

## Изоляция

До M6 встроенные first-party connectors исполняются в процессе gateway за тем же контрактом. Это упрощает проверку архитектуры, но не обеспечивает process isolation. Внешние сторонние connectors подключаются после появления process runtime.

Process isolation защищает Core от падения connector. Ограничение CPU, памяти, filesystem и network access — отдельные возможности sandbox; один IPC transport не даёт таких гарантий. Runtime должен хотя бы ограничивать размеры messages, очереди и время завершения process.
