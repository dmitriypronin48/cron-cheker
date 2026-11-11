# cron-checker

Централизованный учёт cron-задач и systemd-таймеров на Linux-хостах.

Pull-модель: центральный **сервер** периодически опрашивает **агентов** по HTTP.
Агенты пассивны — только отдают текущее состояние. Один общий секрет
(shared secret) в заголовке `Authorization: Bearer <token>`.

- Сервер — единственный Docker-контейнер (Go + SQLite + встроенный Web UI).
- Агент — статический Go-бинарник, ставится на хост напрямую или через
  systemd unit (RedOS/RHEL/Debian/Ubuntu).

Исходное ТЗ: [TASK.md](TASK.md).

## Архитектура

```
┌──────────────┐   GET /api/v1/cron (Bearer)   ┌─────────────────┐
│  cron-checker │ ───────────────────────────▶ │ cron-checker-   │
│  server       │ ◀────── model.AgentReport ── │ agent (Linux)   │
│  (docker)     │    каждые POLL_INTERVAL      │ порт 9300       │
│  SQLite + UI  │                              │ читает crontab, │
└──────────────┘                              │ cron.d, timers  │
                                              └─────────────────┘
```

- Сервер опрашивает агентов горутинами с семафором (`POLL_CONCURRENCY`,
  по умолчанию 10) и таймаутом (`POLL_TIMEOUT`, 15s). Один цикл = одна
  попытка на агента, без retry-штормов.
- Агент кэширует сбор данных на `COLLECT_INTERVAL` (5 мин): запросы сервера
  не вызывают повторное чтение файлов.
- Автообнаружения агентов нет — машины добавляются вручную (UI-форма или
  `POST /api/v1/agents`).

## Быстрый старт

### Сервер (docker compose)

Compose использует готовый образ (секция `build` отсутствует) — перед
запуском образ нужно собрать или загрузить:

```bash
make docker-image            # собрать cron-checker-server:latest
# либо загрузить готовый архив:
docker load -i cron-checker-server-0.1.0.tar

cd deploy
cp .env.example .env        # задать CRON_SERVER_TOKEN!
docker compose up -d
```

UI: http://localhost:8090 · `/healthz` · `/metrics`.

### Агент (Linux)

```bash
# вариант 1: бинарник
CRON_AGENT_TOKEN=<тот же токен> ./cron-checker-agent-linux-amd64

# вариант 2: пакет (создаёт systemd unit и /etc/cron-checker/agent.env)
sudo dpkg -i cron-checker-agent_0.1.0_amd64.deb   # или rpm -i …
sudoedit /etc/cron-checker/agent.env              # CRON_AGENT_TOKEN
sudo systemctl restart cron-checker-agent
```

Затем в UI «Add agent» → hostname + IP хоста.

### Сборка из исходников

```bash
make build                  # бинарники в bin/ (без CGO)
make build-linux-amd64      # кросс-сборка под Linux
make build-linux-arm64
make test / make vet
make package-deb            # требует nfpm: https://nfpm.goreleaser.com
make package-rpm
make docker-image
```

## Установка агента на RedOS

Юнит `deploy/cron-checker-agent.service` — обычный systemd, совместим с RedOS 7.x:

```bash
sudo rpm -i cron-checker-agent-0.1.0-1.x86_64.rpm
sudoedit /etc/cron-checker/agent.env   # CRON_AGENT_TOKEN=...
sudo systemctl enable --now cron-checker-agent
sudo systemctl status cron-checker-agent
curl -H "Authorization: Bearer $CRON_AGENT_TOKEN" http://127.0.0.1:9300/api/v1/cron
```

Агент должен работать от root (читает crontab всех пользователей и
`/var/spool/cron*`). Бинарник статический (`CGO_ENABLED=0`), зависимостей нет.

## Что собирает агент

1. `/var/spool/cron/crontabs/*` (Debian/Ubuntu) и `/var/spool/cron/*` (RHEL)
2. `/etc/crontab`, `/etc/cron.d/*`
3. `/etc/cron.hourly|daily|weekly|monthly/*` (имена файлов + владелец)
4. systemd timers: `systemctl list-timers --all --no-pager --output=json`
   + `OnCalendar=` из unit-файлов (`/etc/systemd/system`, `/usr/lib/systemd/system`)
5. `crontab -l -u <user>` для всех пользователей из `/etc/passwd`

Строки крона **не парсятся** — хранится сырая строка (`raw_line`) + метаданные
(источник, путь, пользователь, номер строки). Секреты в строках не логируются
(уровень DEBUG).

## Web UI

- Тёмная тема, Go templates + HTMX + свой CSS, всё встроено в бинарник,
  без внешних CDN.
- `/` — таблица машин: hostname, IP, статус 🟢/🔴, last seen, число записей,
  число новых за 24ч, timezone. Клик — детальная страница.
- `/agents/{id}` — записи, сгруппированные по источникам; бейдж **NEW**
  для записей моложе 24ч (снимается кнопкой **Ack**); фильтры по source/user;
  кнопка истории — diff со предыдущим snapshot.
- `/search?q=` — поиск по hostname/ip/user/raw_line с подсветкой.
- Время хранится в UTC, отображается в timezone агента.

## HTTP API сервера

| Метод | Путь | Назначение |
|---|---|---|
| GET | `/` | UI: список машин |
| GET | `/agents/{id}` | UI: cron машины |
| GET | `/agents/{id}/history` | UI: diff snapshots |
| GET | `/search?q=` | UI: поиск |
| GET | `/api/v1/agents` | JSON список агентов |
| POST | `/api/v1/agents` | регистрация агента `{hostname, ip}` |
| GET | `/api/v1/agents/{id}/cron` | JSON entries (`?source=&user=&only_new=1`) |
| GET | `/api/v1/agents/{id}/history` | JSON snapshots (последние N) |
| POST | `/api/v1/entries/{id}/ack` | «закрыть» новую строку |
| GET | `/api/v1/search?q=` | JSON поиск |
| GET | `/healthz`, `/metrics` | health, Prometheus |

Агент: `GET /healthz`, `GET /metrics` (без авторизации),
`GET /api/v1/cron` (требует Bearer-токен, constant-time сравнение).

## Переменные окружения

### Сервер

| Переменная | По умолчанию | Описание |
|---|---|---|
| `CRON_SERVER_TOKEN` | — | **обязателен**, = `CRON_AGENT_TOKEN` на агентах |
| `SERVER_LISTEN` | `:8090` | адрес HTTP |
| `DB_PATH` | `/data/cron-checker.db` | файл SQLite (volume в compose) |
| `POLL_INTERVAL` | `300s` | период опроса агентов |
| `POLL_CONCURRENCY` | `10` | параллельных опросов |
| `POLL_TIMEOUT` | `15s` | таймаут на агента |
| `AGENT_PORT` | `9300` | порт агента на хостах |
| `SNAPSHOT_RETENTION_DAYS` | `7` | хранение сырых snapshot-JSON |
| `REMOVED_RETENTION_HOURS` | `72` | хранение записей со статусом `removed` |
| `LOG_LEVEL` | `info` | slog |

### Агент

| Переменная | По умолчанию | Описание |
|---|---|---|
| `CRON_AGENT_TOKEN` | — | **обязателен**, = `CRON_SERVER_TOKEN` |
| `LISTEN_ADDR` | `:9300` | адрес HTTP |
| `COLLECT_INTERVAL` | `300s` | кэш локального сбора (не путать с опросом) |
| `LOG_LEVEL` | `info` | slog |

## Метрики Prometheus

Сервер (`/metrics`):

```
cron_checker_agents_total{status="online|offline"}
cron_checker_agent_up{hostname,ip}
cron_checker_agent_last_seen_timestamp{hostname,ip}
cron_checker_entries_total{hostname,source,status}
cron_checker_new_entries_24h{hostname}
cron_checker_poll_duration_seconds   (histogram)
cron_checker_poll_errors_total{hostname}
```

Агент (`/metrics`):

```
cron_checker_agent_up
cron_checker_agent_last_collect_timestamp
cron_checker_agent_entries_total{source}
cron_checker_agent_collect_errors_total
cron_checker_agent_http_requests_total{path,method,code}
```

Пример scrape-config:

```yaml
scrape_configs:
  - job_name: cron-checker-server
    static_configs:
      - targets: ["cron-checker:8090"]
  - job_name: cron-checker-agent
    static_configs:
      - targets:
          - host1.example:9300
          - host2.example:9300
```

## Структура репозитория

```
├── cmd/server, cmd/agent     # точки входа
├── internal/agent            # сбор cron/systemd, HTTP, кэш, метрики
├── internal/server           # HTTP API, UI, poller, diff, метрики
├── internal/store            # SQLite, миграции, fingerprint
├── internal/model            # общие типы
├── web/                      # templates + static (embed, HTMX локально)
├── deploy/                   # Dockerfile, compose, systemd, nfpm, scripts, env-образцы
├── Makefile
└── TASK.md                   # исходное ТЗ
```

## Решения по умолчанию (зафиксировано)

- Модуль Go: `github.com/example/cron-checker` (при публикации замените на
  свой путь).
- Маршрутизация — stdlib `net/http` ServeMux (Go 1.22+ pattern) на агенте,
  chi на сервере.
- Токен сравнивается через `crypto/subtle.ConstantTimeCompare`; токен
  существует только в env, в репозитории его нет.
- Удалённые записи помечаются `status=removed` и чистятся через
  `REMOVED_RETENTION_HOURS` (по умолчанию 72ч, в ТЗ — 24ч; меняется env).
- Healthcheck в контейнере: `cron-checker-server -healthcheck` (в образе
  нет curl, бинарник проверяет сам себя).
