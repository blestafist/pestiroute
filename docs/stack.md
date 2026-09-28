# Технологический стек

## Базовый выбор

Предлагаемый стек рассчитан на один self-hosted процесс, простой запуск и предсказуемый streaming. Основной язык — **Go**: он подходит для HTTP proxy, конкурентных запросов, отмены через `context.Context` и сборки небольшого бинарника. Конкретную поддерживаемую версию фиксируем в `go.mod` и CI при создании проекта, не привязывая план к плавающему `latest`.

| Область | Предлагаемое решение | Причина |
| --- | --- | --- |
| HTTP server и client | `net/http`, `http.Transport` | Управляемые соединения, cancellation и streaming без большого framework |
| Маршруты API | `http.ServeMux` | Для первого набора endpoints достаточно стандартной библиотеки |
| JSON | `encoding/json` на API-границе и внутри connectors | Core передаёт исходные bytes, а не сериализует запрос заново |
| Конфигурация | YAML через `go.yaml.in/yaml/v3` | Читаемая конфигурация с явной схемой и проверкой неизвестных ключей |
| Storage | SQLite через `database/sql` и `modernc.org/sqlite` | Локальное хранение без отдельного database service и без CGO |
| SQL | Явные запросы и версионируемые SQL migrations | Небольшая схема, прозрачные транзакции; ORM пока не нужен |
| Логи | `log/slog` | Структурированные события без дополнительного logging framework |
| Метрики | Prometheus client | Counters, gauges и latency histograms для эксплуатации |
| Тестирование | `testing`, `httptest`, Go fuzzing и race detector | Большая часть протокольных проверок выполняется локально |
| Упаковка | Go binary и контейнер | Удобный запуск на сервере или локальной машине |
| External connectors | Предварительно gRPC + Protobuf поверх Unix socket | Версионирование, streaming и cancellation через типизированный transport |

Это стартовые предпочтения. SQLite driver проверяется небольшим spike на migrations, конкурентных записях и поддерживаемых платформах. Выбор IPC закрепляется только после сравнения вариантов на реальном streaming-сценарии в M6.

## HTTP и streaming

У каждого connector instance переиспользуемый HTTP client с connection pooling. Таймауты разделяются на установление соединения, TLS handshake, получение response headers и idle time активного потока. Один короткий общий `Client.Timeout` не подходит для длинных agent responses.

На стороне server задаются ограничения размера request body, время чтения headers и graceful shutdown. Полное тело входного JSON можно прочитать в ограниченный buffer, поскольку контракт использует `[]byte`. Запрет на response buffering относится к выходному потоку: ответ передаётся клиенту по мере поступления.

SSE parser находится в connector или его protocol helper. Он обязан корректно обрабатывать разбиение UTF-8 и JSON между сетевыми chunks, multiline events и события больше стандартного лимита `bufio.Scanner`. Core работает с bytes и не разбирает SSE.

## Хранение и secrets

SQLite хранит аккаунты, encrypted credentials, virtual keys, usage и журнал migrations. Режим WAL, busy timeout и короткие транзакции уменьшают конкуренцию; сетевой вызов никогда не выполняется внутри SQL transaction. Для одного процесса допускается сериализованный writer, если нагрузочные проверки подтверждают достаточную пропускную способность.

Credentials шифруются средствами стандартной библиотеки, например AES-GCM с уникальным nonce и AAD, связывающим ciphertext с credential ID и версией формата. Master key поступает из отдельного файла или окружения и не сохраняется в той же БД. Virtual keys генерируются из криптографически случайных bytes, показываются при создании и хранятся только как digest с отдельным публичным идентификатором.

## Observability

Каждому запросу и attempt назначается ID. В логах нужны route, connector instance, account ID, execution mode, status, latency, time to first byte, причина fallback и итог usage. Тела prompts, ответы и secrets по умолчанию не логируются.

Метрики отражают число запросов, активные streams, ошибки, время выполнения, first-byte latency, отказы limits и сбои connectors. Request IDs, произвольные model names и account IDs не используются как неограниченные metric labels. Tracing через OpenTelemetry можно добавить после появления устойчивого request lifecycle.

## Сборка и рабочий цикл

Начинаем с одного Go module и одного основного binary `gateway`. Минимальный CI выполняет проверку `gofmt`, `go vet ./...`, `go test ./...`, `go test -race ./...` и сборку. Protocol fixtures не требуют сетевых credentials. Реальные upstream smoke tests запускаются отдельно и имеют явно ограниченный объём запросов.

Контейнер запускается от непривилегированного пользователя, получает конфигурацию и persistent data directory через mounts и содержит CA certificates для upstream TLS. SQLite backup выполняется согласованным snapshot/backup способом; простое копирование одного файла активной WAL-базы не считается корректной процедурой.
