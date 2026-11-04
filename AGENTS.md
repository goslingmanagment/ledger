# ledger

Финансовая база агентства: журнал двойной записи, сбор Bybit и карт, ночные брифы
владельцу, SQL для агентов. Go, SQLite с Litestream, прод на `agency-de`.
ТЗ: `docs/specs/2026-11-04-ledger-design.md`, план выпуска 1: `docs/plans/2026-11-04-release-1.md`.

## Запуск и проверки

- Локально: `LEDGER_DB=./var/dev.db LEDGER_SCHEDULER=off LEDGER_ADDR=127.0.0.1:8080 go run ./cmd/ledger serve`,
  затем `go run ./cmd/ledger probe --url http://127.0.0.1:8080/health --print`. `ledger help` — все команды.
- SQL агентов локально: `LEDGER_DB=./var/dev.db go run ./cmd/ledger keys create dev --scope read`, затем
  `LEDGER_URL=http://127.0.0.1:8080 LEDGER_KEY=<ключ> go run ./cmd/ledger sql "SELECT * FROM v_balances"` и `ledger schema`.
- Хаб локально: `ledger import business <seed.json>` и `ledger import hub-exceptions <файл>`, затем
  `LEDGER_HUB_AGENT_KEY=<ключ> ledger hub sync`; что доказано — `ledger hub status`, оборот — `ledger hub net --month ГГГГ-ММ`.
  Лимит запросов к датасетам (`hub_rpm`) у хаба на адрес: `hub sync` и `month.py` с одного мака не запускать одновременно.
- Bybit локально: `LEDGER_BYBIT_API_KEY=… LEDGER_BYBIT_API_SECRET=… ledger bybit fetch --since ГГГГ-ММ-ДД`; статус прогона и
  Funding — `ledger bybit status`, без строки журнала — `ledger bybit unlinked`. Перенос из `ledger-sync`: `ledger rules import <config>`,
  затем `ledger import bybit-state <bybit.sqlite>`; после первого прогона сбора, тогда у событий истории есть сырая запись Bybit.
  Настройки `mode` (shadow: документы не создаются) и `bybit_start`.
- Долги людям: `ledger import people <папка people>`, затем `ledger collect sheets [--dry-run] [--local-db]` (конфиг `~/.config/ledger/sheets.json`,
  очередь `~/.config/ledger/spool`, только `gog --readonly`); `ledger payroll open --dry-run` — стартовые долги таблицей, без `--dry-run` они замораживаются
  навсегда; `ledger payroll rebuild` — свести сейчас, `ledger payables` — что вышло. Строки вкладок, которые не пишутся как страница хаба, — в `settings.sheet_pages`.
- Перед PR: `gofmt -l .`, `go vet ./...` и `go test` затронутых пакетов; остальное проверяет CI.
- Устройство: `cmd/ledger` — один бинарник; `internal/cli` — подкоманды (каждая фича своим файлом через
  `cli.Command` в `init`); `internal/server` — HTTP и планировщик (`server.Register`, `server.Every`);
  `internal/db` — SQLite (modernc, без cgo) и миграции `internal/db/migrations/NNNN_*.sql`, только вперёд;
  `internal/auth` — ключи агентов и `auth.Require(s, scope, h)` для маршрутов; `internal/client` — HTTP-клиент CLI;
  `internal/agentsql` — `POST /v1/sql` и `GET /v1/schema`.
- Новая таблица или колонка получает русское описание в `schema_docs` той же миграцией: агенты пишут запросы по `ledger schema`.
- `internal/payroll/parse.go` — порт `payroll.py` до бита: свой `pySum` (встроенный `sum()` Питона считает с компенсацией) и `float64(x*y)` перед сложением
  (на arm64 Go иначе склеит округление). Эталон — `internal/payroll/testdata/golden.json`, генератор рядом.
- modernc прерывает запрос по контексту только на первом шаге; таймаут SQL агентов держит свой сторож в `agentsql`
  (через поле драйвера). После обновления modernc — `go test ./internal/agentsql`.

## Выкатка

- `bash deploy/release.sh` с мака после зелёного main (образ из CI → VPS → `deploy.sh`, копии до и после).
  Первая установка: `--bootstrap`. Откат: `deploy/rollback.sh` на VPS. Подробно — `deploy/README.md`.
- Прод: `agency-de`, checkout `/opt/ledger`, том `ledger_data`, порт `127.0.0.1:3300`, сайт
  `ledger.gosling-agency.ru`. Копии базы — `~/Backups/ledger/` на маке каждые 6 часов.

## Опасно

- Репозиторий публичный. Реальных имён, сумм, адресов кошельков, ставок и
  контрагентов нет ни в коде, ни в тестах, ни в документах, коммитах и PR:
  только синтетика. Настоящие данные живут в базе на сервере.
- ledger учитывает деньги и никогда их не двигает; людям не пишет.
- Проводки в проде не правятся и не удаляются: исправление делается сторно и новой
  проводкой через спор.
- Секреты хранятся только в `/etc/ledger/ledger.env` на сервере и `~/.config/ledger/` на
  машинах агентов. Не печатать их в журнал и ответы.
