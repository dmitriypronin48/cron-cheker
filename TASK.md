Сгенерируй полностью рабочий проект «cron-checker»: центральный сервер с Web UI и агент, который
устанавливается на Linux-хосты. Проект должен запускаться локально
через docker compose (только сервер) и собираться в .deb/.rpm/бинарь
(агент). Рядом с проектом, должен лежать готовый образ для docker-compose
Код — production-grade, но без оверинжиниринга. Всё, что не
описано явно — реши сам разумным дефолтом и зафиксируй в README.

================================================================
1. АРХИТЕКТУРА
================================================================

- Pull-модель: СЕРВЕР периодически опрашивает агентов по HTTP.
  Агенты пассивны: только отдают текущее состояние по GET-запросу.
- Сервер — единственный компонент в docker compose (плюс volume
  для SQLite). Агенты НЕ в docker: ставятся на хост как systemd
  unit или standalone бинарник.
- Shared secret: один общий токен на всех агентов и сервер.
  Передаётся в заголовке `Authorization: Bearer <token>`.
  Токен задаётся через env: `CRON_AGENT_TOKEN` (агент) и
  `CRON_SERVER_TOKEN` (сервер). Сравнение — constant-time.
- Масштаб: до 100 агентов. Никаких Redis/RabbitMQ. Опрос — горутины
  с ограничением concurrency (по умолчанию 10) и per-agent timeout
  (по умолчанию 15s).
- Если агент недоступен — сервер помечает его `offline`, продолжает
  опрашивать по расписанию, при следующем успехе снова `online`.
  Никаких retry-штормов: один цикл опроса = одна попытка на агента.
- Если сервер недоступен — агент просто отдаёт ошибку на запросы
  (агент пассивен, ему некуда «досылать»). В UI это отображается
  как offline-машина.

================================================================
2. АГЕНТ (Go, бинарник cron-checker-agent)
================================================================

Запускается от root (нужно читать crontab всех пользователей).
Работает как HTTP-сервер (порт по умолчанию 9300, конфигурируется).

Эндпоинты агента:
- GET /healthz       -> 200 {"status":"ok"}
- GET /metrics       -> Prometheus text format (см. раздел 6)
- GET /api/v1/cron   -> JSON со всем, что нашёл (см. схему ниже)
  Требует Bearer-токен. /healthz и /metrics — публичные (или
  настраиваемо), чтобы Prometheus мог скрейпить напрямую.

Источники, которые агент ОБЯЗАН читать:
1. /var/spool/cron/crontabs/*   (Debian/Ubuntu, формат user: content)
2. /var/spool/cron/*            (RHEL-совместимые)
3. /etc/crontab
4. /etc/cron.d/*
5. /etc/cron.hourly/*, /etc/cron.daily/*, /etc/cron.weekly/*,
   /etc/cron.monthly/*   (просто имена файлов + user из stat)
6. systemd timers: вывод `systemctl list-timers --all --no-pager
   --output=json` + чтение unit-файлов из /etc/systemd/system,
   /usr/lib/systemd/system (только *.timer). Если systemctl
   недоступен — секция timers пустая, без падения.
7. crontab -l для каждого пользователя: обойти пользователей из
   /etc/passwd с UID >= 0 (root включительно), выполнить
   `crontab -l -u <user>`; ошибки «no crontab for user»
   игнорировать.

ВАЖНО: содержимое строк крона НЕ парсить в структуру
(минуты/часы/команда). Присылать «сырую строку как есть» + метаданные.

Схема ответа /api/v1/cron (JSON):
{
  "agent_version": "0.1.0",
  "hostname": "<uname -n>",
  "ip": "<первый non-loopback IPv4>",
  "collected_at": "RFC3339",
  "timezone": "Europe/Moscow",          // из /etc/timezone или TZ
  "entries": [
    {
      "source": "user_crontab",          // enum: user_crontab |
                                         // system_crontab | cron_d |
                                         // cron_hourly | cron_daily |
                                         // cron_weekly | cron_monthly |
                                         // systemd_timer
      "path": "/var/spool/cron/crontabs/alice",
      "user": "alice",                   // владелец/пользователь строки
      "raw_line": "*/5 * * * * /usr/local/bin/backup.sh",
      "line_no": 12,
      "schedule": "",                    // для systemd_timer — OnCalendar
      "enabled": true                    // для timer: active/inactive
    }
  ]
}

Требования к агенту:
- Конфиг через env + флаги: LISTEN_ADDR, CRON_AGENT_TOKEN,
  COLLECT_INTERVAL (по умолчанию 300s — как часто перечитывать
  локальные файлы; НЕ путать с опросом сервером), LOG_LEVEL.
- Сбор данных — по запросу (lazy) с кэшем на COLLECT_INTERVAL,
  чтобы 100 серверов не долбили агента каждые 5 минут.
- prometheus/client_golang: метрики cron_checker_agent_up,
  cron_checker_agent_last_collect_timestamp, cron_checker_agent_entries_total,
  cron_checker_agent_collect_errors_total, cron_checker_agent_http_requests_total.
- Логи — минимальные, structured (slog), без спама.
- Сборка: Makefile с таргетами build, build-linux-amd64,
  build-linux-arm64, package-deb, package-rpm (nfpm).
  systemd unit-файл cron-checker-agent.service в репозитории.
- Бинарник статический (CGO_ENABLED=0), один файл.

================================================================
3. СЕРВЕР (Go, единый бинарник + встроенная статика UI)
================================================================

Разворачивается через docker compose. Внутри: Go HTTP-сервер,
SQLite (файл в volume), встроенный (embed) фронтенд.

Технологии:
- Go 1.22+, net/http + chi (или stdlib mux, на твой выбор).
- БД: SQLite через modernc.org/sqlite (без CGO — проще сборка
  в docker) + миграции через golang-migrate или простые SQL-миграции
  встроенные в бинарник.
- Фронтенд: ЛЁГКИЙ стек. Предпочтительно — Go templates (html/template)
  + HTMX + минимальный CSS (свой, тёмная тема, без Tailwind CDN).
  Без React/Vue/Node-сборки. Всё встраивается через embed.
- Конфиг через env: SERVER_LISTEN (default :8090), DB_PATH,
  CRON_SERVER_TOKEN, POLL_INTERVAL (default 300s),
  POLL_CONCURRENCY (default 10), POLL_TIMEOUT (default 15s),
  LOG_LEVEL.

Схема БД (минимум):
- agents(id PK, hostname, ip, agent_version, timezone,
         last_seen_at, status TEXT CHECK(online|offline),
         first_seen_at, UNIQUE(hostname, ip))
- cron_entries(id PK, agent_id FK, source, path, user, raw_line,
               line_no, schedule, enabled,
               first_seen_at, last_seen_at,
               status TEXT CHECK(active|removed|acked),
               fingerprint TEXT UNIQUE(agent_id, fingerprint))
    fingerprint = sha256(source + path + user + raw_line + line_no)
- cron_snapshots(id PK, agent_id, collected_at, payload JSON)
    // сырой JSON последнего успешного опроса — для diff/аудита
    хранится N дней (env SNAPSHOT_RETENTION_DAYS, default 7)

Логика опроса (background goroutine в сервере):
- Каждые POLL_INTERVAL секунд опросить всех агентов из таблицы
  agents параллельно (семафор на POLL_CONCURRENCY).
- Успех: обновить last_seen_at, status=online; upsert entries по
  fingerprint; строки, которых больше нет — пометить status=removed,
  last_seen_at не трогать.
- «Новая строка» = запись с first_seen_at в пределах 24ч и
  status=active и не acked. В UI показывается бейдж NEW.
- Ошибка сети/timeout: status=offline, entries НЕ трогаем.
- Очистка: удалять snapshots старше SNAPSHOT_RETENTION_DAYS,
  entries со status=removed старше 24ч (настраивается).

HTTP API сервера:
- GET  /                          -> UI (главная: список машин)
- GET  /agents/{id}               -> UI: крон конкретной машины
- GET  /api/v1/agents             -> JSON список агентов
- GET  /api/v1/agents/{id}/cron   -> JSON entries
- GET  /api/v1/agents/{id}/history-> JSON snapshots (последние N)
- POST /api/v1/entries/{id}/ack   -> «закрыть» новую строку
- GET  /api/v1/search?q=          -> поиск по raw_line/user/hostname
- GET  /healthz                   -> {"status":"ok"}
- GET  /metrics                   -> Prometheus (см. раздел 6)

Опционально (но желательно): POST /api/v1/agents (регистрация
агента админом вручную: hostname + ip), и UI-форма для добавления
машины. Автообнаружения агентов НЕТ — сервер опрашивает только
тех, кого добавили.

================================================================
4. WEB UI (требования)
================================================================

- Тёмная тема, минимализм, моноширинный шрифт для строк крона.
- Главный экран: таблица агентов
  Колонки: Hostname | IP | Status (🟢 online / 🔴 offline) |
           Last seen | # entries | # new (за 24ч) | Timezone
  Клик по строке -> детальная страница машины.
- Детальная страница машины: список всех entries, сгруппированных
  по source (user crontab / system / cron.d / cron.* / systemd timers).
  Для каждой строки: user, raw_line (моноширинно), бейдж NEW если
  first_seen_at < 24ч и не acked, кнопка Ack. Кнопка «Показать
  историю (diff)» — сравнение с предыдущим snapshot.
- Поиск: одна строка сверху, ищет по hostname/ip/user/raw_line,
  результаты — плоский список с подсветкой совпадений.
- Фильтры: по source, по user, «только новые».
- Язык интерфейса: АНГЛИЙСКИЙ.
- Никаких внешних CDN. HTMX и CSS — локально, embed в бинарник.
- Время в UI показывать в TZ машины-агента (поле timezone);
  на сервере хранить UTC.

================================================================
5. DOCKER COMPOSE (только сервер)
================================================================

- Один сервис `cron-checker` (multi-stage Dockerfile: builder ->
  distroless/scratch). Непривилегированный пользователь внутри.
- Volume `cron_checker_data:/data` для SQLite.
- Порт 8090 наружу.
- env_file: .env с CRON_SERVER_TOKEN, POLL_INTERVAL и т.д.
- healthcheck на /healthz.
- docker-compose.yml + .env.example + README с инструкцией запуска + docker image

================================================================
6. PROMETHEUS МЕТРИКИ (сервер и агент)
================================================================

Общие требования: exposition format, без кардинальности по
raw_line. Все метрики с префиксом.

Сервер (/metrics):
- cron_checker_agents_total{status="online|offline"}
- cron_checker_agent_up{hostname,ip}   (1/0)
- cron_checker_agent_last_seen_timestamp{hostname,ip}
- cron_checker_entries_total{hostname,source,status}
- cron_checker_new_entries_24h{hostname}   (gauge — сколько
  записей с first_seen_at за 24ч и не acked)
- cron_checker_poll_duration_seconds (histogram)
- cron_checker_poll_errors_total{hostname}

Семантика «новой строки» и ack:
- Запись считается новой, если first_seen_at >= now-24h
  И status='active' И acked_at IS NULL.
- В таблицу entries добавить колонку acked_at TIMESTAMP NULL.
- POST /api/v1/entries/{id}/ack проставляет acked_at=now().
- Метрика cron_checker_new_entries_24h уменьшается после ack
  и/или по истечении 24ч — без отдельного крон-джоба, считать
  на лету в handler метрик.
- Аналог на агенте: cron_checker_agent_entries_total{source},
  cron_checker_agent_up, cron_checker_agent_last_collect_timestamp.

================================================================
7. СТРУКТУРА РЕПОЗИТОРИЯ (обязательно)
================================================================

cron-checker/
├── cmd/
│   ├── server/main.go
│   └── agent/main.go
├── internal/
│   ├── agent/        (сбор cron, systemd, http handlers)
│   ├── server/       (http, poller, handlers, templates)
│   ├── store/        (sqlite, migrations)
│   ├── model/        (общие типы: CronEntry, AgentReport)
│   └── metrics/      (prometheus)
├── web/              (templates/, static/, embed.go)
├── deploy/
│   ├── Dockerfile
│   ├── docker-compose.yml
│   ├── cron-checker-agent.service
│   └── .env.example
├── Makefile
├── go.mod
└── README.md

================================================================
8. КРИТЕРИИ ПРИЁМКИ
================================================================

1. `docker compose up -d` поднимает сервер, /healthz=200,
   /metrics отдаёт метрики, UI открывается на :8090.
2. `make build` собирает оба бинарника без CGO.
3. Агент на чистой Debian/Ubuntu/redos: `./cron-checker-agent` стартует,
   /healthz=200, /api/v1/cron с Bearer-токеном возвращает
   валидный JSON со всеми источниками (что есть в системе).
4. Сервер, добавленный в БД агент (hostname+ip), через
   POLL_INTERVAL опрашивает его и отображает в UI.
5. Если агент выключить — через 2 цикла статус offline в UI
   и cron_checker_agent_up=0 в /metrics.
6. Новая строка в crontab на агенте -> появляется в UI с бейджем
   NEW и растёт cron_checker_new_entries_24h. После ack бейдж
   исчезает, метрика уменьшается.
7. Удалённая строка -> status=removed, через 24ч чистится.
8. README описывает: запуск сервера, установку агента (.deb/.rpm/
   бинарь), все env-переменные, пример Prometheus scrape config.
9. Юнит-тесты на: парсинг crontab-файлов, fingerprint, diff
   snapshots, ack-логику. Покрытие — разумное, не гоняться за 100%.

================================================================
9. НЕ ДЕЛАЙ ТАК
================================================================

- Не парсить поля cron в структуру — только raw_line.
- Не использовать Redis/Postgres/RabbitMQ/Kafka.
- Не использовать Node/React/Vue/Tailwind CDN — только embed
  шаблоны + HTMX + свой CSS.
- Не делать auto-discovery агентов — только ручное добавление.
- Не ставить агента в docker — только бинарник/systemd.
- Не логировать сырые строки крона на уровне INFO (могут быть
  секреты) — только на DEBUG.
- Не хранить токен в коде/репозитории — только env.
- Не делать ретраи-штормы: один опрос = одна попытка.
- Не тянуть тяжёлые зависимости (ORM, DI-фреймворки).
- Не забывать: сервер хранит время в UTC, UI отображает в TZ агента.

10. Для агента, сразу сделай под RedOS systemd сервис.
