# Контракт connector

## Назначение

Контракт описывает исполнение opaque protocol request. Он не содержит общих `Message`, `Tool`, `Reasoning` или провайдерских методов. Приведённые Go signatures — проект интерфейса, который уточняется в M2 на работающем native connector и fake backend.

```go
type Connector interface {
    Describe(ctx context.Context) (Descriptor, error)
    Authenticate(ctx context.Context, req AuthRequest, rt Runtime) (AuthResult, error)
    EstimateUsage(ctx context.Context, req Request, rt Runtime) (UsageEstimate, error)
    Execute(ctx context.Context, req Request, rt Runtime) (Stream, error)
    Models(ctx context.Context, rt Runtime) ([]Model, error)
    Health(ctx context.Context, rt Runtime) (HealthStatus, error)
}

type Request struct {
    Protocol string
    Headers  map[string][]string
    Body     []byte
    Metadata Metadata
}

type Metadata struct {
    RequestID    string
    AttemptID    string
    Model        string
    Streaming    bool
    Requirements []Capability
}

type Stream interface {
    Next(ctx context.Context) (Frame, error)
    Close() error
}
```

`Execute` выполняет одну попытку на уже выбранном account. Connector не делает скрытый fallback между аккаунтами. Отмена context распространяется на HTTP requests и runtime calls; `Close` идемпотентен и освобождает ресурсы даже при частично прочитанном ответе. Параллельные `Execute` должны быть безопасны; один stream читает один consumer.

## Descriptor и Models

Descriptor содержит stable ID, implementation version, contract version, тип connector, принимаемые protocols и способы auth. Версия реализации и версия IPC/SDK — разные значения. `Models` возвращает доступность моделей для настроенного instance/account и их capabilities, а не универсальное описание внутреннего устройства моделей.

Capabilities имеют три значения: `supported`, `unsupported`, `unknown`. Требование запроса удовлетворяется только первым. Итоговая поддержка определяется сочетанием protocol, execution mode, connector, model и account; широкое заявление connector не должно перекрывать ограничение конкретной модели.

```yaml
id: openai-compatible
type: api
version: "0.1.0"
contract_version: "1"
protocols:
  accepts: [openai.responses/v1]
capabilities:
  streaming: supported
  tools: unknown
  parallel_tools: unknown
auth: [api_key]
```

Это уточнение раннего manifest из PLAN: там `true`, `false` и `unknown` иллюстрируют ту же трёхзначную семантику. Для первой реализации выбираем один canonical формат и валидируем его строго.

## Execution stream

`Frame` — tagged union транспортных и контрольных сообщений, а не LLM events. У него ровно один variant:

| Variant | Данные | Семантика |
| --- | --- | --- |
| `Head` | HTTP status и response headers | Единожды, до любого body chunk |
| `Body` | Непустой набор bytes | Исходный или уже переведённый connector ответ |
| `Complete` | Outcome и финальный UsageReport | Единожды; закрывает успешный транспортный lifecycle |

Допустимая последовательность — `Head → Body* → Complete → EOF`. `Complete` может описывать upstream HTTP error, а не только успешную генерацию. До `Head` connector может вернуть typed error; после `Head` неожиданный `Next` error означает неполный поток. EOF без `Complete` также считается incomplete.

Body chunks не обязаны совпадать с границами SSE events. Core не должен переинтерпретировать их содержимое. Non-streaming JSON передаётся через тот же механизм. При native passthrough connector может параллельно разбирать ограниченную копию событий для usage, но передаваемые bytes остаются неизменными.

## Usage

`UsageEstimate` содержит известную оценку input/output budget, признак неизвестного значения и метод оценки. `UsageReport` содержит nullable counters `input_tokens`, `output_tokens`, `reasoning_tokens`, `cached_tokens`, источник (`provider`, `estimate`, `unknown`) и признак полноты. Неизвестное значение не равно нулю.

Input/output counters считаются верхнеуровневыми величинами. Cached и reasoning tokens — детализация, которая может уже входить в totals; их нельзя безусловно прибавлять ещё раз. Connector нормализует только accounting semantics и сохраняет backend-specific детали в namespaced diagnostics при необходимости.

Если клиент отключился до получения usage, Core сохраняет partial/unknown outcome и применённую оценку отдельно. Отсутствие usage не превращает выполненный запрос в бесплатный и не позволяет удалить reservation без следа.

## Ошибки

Typed error описывает инфраструктурную категорию: `invalid_request`, `unsupported_feature`, `unauthenticated`, `permission_denied`, `rate_limited`, `unavailable`, `timeout`, `cancelled` или `internal`. Дополнительно передаются retry disposition, опциональный retry delay и безопасное сообщение для клиента.

Native connector может вернуть исходный upstream error как `Head` и `Body`, сохраняя внешний протокол. Если Core рассматривает fallback, решение и retry metadata должны быть доступны до пересылки `Head` клиенту. Translation connector преобразует upstream error в northbound-compatible bytes; ошибки самого gateway кодирует northbound adapter.

## Runtime services и auth

Runtime предоставляет выбранные instance/account context, scoped доступ к credential, HTTP transport, logger и общий auth runtime. Он не передаёт connector всю БД или credentials других аккаунтов. Connector может использовать secret во время вызова, но не сохраняет его самостоятельно.

`Authenticate` работает как state machine: начать flow, продолжить после callback/device polling или выполнить refresh. `AuthResult` описывает следующую action, завершение с обновлённым credential либо ошибку. Конкретные endpoints, scopes и обмен токенов остаются внутри connector. Core отвечает за state validation, PKCE storage, срок жизни auth session и атомарное сохранение результата.

Refresh сериализуется для одного account. При ротации refresh token новое значение сохраняется атомарно с expiry; параллельный запрос не должен затереть его старым. Ошибка storage после обмена токенов фиксируется как отдельный auth failure.

## Переход к IPC

В IPC переносятся opaque request bytes, metadata, descriptor и те же stream frames. Для вызовов Runtime со стороны внешнего connector потребуется отдельный scoped host-service channel; одного server-streaming `Execute` RPC для этого недостаточно. В M6 нужно проверить lifecycle обоих направлений, authentication локального канала и отзыв доступа при завершении attempt.

Handshake проверяет совместимость major contract version до выполнения запросов. Unknown control frame нельзя пропускать так же, как unknown JSON field: transport contract должен оставаться однозначным. Совместимость payload обеспечивается его непрозрачностью, совместимость IPC — отдельными правилами версионирования.
