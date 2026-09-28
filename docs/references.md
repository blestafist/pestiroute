# Референсы и порядок исследования

## Как использовать источники

Ниже — исходные точки исследования, а не обещание поддержки всех описанных возможностей. При реализации connector следует фиксировать дату, API/client version или commit источника и превращать нужное поведение в fixtures. Документация API, код официального клиента и наблюдаемый trace отвечают на разные вопросы; один источник не заменяет остальные.

## Протоколы и официальные реализации

| Источник | Что изучать |
| --- | --- |
| [OpenAI Responses API](https://platform.openai.com/docs/api-reference/responses) | Request/response contract, tool items, usage, stateful operations |
| [OpenAI streaming events](https://platform.openai.com/docs/api-reference/responses-streaming) | Event lifecycle, deltas, completion и error events |
| [OpenAI Codex](https://github.com/openai/codex) | Клиентский transport, auth и точный scope совместимости выбранной версии |
| [Anthropic API documentation](https://docs.anthropic.com/) | Messages, streaming, tools, token counting и usage |
| [Claude Code](https://github.com/anthropics/claude-code) | Публичные материалы, releases и issues; полноту исходников проверять отдельно |
| [Gemini API](https://ai.google.dev/gemini-api/docs) | Официальный API, multimodal inputs, tools и streaming |
| [Gemini CLI](https://github.com/google-gemini/gemini-cli) | Реальный клиентский протокол и auth flows конкретной версии |
| [Ollama documentation](https://docs.ollama.com/) | Native API и фактический scope OpenAI compatibility |
| [vLLM documentation](https://docs.vllm.ai/) | Serving endpoints, supported fields и ограничения моделей |
| [llama.cpp](https://github.com/ggml-org/llama.cpp) | Server implementation и поддерживаемые compatibility endpoints |
| [Agent Client Protocol](https://agentclientprotocol.com/) | Agent-process lifecycle и отличие от provider API |

OpenAI-compatible не означает полную поддержку Responses. Для каждого upstream отдельно проверяются endpoint, streaming, tools, reasoning и session semantics. Аналогично официальный public API не обязательно совпадает с backend protocol подписочного клиента.

## Инфраструктура

| Источник | Применение |
| --- | --- |
| [Go net/http](https://pkg.go.dev/net/http) | Streaming, transports, cancellation и server lifecycle |
| [Go context](https://pkg.go.dev/context) | Передача deadline и отмены между слоями |
| [SQLite WAL](https://www.sqlite.org/wal.html) | Конкурентный доступ и operational особенности storage |
| [SQLite Online Backup](https://www.sqlite.org/backup.html) | Согласованные backups активной БД |
| [gRPC flow control](https://grpc.io/docs/guides/flow-control/) | Backpressure для external runtime |
| [Protocol Buffers](https://protobuf.dev/programming-guides/) | Эволюция control contract без изменения opaque payload |
| [OAuth 2.0, RFC 6749](https://www.rfc-editor.org/rfc/rfc6749) | Общие роли и lifecycle authorization flows |
| [PKCE, RFC 7636](https://www.rfc-editor.org/rfc/rfc7636) | Общая часть auth runtime |
| [Device Authorization, RFC 8628](https://www.rfc-editor.org/rfc/rfc8628) | Основа device flows там, где они используются backend |
| [SSE specification](https://html.spec.whatwg.org/multipage/server-sent-events.html) | Framing, multiline data и обработка event stream |

## Миграция из 9Router

Точный репозиторий и commit 9Router пока не зафиксированы. До начала переноса нужно добавить ссылку, revision, лицензию и карту нужных файлов. Название проекта само по себе недостаточно для выбора исходников.

Для каждого adapter исследуются auth flow, сборка request, stream parser, usage extraction, обработка model IDs и известные regression tests. Сначала формируется минимальный trace и список особенностей, затем код адаптируется к новому контракту. Старые routing policies и внутренние LLM abstractions не переносятся вместе с полезными protocol helpers.

## Шаблон исследования connector

Короткая заметка должна содержать backend/client version, источник протокола, способы auth, поддерживаемые northbound features, native/translation mode, retry semantics и statefulness. Завершается она минимальным воспроизводимым request/response fixture и списком ещё неизвестных свойств. Это достаточное основание для оценки implementation milestone.
