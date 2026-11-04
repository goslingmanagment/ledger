# ledger

Финансовая база агентства: журнал двойной записи, сбор Bybit и карт, ночные брифы
владельцу, SQL для агентов. Go, SQLite с Litestream, прод на `agency-de`.
ТЗ: `docs/specs/2026-11-04-ledger-design.md`, план выпуска 1: `docs/plans/2026-11-04-release-1.md`.

## Запуск и проверки

- Локально: `LEDGER_DB=./var/dev.db LEDGER_SCHEDULER=off LEDGER_ADDR=127.0.0.1:8080 go run ./cmd/ledger serve`,
  затем `go run ./cmd/ledger probe --url http://127.0.0.1:8080/health --print`. `ledger help` — все команды.
- Перед PR: `gofmt -l .`, `go vet ./...` и `go test` затронутых пакетов; остальное проверяет CI.
- Устройство: `cmd/ledger` — один бинарник; `internal/cli` — подкоманды (каждая фича своим файлом через
  `cli.Command` в `init`); `internal/server` — HTTP и планировщик (`server.Register`, `server.Every`);
  `internal/db` — SQLite (modernc, без cgo) и миграции `internal/db/migrations/NNNN_*.sql`, только вперёд.

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
