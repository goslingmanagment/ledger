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
  Факты смен и продаж (выпуск 2, ключу нужны `read:team` и `read:money`): джоб `hub-team` раз в час, вручную —
  `ledger hub team-sync [--from ГГГГ-ММ-ДД] [--to …]`; окна по неделям МСК — в `ledger hub status`, поле `team`. Читаются с
  `settings.hub_team_since` (первый день работ людей в хабе, раньше правил): недели до правил — только для «не занесено».
  Начисления из фактов в тени (ничего не проводят) пересчитываются после каждого чтения: `fact_homes`, `fact_accruals`,
  `v_accrual_diff`; было — стало — `ledger accruals diff [--week понедельник]` (API `GET /v1/accruals/diff?week=`).
  Человек хаба — по `people.aliases` (имя хаба или `hub:<id>`), ставка — шапка его вкладки модели («Ставка %», «Фикс/см»).
  Ничья продажа, которую человек заявил, по слову владельца: `ledger accruals claim <transactionRef> --person <код> --note "…"`,
  снять — `ledger accruals unclaim <transactionRef> --note "…"` (ключ с правом decide или рядом с базой); не ничью продажу отказывает,
  действующие заявки и учтены ли — `v_sale_claims`.
- Bybit локально: `LEDGER_BYBIT_API_KEY=… LEDGER_BYBIT_API_SECRET=… ledger bybit fetch --since ГГГГ-ММ-ДД`; статус прогона и
  Funding — `ledger bybit status`, без строки журнала — `ledger bybit unlinked`. Перенос из `ledger-sync`: `ledger rules import <config>`,
  затем `ledger import bybit-state <bybit.sqlite>`; после первого прогона сбора, тогда у событий истории есть сырая запись Bybit.
  Настройки `mode` (shadow: документы не создаются, live: `ledger mode live`) и `bybit_start`.
  Счета монет (`bybit:btc`) — по средней цене покупки: `ledger bybit coin-cost` показывает цену и поправки платежей монетой,
  `--from ГГГГ-ММ-ДД --apply` (рядом с базой) включает их с этой даты (настройка `coin_cost_from`) и проводит; дальше их ведёт сбор.
  Монета без строк Funding заводится строкой `transfer`/«Остаток на старте» с «Кому/Откуда» `SOL (Bybit)`: счёт `bybit:sol`
  откроется сам, проверка сверяет количество с показанием биржи до проводки. Метод монеты — `SOL (Bybit)[ → …]`, иной метод с Bybit — ошибка.
- Долги людям: `ledger import people <папка people>`, затем `ledger collect sheets [--dry-run] [--local-db]` (конфиг `~/.config/ledger/sheets.json`,
  очередь `~/.config/ledger/spool`, только `gog --readonly`); `ledger payroll open --dry-run` — стартовые долги таблицей, без `--dry-run` они замораживаются
  навсегда; `ledger payroll rebuild` — свести сейчас, `ledger payables` — что вышло. Строки вкладок, которые не пишутся как страница хаба, — в `settings.sheet_pages`.
  Хабу («Выплаты» Workboard) — `GET /v1/payouts?from&to` (до 62 дней) ключом с правом `payouts`: он открывает только его и
  `GET /v1/payouts/prepare?person=код`; то же глазами — `ledger payouts --from ГГГГ-ММ-ДД --to … [--person код]`.
  Выплата, шаг 1 (скилл chatter-payout): `ledger payout prepare <код> [--json]` — сумма по `v_payables`, строки по неделям,
  блоки к отметке, флаги и текст работнику с `{перевод}`; ничего не пишет и не отправляет.
- Бриф: `ledger brief [--text] [--new]` — открытый неотправленный или новый из открытых предложений, номера заморожены; `ledger brief-sent --brief N`, `ledger pending`.
  Решения: `ledger decide --brief N --set 1=confirm --set 2=personal --set 3=custom:{…} --set 4=defer --set 5=recorded:388,389`, весь набор одной транзакцией, ключ с правом decide.
  `mode` shadow: решения только записываются, закрывает их импорт журнала или `import bybit-state`; live: `confirm` и `custom` (JSON-правка: `category`, `counterparty`, `model`, `note`, `type`, `amount`, `split`) пишут строки и проводки, точное правило пишет само.
  `ledger propose <json>`, `ledger evidence add|show`, `ledger dispute open|list|resolve` — у споров исправление только в live; `ledger rule add` и `ledger note add` запоминают ответ.
- Карты: `ledger collect cards [--dry-run] [--local-db]` на маке (конфиг `~/.config/ledger/cards.json`, очередь `spool-cards`, только `tg history`); `ledger cards balances` — журнал против анкоров бота, `ledger cards link <msg_id> <withdrawal:N>`.
  На сервере события разбирает `internal/cards` при приходе и джобом: загрузка с даты `settings.cards_load_from` (по умолчанию 2026-09-05, раньше журнал писал дошедшее до карты переводом) — проводка кошелёк → карта. Ответ владельца без строки из кошелька сторнирует её: так бот показывает зачисления саппорта и переносы между картами.
- Перед PR: `gofmt -l .`, `go vet ./...` и `go test` затронутых пакетов; остальное проверяет CI.
- Устройство: `cmd/ledger` — один бинарник; `internal/cli` — подкоманды (каждая фича своим файлом через
  `cli.Command` в `init`); `internal/server` — HTTP и планировщик (`server.Register`, `server.Every`);
  `internal/db` — SQLite (modernc, без cgo) и миграции `internal/db/migrations/NNNN_*.sql`, только вперёд;
  `internal/auth` — ключи агентов и `auth.Require(s, scope, h)` для маршрутов; `internal/client` — HTTP-клиент CLI;
  `internal/agentsql` — `POST /v1/sql` и `GET /v1/schema`; `internal/split` — чья продажа хаба (порт hub `sale-rule.ts`),
  чистая логика: дата и ожидание в `split.Default` и в строках `split_rules` меняются вместе.
- Новая таблица или колонка получает русское описание в `schema_docs` той же миграцией: агенты пишут запросы по `ledger schema`.
- `internal/payroll/parse.go` — порт `payroll.py` до бита: свой `pySum` (встроенный `sum()` Питона считает с компенсацией) и `float64(x*y)` перед сложением
  (на arm64 Go иначе склеит округление). Эталон — `internal/payroll/testdata/golden.json`, генератор рядом.
- modernc прерывает запрос по контексту только на первом шаге; таймаут SQL агентов держит свой сторож в `agentsql`
  (через поле драйвера). После обновления modernc — `go test ./internal/agentsql`.

## Выкатка

- `bash deploy/release.sh` с мака после зелёного main (образ из CI → VPS → `deploy.sh`, копии до и после). С 04.11 CI снова на
  GitHub-hosted (переменной `CI_POOL` нет, скилл ci-pool). Если CI вернётся на мак (`CI_POOL=pc`), артефакта образа нет: `--build-here`,
  зелёный CI проверяется, образ собирается на VPS.
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
