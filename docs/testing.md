# Проверка совместимости

## Что доказывают тесты

Главный объект проверки — наблюдаемое поведение gateway. Для native mode проверяется сохранение bytes и transport lifecycle; для translation — семантика внешнего протокола в пределах заявленных capabilities. Совпадение сгенерированного текста между двумя реальными запросами не является корректным критерием: модель может отвечать недетерминированно.

## Уровни проверки

Unit tests полезны для routing eligibility, retry decisions, auth state transitions и limits reconciliation. Интеграционные tests используют fake HTTP upstream и настоящие server/client connections: cancellation и streaming нельзя убедительно проверить только вызовом handler с recorder.

Conformance harness запускает одни сценарии против разных connectors и transports. Real upstream smoke tests проверяют актуальный backend и конкретную версию клиента отдельно от обычного CI. Они не заменяют детерминированные fixtures.

| Область | Обязательный сценарий | Проверяемый результат |
| --- | --- | --- |
| Passthrough | JSON с неизвестными вложенными полями и нестандартным whitespace | Body совпадает побайтно |
| Streaming | Несколько chunks с паузой | Первый chunk доступен до окончания ответа |
| Parsing | UTF-8, JSON и SSE delimiters разбиты между chunks | Нет потерь или повторов bytes/events |
| Tools | Один вызов и tool result в следующем запросе | Call ID сохраняет связь между раундами |
| Parallel tools | Несколько interleaved tool argument streams | IDs, индексы и порядок каждого item корректны |
| Multi-round | Несколько tool rounds | Контекст и идентификаторы остаются согласованными |
| Tool choice | Auto, forced tool и отключение tools | Заявленные режимы соблюдаются или явно отклоняются |
| Reasoning | Reasoning events и связанные counters | Заявленная семантика не теряется |
| Usage | Exact, estimated, missing и partial | Unknown не превращается в ноль, нет double counting |
| Early errors | Auth failure, 429, unavailable | Корректные status и retry disposition |
| Late errors | Disconnect после Head/первого chunk | Нет fallback и нового HTTP response |
| Cancellation | Клиент прекращает чтение | Upstream и внутренние ресурсы освобождены |
| Backpressure | Медленный клиент и длинный поток | Очереди и память остаются bounded |
| Limits | Конкурентный admission и повторный finalize | Нет обхода лимитов и duplicate accounting |
| Runtime | Crash, malformed frames, version mismatch | Core продолжает обслуживать другие requests |

## Capability-aware suite

Базовые проверки lifecycle, errors и cancellation обязательны для всех implementations. Проверки features запускаются согласно manifest. `Unsupported` проверяется отрицательным сценарием с ожидаемой ошибкой; `unknown` отображается как непроверенная возможность и не считается успешным прохождением.

Translation имеет отдельную feature matrix: images, hosted tools, reasoning variants, background execution и session resume не признаются поддержанными только потому, что обычный текст прошёл тест. Unknown fields гарантированно сохраняются в native mode; для translation документируется, какие поля можно переносить, а какие требуют явного отказа.

## Fixtures и traces

Fixtures группируются по connector, upstream protocol и версии исследованного клиента/API. Рядом хранятся описание сценария, источник и ожидаемые invariants. Из записей удаляются credentials, cookies, личные prompts и account identifiers; после sanitization fixtures должны оставаться валидными для тестируемого протокола.

Для dynamic IDs и timestamps используется структурное сопоставление: сами значения могут отличаться, но ссылки между tool call и tool result обязаны совпадать. Native byte-equality проверяется на одной и той же записи, без такой нормализации.

## Direct versus gateway

Один сценарий запускается напрямую к backend и через gateway с той же моделью, настройками и версией клиента. Сравниваются допустимая последовательность events, завершённость items, tool IDs relationships, количество раундов и accounting semantics. Ошибки и unsupported features тоже входят в сравнение.

Результат smoke test фиксируется с датой, connector version, upstream/client version и scope. Он подтверждает проверенную комбинацию, а не пожизненную совместимость со всеми моделями провайдера.

## Definition of done

Изменение готово, когда целевой сценарий воспроизводим, соответствующие conformance проверки проходят, capabilities и документация согласованы с поведением. Performance baseline фиксируется для native path: latency overhead, time to first byte, memory per active stream и отсутствие утечек после cancellation. Численные budgets задаются после M1 на указанной машине и нагрузке, а не придумываются заранее.
