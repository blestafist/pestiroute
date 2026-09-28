# Universal AI Gateway Runtime

> Практический план разработки, стек, контракты и критерии готовности собраны в [docs/README.md](docs/README.md).

## Видение проекта

Мы строим лёгкий self-hosted AI protocol gateway, который предоставляет OpenAI-compatible API и подключает любые AI-бэкенды через расширяемую систему connectors. Это не просто шлюз к LLM API: он должен работать с обычными API-провайдерами, существующими AI-подписками, локальными моделями и протоколами официальных AI-клиентов, в том числе восстановленными через reverse engineering.

Gateway централизует маршрутизацию между бэкендами, управление credentials, аккаунтами, лимитами и usage. При этом ядро остаётся минимальным: оно управляет инфраструктурой, а вся специфика конкретного бэкенда находится внутри connector.

Главное архитектурное ограничение — не создавать внутренний «универсальный LLM-язык». Внешний контракт уже существует: OpenAI Responses API. Connector самостоятельно решает, как доставить такой запрос своему бэкенду и вернуть совместимый ответ.

## 1. Граница между Core и connectors

Core отвечает за API server, аутентификацию клиентов, virtual keys, routing, limits, usage, жизненный цикл connectors и plugin runtime. Core ничего не знает о конкретных провайдерах: в нём не должно быть кода OpenAI, Anthropic или Gemini, провайдерских OAuth flows, форматов запросов и ответов, правил токенизации и форматов streaming конкретных бэкендов.

OpenAI-compatible API — внешний контракт шлюза, а не основание для появления провайдерской логики внутри инфраструктурного ядра. Обработка внешнего API должна оставаться отделённой от знаний о том, как устроен тот или иной upstream.

```text
                         Clients
                            |
                  OpenAI-compatible API
                            |
                 +----------v-----------+
                 |         CORE         |
                 | API Server / Auth    |
                 | Routing / Limits     |
                 | Usage / Runtime      |
                 +----------+-----------+
                            |
                    Connector Interface
                            |
             +--------------+--------------+
             |              |              |
       API Connector  Agent Connector  Local Connector
             |              |              |
       OpenAI API      Claude Code        Ollama
       Anthropic API   Codex              vLLM
       Gemini API      Gemini CLI         llama.cpp
```

## 2. Внешний API

Основной endpoint — `POST /v1/responses`. Он должен быть совместим с OpenAI Responses API, Codex clients, OpenCode и другими agent frameworks. Responses выбран основным контрактом, поскольку поддерживает reasoning, tool calls, parallel tools, streaming events и многошаговые agent workflows.

Дополнительно можно поддержать `POST /v1/chat/completions`. Этот compatibility endpoint должен преобразовывать запрос во внешний контракт Responses, а не создавать вторую независимую внутреннюю модель работы с LLM.

## 3. Что такое Connector

Connector — изолированная реализация взаимодействия с одним бэкендом или семейством совместимых бэкендов. Он принимает запрос в объявленном протоколе и самостоятельно определяет способ доставки: прямой API-вызов, OpenAI-compatible proxy, протокол официального клиента или обращение к локальному runtime.

Предполагаемая структура implementations:

```text
connectors/
  openai-api/
  anthropic-api/
  gemini-api/
  openai-compatible/
  claude-code/
  codex/
  gemini-cli/
  ollama/
  vllm/
```

### Native API connectors

Native API connector напрямую взаимодействует с API провайдера. Он отвечает за upstream-аутентификацию, формирование запросов, разбор ответов, streaming и извлечение usage. Например, путь к OpenAI API выглядит как `Core → OpenAI Connector → OpenAI API`.

### OpenAI-compatible connector

Общий OpenAI-compatible connector подключает бэкенды, уже реализующие совместимый API: OpenRouter, Together, Groq, LM Studio, vLLM и другие локальные или удалённые серверы. Подключение совместимого сервиса должно требовать минимальной конфигурации, без написания нового кода.

```yaml
connector: openai-compatible
base_url: https://backend.example/v1
api_key: <credential-reference>
models:
  - model-a
  - model-b
```

### Agent protocol connectors

Agent protocol connector воспроизводит способ общения официального AI-клиента с его бэкендом. Вместо запуска самого клиента gateway преобразует OpenAI Responses request в протокол Claude Code, Codex, Gemini CLI или другого поддерживаемого клиента и самостоятельно обращается к backend.

Знания об этом протоколе получают в том числе через reverse engineering. Connector воспроизводит поведение официального клиента, включая OAuth flow, доступ через существующую подписку, необходимые скрытые параметры и особенности взаимодействия с провайдером. Все эти детали остаются внутри connector.

Типичный путь запроса: `OpenAI Responses API → Claude Code Connector → Provider backend`.

### Local runtime connectors

Local runtime connectors обеспечивают работу с Ollama, vLLM и llama.cpp. Если runtime уже предоставляет подходящий OpenAI-compatible API, его можно подключать через общий compatible connector; отдельная реализация нужна для специфичного протокола или поведения.

## 4. Connector API

Интерфейс connector должен быть небольшим и инфраструктурным. Нельзя добавлять в него методы вроде `ChatCompletion()`, `Responses()`, `AnthropicMessages()` или `GeminiGenerate()`: такая форма протаскивает провайдерские модели в Core.

Предварительный контракт на Go:

```go
type Connector interface {
    Describe(ctx context.Context) Descriptor
    Authenticate(ctx context.Context, request AuthRequest, runtime Runtime) AuthResult
    EstimateUsage(ctx context.Context, request Request, runtime Runtime) UsageEstimate
    Execute(ctx context.Context, request Request, runtime Runtime) Stream
    Models(ctx context.Context, runtime Runtime) []Model
    Health(ctx context.Context, runtime Runtime) HealthStatus
}
```

`Describe` сообщает возможности connector, `Authenticate` выполняет его часть аутентификации, `EstimateUsage` оценивает расход перед выполнением, а `Execute` возвращает поток результата. `Models` предоставляет доступные модели, `Health` — состояние connector. Конкретные типы и механика ошибок уточняются при реализации без расширения интерфейса провайдерскими методами.

## 5. Request model и native passthrough

Запрос передаётся как opaque payload с указанием протокола и небольшим набором инфраструктурных metadata. Не нужно вводить универсальные `Messages[]`, `Tools[]`, `Reasoning{}` или `Images{}`: провайдеры развиваются независимо, и такая абстракция быстро начнёт ограничивать доступные возможности.

```go
type Request struct {
    Protocol string
    Headers  map[string][]string
    Body     []byte

    Metadata struct {
        Model        string
        Streaming    bool
        Requirements []Capability
    }
}
```

Например, `Protocol` содержит `openai.responses/v1`, а `Body` — исходный JSON. Metadata используется для routing и управления выполнением, но не заменяет payload внутренней LLM-моделью.

Native passthrough — обязательное требование. Если connector нативно поддерживает входящий протокол, тело запроса проходит путь `Client → Core → Connector → Provider` без изменений. Core не должен разбирать, пересобирать, нормализовывать payload или удалять неизвестные поля. Получение необходимых routing metadata на границе внешнего API не должно превращаться в реконструкцию тела запроса внутри Core.

Это сохраняет tool calls, parallel tool calls, reasoning и provider extensions, включая поля, о которых gateway ещё ничего не знает.

## 6. Translation

Если backend не поддерживает входящий протокол, преобразование выполняется исключительно внутри connector. Например, Anthropic connector переводит OpenAI Responses в Anthropic Messages и возвращает результат в совместимом с внешним API виде.

Только connector знает правила сопоставления полей, преобразования tools, streaming events, ошибок и usage. Core не участвует в семантическом переводе между API и не хранит промежуточную универсальную модель ответа или запроса.

## 7. Capabilities

Каждый connector декларирует свои возможности. Описание должно позволять различать подтверждённую поддержку, отсутствие поддержки и неизвестный статус.

```yaml
capabilities:
  streaming: true
  tools: true
  parallel_tools: true
  reasoning: true
  images: false
  session_resume: false
  exact_usage: true
```

Routing учитывает требования запроса: если нужны `tools`, `parallel_tools` и `reasoning`, выбирать можно только connectors, которые поддерживают все необходимые возможности. В MVP эти требования могут быть явно заданы metadata и конфигурацией маршрута; автоматический подбор по capabilities развивается позднее.

## 8. Authentication и credentials

Core владеет secret storage, шифрованием, credential references и управлением аккаунтами. Он также предоставляет общую инфраструктуру OAuth: callback server, PKCE и планирование refresh. При этом провайдерские OAuth endpoints, scopes, token exchange и реализация refresh принадлежат connector.

Поток аутентификации выглядит как `User → Core Auth Runtime → Connector Authenticate() → Provider → Credential Store`. Connector выполняет специфичную часть протокола через runtime, но самостоятельно не хранит secrets. Такое разделение позволяет централизованно управлять credentials, не добавляя в Core знания об отдельных провайдерах.

## 9. Usage и подсчёт токенов

Токенизация принадлежит connector, поскольку одинаковый запрос может занимать, например, 12 тысяч токенов у OpenAI и 13 тысяч у Anthropic. Core не должен выбирать tokenizer или интерпретировать провайдерские правила подсчёта.

Перед выполнением Core вызывает `connector.EstimateUsage()` и использует оценку для проверки лимитов. После выполнения connector извлекает фактический usage из ответа backend и передаёт его в Core Usage Storage. Отчёт включает `input_tokens`, `output_tokens`, `reasoning_tokens` и `cached_tokens`; capability `exact_usage` описывает наличие точного учёта.

## 10. Virtual keys

Gateway предоставляет собственные virtual API keys. Их можно создавать, отзывать, включать и отключать; для каждого ключа доступны ограничения по моделям и connectors, лимиты RPM и TPM, а также учёт usage. Клиент получает только virtual key и никогда не получает credentials провайдера.

## 11. Routing

В MVP routing строится на явном сопоставлении моделей, выборе connector и аккаунта, fallback и учёте лимитов. Для каждого запроса gateway должен определить подходящий маршрут и проверить необходимые возможности connector.

Позднее можно добавить выбор по latency и стоимости, а также автоматический capability matching. Эти механизмы не должны менять границу ответственности: Core выбирает исполнителя, connector понимает backend.

## 12. Streaming

Streaming обязателен с первой рабочей версии. Поток идёт по цепочке `Provider Stream → Connector → Core → Client`, без буферизации полного ответа. Gateway должен поддерживать SSE, передачу отмены выполнения от клиента до upstream, tool streaming, reasoning streaming и usage events.

Провайдерский streaming format разбирает connector. Core передаёт результат клиенту и управляет жизненным циклом запроса, не интерпретируя внутренний протокол backend.

## 13. Plugin runtime

Сторонний connector не должен иметь возможность обрушить Core своим падением. Поэтому Go native plugins не используются; целевая модель — отдельный connector process, связанный с Core через версионируемый IPC protocol.

Возможные transports — stdio, Unix socket или gRPC/Connect. Окончательный выбор должен обеспечивать streaming, cancellation, health checks, версионирование и изоляцию процессов. Внешний plugin runtime выделяется отдельным этапом, но граница connector проектируется с учётом такого исполнения с самого начала.

## 14. Connector manifest

Каждый connector предоставляет manifest с идентификатором, типом, версией, принимаемыми протоколами, capabilities и доступными способами аутентификации.

```yaml
id: claude-code
type: agent
version: "1.0"
protocols:
  accepts:
    - openai.responses/v1
capabilities:
  tools: true
  streaming: true
  parallel_tools: unknown
auth:
  - oauth
```

Manifest позволяет Core обнаруживать и использовать connector без знания его внутренней реализации.

## 15. Testing и conformance

Для connectors нужна общая conformance suite. Она проверяет basic request, streaming, tool calls, parallel tool calls, несколько последовательных tool rounds, reasoning, tool choice, usage, ошибки, cancellation и сохранение неизвестных полей. Проверки возможностей соотносятся с manifest; неподдерживаемая возможность не должна выглядеть как успешно поддерживаемая.

Ключевая регрессия — сравнение прямого подключения `OpenCode → Provider` с подключением `OpenCode → Gateway → Provider`. Поведение должно оставаться эквивалентным, особенно для параллельных tool calls, идентификаторов tools и порядка событий. Native passthrough отдельно проверяется на сохранение тела запроса без изменений.

## 16. Первые connectors

Первым нужен общий OpenAI-compatible connector: он покрывает большую часть совместимых сервисов. OpenAI API и Codex дают reference implementations для прямого API и клиентского протокола соответственно. Anthropic API нужен для проверки архитектуры translation, Claude Code — для subscription-backed usage, а Ollama/vLLM — для локальных моделей.

Эти реализации должны пользоваться одним инфраструктурным контрактом, не требуя провайдерских исключений в Core.

## 17. Миграция из 9Router

Из 9Router следует переиспользовать provider adapters, OAuth flows, stream parsers, извлечение usage, обработку моделей и накопленные знания о протоколах. Этот код переносится внутрь соответствующих connectors.

Старый routing, старое ядро и прежние абстракции не переносятся. Каждый мигрированный connector обязан пройти conformance suite: наличие работающего кода в 9Router само по себе не подтверждает совместимость с новым gateway.

## 18. Порядок реализации MVP

### Phase 1 — Transparent proxy

Собрать минимальный рабочий путь `OpenCode → Gateway → OpenAI Responses`. Главная цель — подтвердить прозрачную передачу запросов, streaming, tools и parallel tools. Эта версия задаёт baseline поведения для последующих изменений.

### Phase 2 — Connector API

Оформить минимальный Connector API и перенести OpenAI implementation в connector. Проверить, что инфраструктурное ядро больше не зависит от устройства upstream и сохраняет native passthrough.

### Phase 3 — Управление доступом и расходом

Добавить credentials, accounts, virtual keys, usage и limits. Связать оценку usage перед выполнением с проверкой лимитов и сохранением фактического usage после выполнения.

### Phase 4 — Anthropic connector

Добавить Anthropic API connector и реализовать translation из OpenAI Responses. Этим этапом проверить, что все преобразования запросов, ответов и streaming остаются внутри connector.

### Phase 5 — Agent connectors

Добавить connectors для Claude Code, Codex и ACP. Реализовать необходимые клиентские протоколы и способы аутентификации через общую инфраструктуру runtime.

### Phase 6 — External plugin runtime

Выделить выполнение connectors в отдельные процессы. Реализовать IPC, версионирование, health checks, streaming, cancellation и изоляцию с сохранением уже проверенного поведения API.

## 19. Что не входит в задачи проекта

Не следует начинать с UI, строить marketplace, billing, Kubernetes-инфраструктуру или распределённый кластер. Gateway также не должен становиться agent framework, системой prompt management или универсальной LLM-абстракцией. Приоритет — небольшой runtime с надёжным внешним контрактом и расширяемыми connectors.

## Итоговое архитектурное правило

**Core управляет инфраструктурой. Connector понимает backend. Эти обязанности нельзя смешивать.**

OpenAI Responses остаётся внешним контрактом. Все особенности API, подписок, клиентских протоколов и локальных runtime изолируются внутри connectors, а не превращаются в новый универсальный язык внутри gateway.
