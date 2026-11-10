-- Договорённости и общаки, этап R3 (план docs/plans/2026-11-10-shift-reports.md): решения «чья продажа» и общаки
-- владельца — записи в ledger, а не абзацы в файлах памяти агента. Доля общака идёт в базу продаж человека и платится
-- по его ставке (report_accruals: страницы поста плюс доли общаков за день). Пока settings.payroll_reports_from пуст —
-- тень: ни одной проводки эта миграция не пишет. Деньги — целые микродоллары, времена — МСК. Таблица deals не трогается.

CREATE TABLE sale_decisions (
  id                  INTEGER PRIMARY KEY,
  page_label          TEXT NOT NULL,
  fan_ref             TEXT NOT NULL DEFAULT '',
  fan_name            TEXT NOT NULL DEFAULT '',
  transaction_ref     TEXT,
  net_micros          INTEGER,
  occurred_at         TEXT,
  kind                TEXT NOT NULL CHECK (kind IN ('owner','split','pot','off_platform','exclude')),
  shares              TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(shares)),
  pot                 TEXT,
  said_by             TEXT NOT NULL,
  said_at             TEXT NOT NULL,
  source_link         TEXT NOT NULL DEFAULT '',
  note                TEXT NOT NULL DEFAULT '',
  status              TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','applied','cancelled')),
  applied_report_date TEXT,
  applied_person      TEXT,
  applied_at          TEXT,
  cancelled_at        TEXT,
  cancel_note         TEXT,
  supersedes          INTEGER REFERENCES sale_decisions(id),
  created_at          TEXT NOT NULL,
  key_name            TEXT NOT NULL
);
CREATE UNIQUE INDEX sale_decisions_open_ref ON sale_decisions(transaction_ref) WHERE status = 'open' AND transaction_ref IS NOT NULL;
CREATE INDEX sale_decisions_ref ON sale_decisions(transaction_ref);
CREATE INDEX sale_decisions_status ON sale_decisions(status, id);

-- pots из первой миграции пуст: дописываем, чего не хватает, ничего не удаляя.
ALTER TABLE pots ADD COLUMN slug TEXT;
ALTER TABLE pots ADD COLUMN model TEXT NOT NULL DEFAULT '';
ALTER TABLE pots ADD COLUMN pages TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(pages));
ALTER TABLE pots ADD COLUMN starts_at TEXT;
ALTER TABLE pots ADD COLUMN ends_at TEXT;
ALTER TABLE pots ADD COLUMN recognise TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(recognise));
ALTER TABLE pots ADD COLUMN declared_by TEXT NOT NULL DEFAULT '';
ALTER TABLE pots ADD COLUMN declared_at TEXT;
ALTER TABLE pots ADD COLUMN note TEXT NOT NULL DEFAULT '';
ALTER TABLE pots ADD COLUMN updated_at TEXT;
ALTER TABLE pots ADD COLUMN key_name TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX pots_slug ON pots(slug) WHERE slug IS NOT NULL;

CREATE TABLE pot_sales (
  id              INTEGER PRIMARY KEY,
  pot             TEXT NOT NULL,
  transaction_ref TEXT,
  page_label      TEXT NOT NULL,
  fan_ref         TEXT NOT NULL DEFAULT '',
  fan_name        TEXT NOT NULL DEFAULT '',
  net_micros      INTEGER NOT NULL CHECK (net_micros > 0),
  occurred_at     TEXT NOT NULL,
  day             TEXT NOT NULL,
  source          TEXT NOT NULL CHECK (source IN ('agent','report_line','owner')),
  report_id       INTEGER,
  report_n        INTEGER,
  note            TEXT NOT NULL DEFAULT '',
  added_at        TEXT NOT NULL,
  key_name        TEXT NOT NULL,
  removed_at      TEXT,
  removed_note    TEXT,
  removed_by      TEXT
);
CREATE UNIQUE INDEX pot_sales_ref ON pot_sales(transaction_ref) WHERE transaction_ref IS NOT NULL AND removed_at IS NULL;
CREATE INDEX pot_sales_pot ON pot_sales(pot, day);

-- Доли: покупка × участник общака на её дату. Нетто делится поровну; неделимый остаток (микродоллары) — по одному
-- участникам по порядку кода, так что доли в сумме дают нетто точно. Покупка в день без участников — строка без доли.
CREATE VIEW v_pot_shares AS
  WITH s AS (
    SELECT ps.id AS sale_id, ps.pot AS pot, p.name AS pot_name, p.model AS model, ps.transaction_ref AS transaction_ref,
           ps.page_label AS page_label, ps.fan_ref AS fan_ref, ps.fan_name AS fan_name, ps.occurred_at AS occurred_at,
           ps.day AS day, ps.net_micros AS net_micros, p.participants AS participants
    FROM pot_sales ps JOIN pots p ON p.slug = ps.pot
    WHERE ps.removed_at IS NULL
  ),
  part AS (
    SELECT s.sale_id AS sale_id, json_extract(j.value, '$.person') AS person
    FROM s, json_each(s.participants) j
    WHERE json_extract(j.value, '$.from') <= s.day
      AND (json_extract(j.value, '$.to') IS NULL OR json_extract(j.value, '$.to') >= s.day)
  ),
  ranked AS (
    SELECT sale_id, person, ROW_NUMBER() OVER (PARTITION BY sale_id ORDER BY person) AS rn,
           COUNT(*) OVER (PARTITION BY sale_id) AS n
    FROM part
  )
  SELECT s.sale_id AS sale_id, s.pot AS pot, s.pot_name AS pot_name, s.model AS model, s.transaction_ref AS transaction_ref,
         s.page_label AS page_label, s.fan_ref AS fan_ref, s.fan_name AS fan_name, s.occurred_at AS occurred_at, s.day AS day,
         s.net_micros AS net_micros, COALESCE(r.n, 0) AS participants, r.person AS person,
         CASE WHEN r.person IS NULL THEN NULL
              ELSE s.net_micros / r.n + CASE WHEN r.rn <= s.net_micros % r.n THEN 1 ELSE 0 END END AS share_micros,
         CASE WHEN r.person IS NULL THEN 'нет участников на дату покупки' END AS flag
  FROM s LEFT JOIN ranked r ON r.sale_id = s.sale_id;

ALTER TABLE report_accruals ADD COLUMN pot_micros INTEGER NOT NULL DEFAULT 0;
ALTER TABLE report_accruals ADD COLUMN pots TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(pots));

UPDATE schema_docs SET description = 'Выпуск 2, R3: общаки владельца — покупки какого-то контента на страницах модели за срок делятся поровну между участниками на дату покупки; доля идёт в базу продаж человека и платится по его ставке. Объявляет и меняет только владелец (ledger pot declare). Покупки — pot_sales, доли — v_pot_shares'
 WHERE tbl = 'pots' AND col = '';

INSERT INTO schema_docs(tbl, col, description) VALUES
 ('sale_decisions','', 'Договорённости «чья продажа» (R3): решение владельца или договорённость чаттеров о продаже или ожидаемой продаже — кому и в каких долях, кто сказал, когда, ссылка на сообщение, в каком отчёте учтено. Пишет агент (право reports) или владелец (decide); прежний файл памяти агента — зеркало: ledger decision list --md. Ничего не проводит'),
 ('sale_decisions','id','Номер решения'),
 ('sale_decisions','page_label','Страница хаба'),
 ('sale_decisions','fan_ref','Фан: id хаба'),
 ('sale_decisions','fan_name','Фан: ник'),
 ('sale_decisions','transaction_ref','Продажа хаба (hub_sales.transaction_ref); NULL — продажи ещё нет («обещал купить в пятницу») или она мимо хаба'),
 ('sale_decisions','net_micros','Нетто продажи, микродоллары; NULL — пока неизвестно'),
 ('sale_decisions','occurred_at','Когда продажа, МСК; NULL — пока неизвестно'),
 ('sale_decisions','kind','owner — вся продажа одного человека; split — доли; pot — в общак (pot); off_platform — деньги мимо платформы, в отчёт человека по его слову; exclude — никому'),
 ('sale_decisions','shares','Кому и сколько: JSON [{"person": "<people.slug>", "percent": <целое>}], в сумме 100; у owner одна строка, у pot и exclude пусто'),
 ('sale_decisions','pot','Общак (pots.slug) у kind = pot'),
 ('sale_decisions','said_by','Кто решил: владелец или какие чаттеры, свободный текст'),
 ('sale_decisions','said_at','Когда сказано: ГГГГ-ММ-ДД или время МСК'),
 ('sale_decisions','source_link','Ссылка на сообщение, где сказано'),
 ('sale_decisions','note','Почему так, как пишет агент (до 4000 знаков)'),
 ('sale_decisions','status','open — ждёт отчёта; applied — учтено в отчёте (applied_person, applied_report_date); cancelled — отменено или заменено новым (supersedes)'),
 ('sale_decisions','applied_report_date','Дата отчёта, в котором учтено'),
 ('sale_decisions','applied_person','Чей отчёт (people.slug)'),
 ('sale_decisions','applied_at','Когда отмечено учтённым, МСК'),
 ('sale_decisions','cancelled_at','Когда отменено, МСК'),
 ('sale_decisions','cancel_note','Почему отменено; у заменённого — номер нового решения'),
 ('sale_decisions','supersedes','Какое решение это заменяет (sale_decisions.id); заменённое становится cancelled'),
 ('sale_decisions','created_at','Когда записано, МСК'),
 ('sale_decisions','key_name','Ключ API, которым записано'),
 ('pots','slug','Код общака, уникальный'),
 ('pots','name','Название общака'),
 ('pots','model','Модель страниц общака (как в report_accruals.model): доля идёт на вкладку человека этой модели'),
 ('pots','pages','Страницы хаба общака: JSON-массив page_label'),
 ('pots','starts_at','Первый день общака ГГГГ-ММ-ДД (МСК, включительно): покупка в общаке по её occurred_at'),
 ('pots','ends_at','Последний день общака ГГГГ-ММ-ДД включительно; NULL — без конца'),
 ('pots','participants','Кто делит покупку на её дату: JSON [{"person": "<people.slug>", "from": "ГГГГ-ММ-ДД", "to": "ГГГГ-ММ-ДД" | null}]'),
 ('pots','recognise','Как агенту узнать покупку общака: JSON свободной формы (offer id, media id, слова); ledger его не читает'),
 ('pots','declared_by','Кто объявил: владелец'),
 ('pots','declared_at','Когда объявлено: ГГГГ-ММ-ДД или время МСК'),
 ('pots','decision_ref','Ссылка на решение владельца, текст'),
 ('pots','status','open — принимает покупки; closed — новых покупок не принимает, доли записанных остаются'),
 ('pots','note','Заметка'),
 ('pots','updated_at','Когда объявление последний раз изменено, МСК'),
 ('pots','key_name','Ключ API, которым объявлено или изменено'),
 ('pots','opened_at','Когда записано впервые, МСК'),
 ('pots','id','Номер общака'),
 ('pots','scope','Не используется с R3 (поле первой миграции): страницы и срок — pages, starts_at, ends_at'),
 ('pots','method','Как делится: equal — поровну между участниками на дату покупки'),
 ('pots','closed_at','Не используется с R3: срок — ends_at, закрытие — status'),
 ('pots','settlement','Не используется с R3: доли считает v_pot_shares'),
 ('pot_sales','', 'Покупки общаков (R3): одна строка — одна покупка; снятая остаётся строкой с removed_at. Пишет агент (reports) или владелец (decide): ledger pot add-sale, remove-sale'),
 ('pot_sales','id','Номер покупки общака'),
 ('pot_sales','pot','Общак (pots.slug)'),
 ('pot_sales','transaction_ref','Продажа хаба; уникальна среди не снятых; NULL — покупки нет в хабе'),
 ('pot_sales','page_label','Страница хаба'),
 ('pot_sales','fan_ref','Фан: id хаба'),
 ('pot_sales','fan_name','Фан: ник'),
 ('pot_sales','net_micros','Нетто покупки, микродоллары, больше нуля'),
 ('pot_sales','occurred_at','Когда покупка, МСК'),
 ('pot_sales','day','День покупки ГГГГ-ММ-ДД по МСК: по нему участники и день начисления доли'),
 ('pot_sales','source','agent — агент; report_line — из строки сохранённого отчёта (report_id, report_n); owner — владелец'),
 ('pot_sales','report_id','Отчёт (shift_reports.id), из строки которого взята покупка'),
 ('pot_sales','report_n','Номер строки (n) в том отчёте'),
 ('pot_sales','note','Заметка'),
 ('pot_sales','added_at','Когда записано, МСК'),
 ('pot_sales','key_name','Ключ API, которым записано'),
 ('pot_sales','removed_at','Когда покупка снята из общака, МСК; NULL — в общаке'),
 ('pot_sales','removed_note','Почему снята'),
 ('pot_sales','removed_by','Ключ, которым снята'),
 ('v_pot_shares','', 'Доли общаков: покупка × участник на дату покупки. Доля = нетто ÷ число участников, неделимый остаток — по микродоллару участникам по порядку кода; доли покупки в сумме равны нетто. Покупка без участников на дату — строка без доли с flag'),
 ('v_pot_shares','sale_id','Покупка (pot_sales.id)'),
 ('v_pot_shares','pot','Общак (pots.slug)'),
 ('v_pot_shares','pot_name','Название общака'),
 ('v_pot_shares','model','Модель общака'),
 ('v_pot_shares','transaction_ref','Продажа хаба или NULL'),
 ('v_pot_shares','page_label','Страница хаба'),
 ('v_pot_shares','fan_ref','Фан: id хаба'),
 ('v_pot_shares','fan_name','Фан: ник'),
 ('v_pot_shares','occurred_at','Когда покупка, МСК'),
 ('v_pot_shares','day','День покупки, МСК'),
 ('v_pot_shares','net_micros','Нетто покупки, микродоллары'),
 ('v_pot_shares','participants','Сколько участников на дату'),
 ('v_pot_shares','person','Участник (people.slug); NULL — участников на дату нет'),
 ('v_pot_shares','share_micros','Доля участника, микродоллары; NULL — без доли'),
 ('v_pot_shares','flag','Почему без доли'),
 ('report_accruals','pot_micros','Из выручки дня — доли общаков модели за покупки этого дня, микродоллары (в net_micros уже входят)'),
 ('report_accruals','pots','Доли общаков дня: JSON [{"pot": "<slug>", "name": "<название>", "micros": <доля>}]');

UPDATE schema_docs
   SET description = 'Выручка дня: сумма страниц модели по постам плюс доли общаков модели за покупки этого дня (pot_micros), микродоллары'
 WHERE tbl = 'report_accruals' AND col = 'net_micros';

UPDATE schema_docs
   SET description = 'Начислено бы: выручка (страницы и доли общаков) × ставка + фикс, микродоллары; фикс — только у дня с опубликованной страницей, день из одной доли общака его не получает; NULL — «без ставки» (reason)'
 WHERE tbl = 'report_accruals' AND col = 'amount_micros';

UPDATE schema_docs
   SET description = 'Права через запятую: read — SQL и схема, propose — предложения и споры, decide — решения владельца: бриф, правила, споры, заявки на ничьи продажи, изменения за выплаченные дни, общаки (только openclaw-main), collect — сборщики, import — разовые импорты, payouts — только GET /v1/payouts, GET /v1/payouts/prepare (выплаты чаттерам для хаба), GET /v1/reports (отчёты смен), GET /v1/report-days (база оплаты по отчётам), GET /v1/report-posts без текста постов, GET /v1/decisions и GET /v1/pots, больше ничего, reports — POST /v1/reports (агент команды сохраняет отчёты смен), договорённости (POST /v1/decisions…), покупки общаков (POST /v1/pots/{slug}/sales, POST /v1/pot-sales/{id}/remove) и их чтение'
 WHERE tbl = 'keys' AND col = 'scopes';
