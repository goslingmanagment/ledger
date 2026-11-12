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
  Деньги одного человека для него самого («Мои смены» хаба) — `GET /v1/payouts/person?person=код&from&to`, только право `payouts`
  (ключ `read` — 403): дни книг с постами отчёта, что оплачено какой строкой журнала, ручные поправки, выплаты, долг по `v_payables`;
  ни ставок, ни выручки, ни «К выплате» вкладки, ни чужих строк.
  Выплата, шаг 1 (скилл chatter-payout): `ledger payout prepare <код> [--json]` — сумма по `v_payables`, строки по неделям,
  блоки к отметке, флаги и текст работнику с `{перевод}`; ничего не пишет и не отправляет.
  Переключение на факты (L4): с `settings.payroll_facts_from` (`ledger payroll facts-from ГГГГ-ММ-ДД` рядом с базой, откат — `--off`)
  дни начисляет `fact_accruals` на вкладку человека модели (source_key с `:facts`, `payable_days.source`), раньше — таблица как была;
  таблица после даты — зеркало (расхождение — сведения в health и payables), день без доказанных фактов стоит как проведён и
  «неизвестно» в «не занесено» (reason «факты хаба не начислили: …»).
- Отчёты смен (R1, план `docs/plans/2026-11-10-shift-reports.md`): `ledger report save <файл|->` — агент команды сохраняет
  расклад отчёта (`POST /v1/reports`, ключ с правом `reports`: только эта запись, сочетается с другими правами); суммы в mills,
  страницы сходятся с итогом, включённые строки и добавки — со страницей, до тысячной. Тот же JSON новой версии не создаёт,
  иной — версия N+1, прежняя `superseded`. `ledger report list [--from] [--to] [--person] [--json]` (по умолчанию 14 дней),
  `ledger report show <id> [--json]`; хаб читает `GET /v1/reports?from&to[&hub_user_id][&hub_username]` ключом `payouts`
  (только отчёты этого пользователя или его карточки). Таблицы `shift_reports*`, вид `v_shift_reports`. Этап ничего не проводит и не начисляет.
- Посты «Отчётности» (R2a): `ledger collect reports [--dry-run] [--local-db] [--since]` на маке (конфиг `~/.config/ledger/reports.json`:
  `topics` [{peer, topic}], `since_days`, `tg_dir`; очередь `spool-reports`, только `tg history`, launchd раз в 15 минут) шлёт сообщения
  с «Общий доход» событиями `tg_reports`; правка поста — новое событие. Сервер разбирает их в `shift_report_posts*` при приходе и джобом,
  сейчас — `ledger report parse`. Настройки (seed `ledger import business`, значения — JSON-строки): `report_topics` — топик, его модель
  и `pages` {имя в посте строчными: страница хаба}; `report_authors` — {"tg:<id>": человек, "#тег": человек}, отправитель важнее тега.
  База оплаты по страницам — вид `v_report_pages` (пост против черновика: `posted_equal|posted_changed|post_only|draft_only`), глазами —
  `ledger report days`, пометки постов — `ledger report posts --flagged` и проверка «отчёты разобраны». Хабу — `GET /v1/report-days`
  (`read`, `payouts`), текст постов — `GET /v1/report-posts` (только `read`). Ничего ещё не начисляется: это этап R2b.
- Начисления по отчётам (R2): `report_accruals` — день человека модели = сумма его страниц поста за дату отчёта × «Ставка %» вкладки
  плюс «Фикс/см» раз за день (без вкладки, без ставки, страница без модели — «без ставки», не начислено). Пересчёт после разбора постов,
  сохранения черновика и перед каждой пересборкой долгов; тень, пока выключатель пуст. Сравнение трёх источников — `ledger accruals diff`
  (таблица, отчёты, факты; разница меньше доллара на целой ячейке таблицы — «округление таблицы», в итог не идёт).
  Выключатель — `ledger payroll reports-from ГГГГ-ММ-ДД` рядом с базой (без аргумента — показать, `--off` — назад к таблицам), включает
  только владелец; вместе с `payroll_facts_from` не ставится. С даты дни — из отчётов (`:reports`, `payable_days.source = reports`),
  таблица — зеркало. День держится как проведён и стоит в «не занесено» unknown («по отчёту не начислено: …»), если пост дня с блокирующей
  пометкой или непонятой страницей, у вкладки нет ставки, либо в таблице или книгах день есть, а опубликованного отчёта нет; смена хаба без
  отчёта — тоже «не занесено» («смена без отчёта»). Выплаченный день (`payable_days.paid > 0`) сам не меняется: изменение ждёт в
  `payroll_pending_changes` (новое того же дня заменяет сумму), решение — `ledger payroll change apply|dismiss <id> [--note]` (право
  decide, `POST /v1/payroll/changes/{id}/apply|dismiss`); `--off` закрывает открытые. Проверки: «у смен есть отчёты» (смены 14 дней,
  кончились больше суток назад; дата отчёта смены — `payroll.ReportDate`) и «изменения за выплаченные дни ждут решения».
- Договорённости «чья продажа» (R3): `sale_decisions` — решение владельца или договорённость чаттеров о продаже (или ожидаемой, без
  `--tx`): `owner`, `split` (доли в сумме 100), `pot`, `off_platform`, `exclude`; кто сказал, когда, ссылка, заметка. Пишет агент ключом
  `reports` или владелец (`decide`): `ledger decision add …` (или `--json` из stdin), `ledger decision applied <id> --person --date`,
  `ledger decision cancel <id> --note`; второе решение по той же продаже — только с `--supersedes <id>` открытого. Читают `read`, `payouts`,
  `reports`: `ledger decision list [--open] [--fan] [--person] [--from --to] [--json] [--md]`; `--md` — открытые списком, как файл памяти
  агента (файл — зеркало). У строк `GET /v1/reports` — поле `agreement`. Ничего не проводит.
  Открытые вопросы «чья продажа» (кого спросили: `owner` или `chatters`) — `sale_questions`, те же права: `ledger question add …`,
  `ledger question answer <id> [--decision <id>] [--answer "…"]`, `ledger question drop <id> --note`, `ledger question list [--open]
  [--asked-of] [--fan] [--json] [--md]` (`--md` — «Ждёт Диму» и «Решают сами чаттеры»). Решение с `--answers <id>` закрывает открытый вопрос
  в той же транзакции; закрытый или несуществующий вопрос — отказ, решение не пишется.
- Общаки (R3): объявляет и меняет только владелец — `ledger pot declare <файл|-|JSON>` (право decide или рядом с базой; тот же slug —
  правка); покупки пишет агент (`reports`) или владелец: `ledger pot add-sale <код> --tx <ref>` (страница, фан, нетто и время — из
  `hub_sales`, иначе `--page --net --at`) или `--from-line <отчёт>:<n>`, `ledger pot remove-sale <id> --note` (строка остаётся);
  `ledger pot list|show <код>|candidates [<код>]` (строки отчётов не в итоге с пометкой «общак»). Доля (`v_pot_shares`) — нетто поровну
  участникам на дату покупки (МСК), остаток микродолларов — по одному по порядку кода. Доля идёт в выручку дня человека на вкладке модели
  общака (`report_accruals.pot_micros`, `pots`) и платится по его ставке; день из одной доли фикса не получает. Проводится только через
  выключатель `payroll_reports_from`; продажу, учтённую в отчёте чаттера, `add-sale` не берёт без `--force`.
- Зеркало таблиц выплат (R2): `ledger mirror sheets [--apply] [--model] [--person] [--from ГГГГ-ММ-ДД] [--yes-many] [--local-db] [--json]`
  на маке — с `payroll_reports_from` пишет `posted_micros` страниц из `v_report_pages` в ячейки дней вкладки человека (точная сумма, $0 — `0`,
  страница без поста не трогается); база — `GET /v1/sheets-mirror` ключом сборщика. По умолчанию только план (вкладка, ячейка, было → станет,
  что не трогается и почему); без выключателя пишет одну строку и выходит с 0, план до него — `--from`. Никогда не трогает шапку, формулы,
  строки не-страниц (имя строки — как у читателя: метка хаба или `settings.sheet_pages`), «Выплачено», выплаченные недели (только сообщает),
  несопоставленные вкладки и дни до старта; строк, колонок и блоков не создаёт. Больше 200 ячеек — только с `--yes-many`; после записи
  перечитывает вкладку, журнал — `~/.config/ledger/sheets-mirror.jsonl`. Задание launchd ставится, только если есть `~/.config/ledger/sheets-mirror.on`.
  Проверка «у завтрашней даты есть колонка во вкладке» (сведения, не сбой; список в details): вкладки активных людей, где за 14 дней
  был день таблицы или отчёта, без колонки завтрашнего дня МСК — блок недели добавляет человек.
- Бриф: `ledger brief [--text] [--new]` — открытый неотправленный или новый из открытых предложений, номера заморожены; `ledger brief-sent --brief N`, `ledger pending`.
  Решения: `ledger decide --brief N --set 1=confirm --set 2=personal --set 3=custom:{…} --set 4=defer --set 5=recorded:388,389`, весь набор одной транзакцией, ключ с правом decide.
  Один пункт без брифа, когда владелец сказал «да» в чате сессии: `ledger decide --proposal N --set confirm|personal|custom:{…}|recorded:388`
  (номер — `proposals.id`, только последняя версия, без defer); вопрос о нём в брифе отвечается тем же решением, кто решил — имя ключа.
  `mode` shadow: решения только записываются, закрывает их импорт журнала или `import bybit-state`; live: `confirm` и `custom` (JSON-правка: `category`, `counterparty`, `model`, `note`, `type`, `amount`, `split`) пишут строки и проводки, точное правило пишет само.
  `ledger propose <json>`, `ledger evidence add|show`, `ledger dispute open|list|resolve` — у споров исправление только в live; `ledger rule add` и `ledger note add` запоминают ответ.
- Карты: `ledger collect cards [--dry-run] [--local-db]` на маке (конфиг `~/.config/ledger/cards.json`, очередь `spool-cards`, только `tg history`); `ledger cards balances` — журнал против анкоров бота, `ledger cards link <msg_id> <withdrawal:N>`.
  На сервере события разбирает `internal/cards` при приходе и джобом: загрузка с даты `settings.cards_load_from` (по умолчанию 2026-09-05, раньше журнал писал дошедшее до карты переводом) — проводка кошелёк → карта. Ответ владельца без строки из кошелька сторнирует её: так бот показывает зачисления саппорта и переносы между картами.
- Параметры владельца — в seed `settings` (`ledger import business` на сервере). `owner_salary_rate`: число или JSON `{"ГГГГ-ММ-ДД": ставка}` с днями,
  с которых ставка действует; месяц судится по ставке своего первого дня, `ledger free` — по сегодняшней. Повышение, принятое до закрытия, —
  новая строка с датой: карточка, бриф и закрытие пишут «повышение уже принято» и вопроса не задают. `category_caps`: `{"категория расходов": лимит в месяц}` —
  строка «Лимиты» в `ledger month brief`. Обязательство с полем `account` (`card:1234`) в `commitments.json`: проверка «карты покрывают списания»
  предупреждает за 3 дня, если на счёте меньше ближайших списаний (сегодняшние не считает: они могли уже пройти).
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
