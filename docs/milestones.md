# Milestones

## Общая последовательность

Работа идёт от прозрачного request path к управляемому runtime. M1–M6 соответствуют фазам PLAN; M0 добавляет короткую подготовку. Оценки ниже — ориентир для одного разработчика, знакомого с Go, а не календарное обязательство. Неопределённость особенно велика для subscription protocols и translation.

| Этап | Результат | Ориентир |
| --- | --- | --- |
| M0 | Каркас проекта и fake upstream | 2–3 рабочих дня |
| M1 | Native Responses proxy | 4–7 дней |
| M2 | Рабочая граница Connector API | 4–7 дней |
| M3 | Accounts, virtual keys, usage и limits | 8–12 дней |
| M4 | Anthropic translation и compatibility matrix | 8–15 дней |
| M5 | Codex / Claude Code и локальные backends | Отдельный spike и оценка на каждый протокол |
| M6 | External connector process runtime | 8–15 дней после IPC spike |

M3 завершает минимально пригодную для самостоятельного использования native версию. M4 проверяет главное архитектурное предположение о translation. Поддержка внешних сторонних plugins появляется после M6.

## M0 · Основа проекта

Создать Go module, entrypoint, configuration loader и минимальные health/readiness endpoints. Добавить fake upstream, который умеет отдавать JSON и управляемый SSE stream, задерживать chunks, разрывать соединение и наблюдать cancellation. Зафиксировать toolchain и базовый CI.

**Готово, когда:** binary запускается из чистого checkout по описанной команде; invalid configuration останавливает startup с понятной ошибкой; локальный тест проходит без внешних credentials. Health означает живой процесс, readiness — готовность принимать запросы с загруженной конфигурацией.

## M1 · Прозрачный Responses proxy

Реализовать `POST /v1/responses` с одним явно настроенным target, bounded request body, native forwarding и cancellation. Backend-specific код с первого дня поместить в отдельный пакет, даже до окончательного Connector API. Для smoke tests использовать существующий upstream model ID без alias rewrite.

**Готово, когда:** bytes request body, включая unknown fields, совпадают на входе fake upstream; response bytes сохраняются; первый chunk доходит до клиента до завершения upstream ответа; tool IDs и порядок events не меняются. Disconnect клиента закрывает upstream request; поздний upstream failure не запускает второй ответ. Выполнен один документированный smoke test реального клиента с tools и parallel tool calls.

## M2 · Connector API и conformance baseline

Выделить execution envelope, registry, descriptor, stream frames, typed errors и runtime services. Перенести M1 connector за общий interface. Добавить model/capability eligibility, explicit routing и фиксацию attempts. Реализовать тестовый connector с предсказуемыми failures, чтобы routing не проверялся только успешным HTTP proxy.

**Готово, когда:** Core не импортирует concrete connectors и protocol parsers; conformance harness запускается против native и fake implementations; native regression suite M1 остаётся зелёной. Неизвестная capability не удовлетворяет обязательному requirement, а malformed stream sequence фиксируется как runtime error. Scoped contract достаточно конкретен для реализации без скрытого глобального состояния.

## M3 · Доступ, accounts и accounting

Добавить SQLite schema/migrations, secret storage, accounts, CLI управления virtual keys, key policies и usage records. Реализовать reservations, reconciliation, recovery после перезапуска и bounded fallback только для safe failures. OAuth runtime interfaces готовятся здесь; реальные provider flows проверяются на M5.

**Готово, когда:** revoked key не достигает upstream; параллельные запросы не обходят admission limit; usage не дублируется при повторном завершении; interrupted attempts корректно восстанавливаются. Credentials переживают restart в encrypted storage, master key не хранится в БД. Fallback соблюдает restrictions ключа и не выполняется после commit или ambiguous delivery.

## M4 · Первый translation connector

Реализовать Anthropic API connector как явное преобразование Responses ↔ Messages. Сначала проверить обычный текст и streaming, затем tools, parallel tool calls, несколько tool rounds, tool choice, usage и supported reasoning behavior. Для каждого feature записать supported/unsupported/unknown и ограничения преобразования.

**Готово, когда:** conformance suite подтверждает заявленные возможности; tool call IDs и tool results правильно связываются между раундами; streaming events имеют корректный lifecycle. Неподдерживаемые поля с существенной семантикой отклоняются с понятной ошибкой, а не молча теряются. В Core не появились ветки по Anthropic model или error code.

## M5 · Подписки и локальные модели

Для Codex и Claude Code начать с короткого research spike: закрепить исходники/версию клиента, auth flow, request format, streaming и минимальный trace. После этого реализовать каждый connector отдельно, включая refresh и account affinity. Gemini CLI следует тому же процессу по приоритету.

Ollama/vLLM сначала проверяются через общий compatible connector. Если backend предоставляет только Chat Completions, нужен заявленный Responses translator либо отдельный connector; одно лишь совпадение URL-стиля не означает готовую поддержку.

ACP рассматривается отдельно: это протокол общения с agent process, а не эквивалент subscription backend API. Spike должен определить конкретного агента, transport и владельца agent/tool loop до включения ACP в implementation scope.

**Готово для каждого connector, когда:** свежая авторизация, expiry и concurrent refresh проверены; нет собственной secret persistence; versioned fixtures и capability matrix добавлены; direct-versus-gateway smoke test воспроизводим. Неизвестные stateful возможности явно обозначены. Каждый connector можно выпускать независимо от остальных.

## M6 · External plugin runtime

Сравнить stdio framing и gRPC over Unix socket на одном сценарии с долгим потоком и cancellation. После выбора реализовать handshake, process supervision, scoped host services, bounded queues, shutdown и restart backoff. Пропустить тот же native connector через process boundary.

**Готово, когда:** crash connector не завершает gateway; slow consumer не вызывает неограниченный рост памяти; incompatible contract version отклоняется до выполнения; cancellation останавливает работу и освобождает process resources. Conformance результаты in-process и external implementations совпадают. Повторно запускать уже начатую генерацию после crash автоматически нельзя.

## Первая задача после документации

Начать с M0 и одного интеграционного теста M1: fake upstream принимает JSON с неизвестным полем и выдаёт два SSE chunks с управляемой паузой. Тест одновременно проверяет сохранение payload, немедленную доставку первого chunk и cancellation. Это создаёт полезный baseline раньше, чем появятся полноценные auth, storage и plugin SDK.
