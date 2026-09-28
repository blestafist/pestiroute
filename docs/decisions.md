# Решения и открытые вопросы

## Статусы

**Принято** означает ограничение из PLAN или прямое уточнение его границ. **Предложено** — рабочий выбор для первой реализации, который нужно подтвердить кодом. **Открыто** — вопрос с конкретным этапом принятия решения. Изменение принятого ограничения требует обновления PLAN и связанных документов.

## Реестр решений

| ID | Статус | Решение | Причина |
| --- | --- | --- | --- |
| D01 | Принято | OpenAI Responses — основной внешний контракт | Agent workflows без собственного LLM-языка |
| D02 | Принято | Core не содержит backend-specific code | Расширение через connectors |
| D03 | Принято | Payload opaque; native body сохраняется побайтно | Совместимость с unknown fields и extensions |
| D04 | Принято | Northbound adapter отделён от Core | API parsing не проникает в routing и limits |
| D05 | Принято | Translation и tokenizer принадлежат connector | Различия API не нормализуются внутри ядра |
| D06 | Принято | Third-party connectors исполняются вне процесса | Их падение не завершает gateway |
| D07 | Предложено | Go, `net/http`, один module | Минимальная инфраструктура для M0–M3 |
| D08 | Предложено | SQLite для single-node state | Self-hosted запуск без отдельной БД |
| D09 | Предложено | `Head / Body / Complete` как stream contract | Transport semantics без LLM event model |
| D10 | Предложено | gRPC over Unix socket для IPC | Проверить стоимость и host-service lifecycle в M6 |
| D11 | Принято | Model rewrite не является native passthrough | Изменение body должно быть явным |
| D12 | Предложено | CLI для первых admin operations | Управление accounts/keys без UI milestone |

## Вопросы перед реализацией

| Вопрос | Когда решить | Как проверить |
| --- | --- | --- |
| Какой upstream и клиент дают первый baseline? | M0 | Выбрать Responses-native endpoint и воспроизводимый tool scenario |
| Где находятся нужные исходники 9Router? | До миграции | Зафиксировать repository, commit, лицензию и список adapters |
| Какие Responses features входят в первую публичную compatibility claim? | M1 | Опубликовать endpoint/feature matrix, включая unsupported stateful operations |
| Как публичные model IDs соотносятся с upstream IDs? | M2 | Native identity mapping; rewrite только как явный connector mode |
| Как получать requirements без полного разбора payload? | M2 | Route policy плюс минимальный northbound extractor; unknown fields сохраняются |
| Как выглядит output budget при неизвестном usage? | M3 | Выбрать rejection/conservative policy и проверить concurrency |
| Как сохраняется account affinity для response IDs? | До multi-account stateful routing | Проверить scope IDs и lifetime без переписывания native response |
| Какие возможности reasoning можно перенести без потери семантики? | M4 | Реальные fixtures и явные negative cases |
| Нужен ли Responses → Chat translator в первом локальном connector? | M5 | Проверить capabilities конкретных версий Ollama/vLLM |
| Что именно исполняет ACP connector? | Перед ACP implementation | Выбрать agent process, transport и владельца tool loop |
| Как external connector вызывает host runtime services? | M6 | Прототип scoped bidirectional lifecycle и cancellation |

## Формат будущих ADR

Для решения с несколькими серьёзными вариантами создаётся короткий ADR: контекст, выбранный вариант, причины, последствия и критерий пересмотра. Не нужно оформлять отдельный ADR на каждую библиотеку; важнее фиксировать решения, которые меняют границы пакетов, гарантии совместимости или формат persistent data.

До появления таких решений этот реестр остаётся единой точкой входа. Он помогает отличать обещанные свойства системы от гипотез, которые ещё предстоит проверить.
