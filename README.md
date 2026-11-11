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

- Сервер опрашивает агентов горутинами с семафором (`poll_concurrency`,
  по умолчанию 10) и таймаутом (`poll_timeout`, 15s). Один цикл = одна
  попытка на агента, без retry-штормов.
- Агент кэширует сбор данных на `COLLECT_INTERVAL` (5 мин): запросы сервера
  не вызывают повторное чтение файлов.
- Автообнаружения агентов нет — машины перечисляются в инвентаре
  конфиг-файла сервера (`agents:`), добавление через веб отсутствует.

## Быстрый старт

### Сервер (docker compose)

Compose использует готовый образ (секция `build` отсутствует) — перед
запуском образ нужно собрать или загрузить:

```bash
make docker-image            # собрать cron-checker-server:latest
# либо загрузить готовый архив:
docker load -i cron-checker-server-0.1.0.tar

cd deploy
cp config.example.yaml config.yaml   # задать token и список агентов!
docker compose up -d
```

UI: http://localhost:8090 · `/healthz` · `/metrics`.

Настройки сервера и инвентарь агентов живут в одном файле `config.yaml`
(в контейнере: `/etc/cron-checker/server.yaml`). Добавление агентов через
веб-интерфейс отсутствует — хосты добавляются и удаляются только в этом
файле: агенты, удалённые из файла, стираются из веба и БД в следующем
цикле опроса (записи «offline» не зависают). Файл перечитывается каждый
цикл опроса, перезапуск не нужен.

### Агент (Linux)

```bash
# вариант 1: бинарник
CRON_AGENT_TOKEN=<тот же токен> ./cron-checker-agent-linux-amd64

# вариант 2: пакет (создаёт systemd unit и /etc/cron-checker/agent.yaml)
sudo dpkg -i cron-checker-agent_0.1.0_amd64.deb   # или rpm -i …
sudoedit /etc/cron-checker/agent.yaml             # token: ...
sudo systemctl restart cron-checker-agent
```

Периодичность обновления локального состояния cron (кэш агента, по
умолчанию каждые 5 минут) задаётся в YAML-конфиге агента
`/etc/cron-checker/agent.yaml`:

```yaml
token: "same-as-server"
listen: ":9300"
collect_interval: 5m     # 3m, 15m, 1h, ...
log_level: info
```

Файл опционален: если его нет, работают env-переменные и флаги. Приоритет
каждого параметра: **флаг > env > конфиг-файл > значение по умолчанию**.

Затем добавьте хост (hostname + IP) в `agents:` файла `config.yaml`
сервера — на следующем цикле опроса он появится в вебе.

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
sudoedit /etc/cron-checker/agent.yaml   # token: ...
sudo systemctl enable --now cron-checker-agent
sudo systemctl status cron-checker-agent
curl -H "Authorization: Bearer $CRON_AGENT_TOKEN" http://127.0.0.1:9300/api/v1/cron
```

Агент должен работать от root (читает crontab всех пользователей и
`/var/spool/cron*`). Бинарник статический (`CGO_ENABLED=0`), зависимостей нет.

## Что собирает агент

1. `/var/spool/cron/crontabs/*` (Debian/Ubuntu) и `/var/spool/cron/*` (RHEL)
2. `/etc/crontab`, `/etc/cron.d/*`
3. `/etc/cron.hourly|daily|weekly|monthly/*` — **содержимое файлов**
   построчно (имя файла остаётся в колонке path, владелец — в user)
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
- Многострочные файлы (скрипты в `cron.weekly/` и т.п., а также файлы с
  несколькими заданиями: `/etc/crontab`, `cron.d`) показываются **одной
  сворачиваемой строкой**: имя файла, число строк и полный путь; клик
  раскрывает весь файл целиком в моноширинном блоке. Кнопка **Ack all**
  закрывает все строки файла разом.
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
| GET | `/api/v1/agents/{id}/cron` | JSON entries (`?source=&user=&only_new=1`) |
| GET | `/api/v1/agents/{id}/history` | JSON snapshots (последние N) |
| POST | `/api/v1/entries/{id}/ack` | «закрыть» новую строку |
| POST | `/api/v1/entries/ack-block` | ack всех строк файла (`ids=1,2,3`) |
| GET | `/api/v1/search?q=` | JSON поиск |
| GET | `/healthz`, `/metrics` | health, Prometheus |

Агент: `GET /healthz`, `GET /metrics` (без авторизации),
`GET /api/v1/cron` (требует Bearer-токен, constant-time сравнение).

## Конфигурация сервера

Единый YAML-файл (по умолчанию `/etc/cron-checker/server.yaml`, путь
переопределяется флагом `-config` или переменной окружения
`CRON_CHECKER_CONFIG`). Формат — `deploy/config.example.yaml`:

```yaml
listen: ":8090"                  # HTTP-адрес
db_path: /data/cron-checker.db    # файл SQLite
token: "secret"                   # = CRON_AGENT_TOKEN на агентах (обязателен)
poll_interval: 300s               # период опроса агентов
poll_concurrency: 10              # параллельных опросов
poll_timeout: 15s                 # таймаут на агента
agent_port: "9300"                # порт агента на хостах
snapshot_retention_days: 7        # хранение сырых snapshot-JSON
removed_retention_hours: 72       # хранение записей со статусом removed
log_level: info
agents:                           # инвентарь: один хост — одна пара hostname/ip
  - hostname: web1
    ip: 192.168.0.10
  - hostname: db1
    ip: 192.168.0.20
```

Файл перечитывается каждый цикл опроса: добавленные хосты появляются в вебе,
удалённые стираются из веба и БД вместе со своими записями и снапшотами.
Если файл временно недоступен/испорчен — сервер продолжает опрос с прежним
набором агентов и пишет warning в лог.

## Переменные окружения

### Сервер

Сервер настраивается только конфиг-файлом выше (см. «Конфигурация
сервера»); переменные окружения не используются.

### Агент

Агент можно настроить тремя способами (по убыванию приоритета): флаги
командной строки, переменные окружения, YAML-конфиг
(`/etc/cron-checker/agent.yaml`, пример — `deploy/agent.config.example.yaml`).
См. «Быстрый старт → Агент».

| Переменная | По умолчанию | Описание |
|---|---|---|
| `CRON_AGENT_TOKEN` | — | **обязателен**, = `token` в конфиг-файле сервера |
| `LISTEN_ADDR` | `:9300` | адрес HTTP |
| `COLLECT_INTERVAL` | `300s` (= `collect_interval`) | как часто агент перечитывает cron-файлы |
| `LOG_LEVEL` | `info` | slog |

## Метрики Prometheus

Сервер (`/metrics`):

```
cron_checker_agents_total{hostname,ip,status}          # 1 для текущего статуса каждого агента
cron_checker_agent_up{hostname,ip}
cron_checker_agent_last_seen_timestamp{hostname,ip}
cron_checker_entries_total{hostname,source,status}
cron_checker_new_entries_24h{hostname}
cron_checker_new_entry{hostname,ip,source,raw_line}   # 1 пока запись новая (≤24ч, не acked);
                                                      # 0 после Ack или по истечении 24ч (серия не удаляется)
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

## Kubernetes (Helm)

В `helm-chart/` — чарт для деплоя **только сервера** в Kubernetes
(агенты остаются на Linux-хостах вне кластера и опрашиваются по IP).
Конфигурация сервера (`server.yaml`, включая инвентарь `agents:`)
рендерится в ConfigMap и монтируется в контейнер, БД — на PVC.
Ingress — Traefik (`ingress.className: traefik`).

```bash
helm install cron-checker ./helm-chart \
  --set token=my-secret \
  --set ingress.host=cron-checker.example.com
# или со своим values.yaml:
helm upgrade --install cron-checker ./helm-chart -f my-values.yaml
```

Перед установкой образ `cron-checker-server:latest` должен быть доступен
кластеру (например, `docker save … | docker load` на нодах или приватный
registry — см. `image.repository`/`image.tag` в `helm-chart/values.yaml`).

## Структура репозитория

```
├── cmd/server, cmd/agent     # точки входа
├── internal/agent            # сбор cron/systemd, HTTP, кэш, метрики
├── internal/server           # HTTP API, UI, poller, diff, метрики
├── internal/store            # SQLite, миграции, fingerprint
├── internal/model            # общие типы
├── web/                      # templates + static (embed, HTMX локально)
├── deploy/                   # Dockerfile, compose, config-образцы (server+agent), systemd, nfpm, scripts
├── helm-chart/               # Helm-чарт сервера для Kubernetes (Traefik ingress, ConfigMap)
├── Makefile
└── TASK.md                   # исходное ТЗ
```

## Решения по умолчанию (зафиксировано)

- Модуль Go: `github.com/example/cron-checker` (при публикации замените на
  свой путь).
- Агенты добавляются только через инвентарь в конфиг-файле сервера: каждый
  цикл опроса БД синхронизируется с файлом (добавленные — inserted,
  удалённые — deleted cascade вместе с записями и снапшотами).
- Маршрутизация — stdlib `net/http` ServeMux (Go 1.22+ pattern) на агенте,
  chi на сервере.
- Токен сравнивается через `crypto/subtle.ConstantTimeCompare`; токен
  существует только в конфиг-файле сервера и в env агентов, в репозитории
  его нет.
- Удалённые записи помечаются `status=removed` и чистятся через
  `removed_retention_hours` конфига (по умолчанию 72ч, в ТЗ — 24ч).
- Healthcheck в контейнере: `cron-checker-server -healthcheck` (в образе
  нет curl, бинарник проверяет сам себя).
