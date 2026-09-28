# Данные и конфигурация

## Разделение состояния

YAML описывает желаемую топологию: listener, connector instances, routes и policy defaults. SQLite хранит изменяемое состояние: accounts, credentials, virtual keys, auth sessions, attempts и usage. Secrets в YAML заменяются credential references. При старте configuration validation должна находить несуществующие ссылки и несовместимые сочетания protocol/mode до первого клиентского запроса.

Hot reload не нужен для первого MVP. Конфигурация применяется целиком при старте; административные операции над ключами и аккаунтами используют storage и runtime services. Первый интерфейс управления — локальные CLI-команды, позже возможен отдельный admin API.

## Предлагаемый YAML

Это целевая схема M3. Значения в `settings` принадлежат connector и валидируются им; Core не интерпретирует `base_url` или upstream protocol. Credentials и accounts из примера предварительно создаются через административный интерфейс.

```yaml
version: 1

server:
  listen: "127.0.0.1:8080"
  max_request_bytes: 16777216

storage:
  driver: sqlite
  path: ./data/gateway.db

secrets:
  master_key_file: ./secrets/master.key

connectors:
  - id: primary
    implementation: openai-compatible
    settings:
      base_url: https://backend.example/v1
      upstream_protocol: openai.responses/v1
      mode: native

routes:
  - id: default-model
    match:
      protocol: openai.responses/v1
      model: model-a
    requirements: [streaming, tools]
    targets:
      - connector: primary
        account: primary-account
    retry:
      max_attempts: 1

policies:
  default:
    rpm: 60
    tpm: 100000
    unknown_usage: reject
```

Имена моделей в native route передаются без замены. Requirements маршрута — явное требование оператора, а не автоматическое заключение, что каждый запрос использует tools. Значения limits приведены как пример, а не как рекомендуемые лимиты любого провайдера.

## Основные сущности

| Сущность | Назначение и важные поля |
| --- | --- |
| `accounts` | Connector instance, credential reference, enabled state, health/cooldown |
| `credentials` | Encrypted payload, format version, key version, expiry и revision |
| `virtual_keys` | Public ID, digest, enabled/revoked state, policy и timestamps |
| `key_policies` | Model/connector allowlists, RPM, TPM и дополнительные ограничения |
| `auth_sessions` | Flow ID, account, срок жизни и защищённое временное состояние |
| `requests` | Request ID, virtual key ID, route и итоговый outcome |
| `attempts` | Attempt ID, account, timings, commit state, error и retry reason |
| `usage_records` | Counters, source, completeness, estimate и связь с attempt |
| `reservations` | Зарезервированный budget, lifecycle и reconciliation state |
| `schema_migrations` | Применённые версии схемы |

Модель рассчитана на один gateway process. Все timestamps хранятся в UTC. Request/attempt identifiers позволяют расследовать fallback без хранения prompt. Удаление аккаунта не должно каскадно уничтожать исторические usage records.

## Virtual keys и доступ

Ключ идентифицирует policy, по которой проверяются модели и connector targets до выполнения. Ограничение на model alias применяется к имени клиента; connector restriction проверяется и для первоначального target, и для fallback. Неизвестный или отозванный ключ отклоняется до обращения к upstream.

Admin interface отделён от публичных inference endpoints. Для первого этапа достаточно CLI с доступом к локальному data directory; operations создания и отзыва ключей фиксируются без вывода secrets в общий лог.

## Limits и reconciliation

Admission состоит из атомарной проверки RPM и reservation token budget. В MVP RPM считается по принятым клиентским запросам; upstream attempts учитываются отдельно, чтобы fallback не скрывал реальную нагрузку. TPM считается по input + output, без повторного сложения cached/reasoning detail counters.

Оценка не гарантирует жёсткую верхнюю границу upstream consumption. Если backend поддерживает execution budget, connector применяет его в соответствии с выбранным режимом; native passthrough не позволяет незаметно добавить ограничение в body. После завершения reservation заменяется фактическим usage, а превышение уменьшает доступный бюджет последующих запросов.

Для неизвестной оценки нужны явные policies: `reject` или фиксированный консервативный reservation. Молчаливое резервирование нуля запрещено. Точная величина conservative budget задаётся оператором для маршрута, а не угадывается Core.

Reconciliation идемпотентен по attempt ID. Если gateway перезапустился с активными reservations, startup recovery помечает attempts как interrupted, сохраняет conservative charge и не выдаёт неизвестный upstream результат за отменённое выполнение. Refresh и лимиты не держат SQL transactions открытыми на время сетевых вызовов.
