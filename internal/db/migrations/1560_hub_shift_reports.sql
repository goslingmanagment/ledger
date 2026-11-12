-- Отчёты смен, сданные в хабе (экосистема, пакет F2; хаб — hub#1225): третий источник hub рядом с черновиком агента
-- и постом «Отчётности». Джоб hub-reports раз в 5 минут читает ленту хаба GET /api/v1/agent/shift-reports по курсору
-- (cursors 'hub:shift_reports') ключом ledger в хабе и хранит каждую сданную версию как есть: страницы, строки продаж
-- с transaction_ref, слово человека, «мимо площадки». Новая версия той же смены заменяет прежнюю; две смены человека
-- за день — два отчёта, их страницы за день складываются. Версия, которую не сохранить (кода человека нет в people,
-- страницы нет в hub_pages, суммы не сходятся), хранится с причиной (problem) и видна в ledger health — не пропускается
-- и человек не угадывается. В базу оплаты хаб входит только с settings.payroll_basis_hub_from, она пуста: до неё
-- v_report_pages.accrue_micros равен posted_micros, и ни начисления, ни долги, ни зеркало таблиц не меняются.
-- Миграция только добавляет: новые таблицы, настройку, колонку report_accruals.hub_reports (NULL), вид пересоздаётся с
-- прежними колонками в прежнем порядке и новыми в конце. Деньги — целые микродоллары (mills хаба × 1000), времена — МСК.

CREATE TABLE hub_shift_reports (
  id                   INTEGER PRIMARY KEY,
  shift_id             INTEGER NOT NULL,
  version              INTEGER NOT NULL,
  person               TEXT,
  hub_person           TEXT,
  hub_user_id          INTEGER,
  report_date          TEXT NOT NULL,
  submitted_at         TEXT NOT NULL,
  door                 TEXT NOT NULL DEFAULT '',
  telegram_message_ref TEXT,
  total_micros         INTEGER NOT NULL DEFAULT 0,
  comment              TEXT NOT NULL DEFAULT '',
  day_paid             INTEGER CHECK (day_paid IN (0,1)),
  rule_version         TEXT NOT NULL DEFAULT '',
  current              INTEGER NOT NULL DEFAULT 0 CHECK (current IN (0,1)),
  problem              TEXT,
  payload              TEXT NOT NULL CHECK (json_valid(payload)),
  received_at          TEXT NOT NULL,
  stored_at            TEXT
);
CREATE UNIQUE INDEX hub_shift_reports_version ON hub_shift_reports(shift_id, version);
CREATE INDEX hub_shift_reports_day ON hub_shift_reports(person, report_date);

CREATE TABLE hub_shift_report_pages (
  report_id     INTEGER NOT NULL REFERENCES hub_shift_reports(id),
  seq           INTEGER NOT NULL,
  page_label    TEXT NOT NULL,
  amount_micros INTEGER NOT NULL,
  PRIMARY KEY (report_id, page_label)
);

CREATE TABLE hub_shift_report_lines (
  report_id          INTEGER NOT NULL REFERENCES hub_shift_reports(id),
  seq                INTEGER NOT NULL,
  transaction_ref    TEXT NOT NULL,
  page_label         TEXT NOT NULL,
  grp                TEXT NOT NULL,
  net_micros         INTEGER NOT NULL,
  rule_share_micros  INTEGER NOT NULL,
  rule_home          TEXT NOT NULL DEFAULT '',
  rule_reason        TEXT NOT NULL DEFAULT '',
  statement          TEXT,
  share_with_user_id INTEGER,
  note               TEXT,
  amount_micros      INTEGER NOT NULL,
  counted            INTEGER NOT NULL CHECK (counted IN (0,1)),
  added              INTEGER NOT NULL CHECK (added IN (0,1)),
  disputed           INTEGER NOT NULL CHECK (disputed IN (0,1)),
  PRIMARY KEY (report_id, seq)
);
CREATE INDEX hub_shift_report_lines_ref ON hub_shift_report_lines(transaction_ref);

CREATE TABLE hub_shift_report_adds (
  report_id     INTEGER NOT NULL REFERENCES hub_shift_reports(id),
  seq           INTEGER NOT NULL,
  hub_id        INTEGER NOT NULL,
  amount_micros INTEGER NOT NULL,
  from_whom     TEXT NOT NULL DEFAULT '',
  what_for      TEXT NOT NULL DEFAULT '',
  state         TEXT NOT NULL,
  counted       INTEGER NOT NULL DEFAULT 0 CHECK (counted IN (0,1)),
  PRIMARY KEY (report_id, seq)
);

INSERT INTO settings(key, value, note, updated_at) VALUES ('payroll_basis_hub_from', '',
  'С какого дня (МСК) день человека начисляется по отчёту, сданному в хабе (hub_shift_reports), когда он есть за человека и день, иначе по посту, как прежде; пусто — хаб в начисления не входит. Только вместе с payroll_reports_from и не раньше неё. Ставит и снимает ledger payroll hub-from; откат — --off и пересборка',
  '2026-11-12T00:00:00+03:00')
ON CONFLICT(key) DO NOTHING;

ALTER TABLE report_accruals ADD COLUMN hub_reports TEXT CHECK (hub_reports IS NULL OR json_valid(hub_reports));

-- База оплаты: прежние колонки как в 1510; новые — сданный в хабе отчёт рядом с постом и что начисляется.
DROP VIEW v_report_pages;

CREATE VIEW v_report_pages AS
WITH posted AS (
  SELECT p.person, p.report_date, pp.page_label, p.id AS post_id, p.posted_at, p.model AS post_model,
         SUM(pp.amount_micros) AS amount
    FROM shift_report_posts p JOIN shift_report_post_pages pp ON pp.post_id = p.id
   WHERE p.current = 1 AND p.person IS NOT NULL AND p.report_date IS NOT NULL
     AND pp.page_label IS NOT NULL AND pp.in_shift = 1
     AND NOT EXISTS (SELECT 1 FROM json_each(p.flags) f
                      WHERE f.value IN ('unknown_author','no_date','sum_mismatch','date_mismatch'))
   GROUP BY p.id, pp.page_label
),
latest AS (
  SELECT posted.*, ROW_NUMBER() OVER (PARTITION BY person, report_date, page_label
                                      ORDER BY posted_at DESC, post_id DESC) AS rn
    FROM posted
),
report AS (
  SELECT r.id, r.person, r.report_date,
         ROW_NUMBER() OVER (PARTITION BY r.person, r.report_date ORDER BY r.created_at DESC, r.id DESC) AS rn
    FROM shift_reports r
   WHERE r.source = 'agent' AND r.status <> 'superseded' AND r.person IS NOT NULL
),
draft AS (
  SELECT d.person, d.report_date, sp.page_label, d.id AS report_id, sp.amount_micros AS amount
    FROM report d JOIN shift_report_pages sp ON sp.report_id = d.id
   WHERE d.rn = 1
),
hub AS (
  SELECT r.person, r.report_date, hp.page_label, SUM(hp.amount_micros) AS amount
    FROM hub_shift_reports r JOIN hub_shift_report_pages hp ON hp.report_id = r.id
   WHERE r.current = 1 AND r.problem IS NULL AND r.person IS NOT NULL
   GROUP BY r.person, r.report_date, hp.page_label
),
hub_days AS (
  SELECT DISTINCT person, report_date FROM hub
),
hub_from AS (
  SELECT COALESCE((SELECT trim(value) FROM settings WHERE key = 'payroll_basis_hub_from'), '') AS day
),
day_keys AS (
  SELECT person, report_date, page_label FROM latest WHERE rn = 1
  UNION
  SELECT person, report_date, page_label FROM draft
  UNION
  SELECT person, report_date, page_label FROM hub
)
SELECT k.person AS person,
       k.report_date AS report_date,
       k.page_label AS page,
       COALESCE((SELECT COALESCE((SELECT j.value FROM settings s, json_each(s.value) j
                                   WHERE s.key = 'hub_model_slugs' AND json_valid(s.value) AND j.key = hp.model_slug),
                                 hp.model_slug)
                   FROM hub_pages hp WHERE hp.page_label = k.page_label), l.post_model, '') AS model,
       l.amount AS posted_micros,
       d.amount AS draft_micros,
       CASE WHEN l.amount IS NOT NULL AND d.amount IS NOT NULL
            THEN CASE WHEN abs(l.amount - d.amount) <= 5000 THEN 0 ELSE l.amount - d.amount END END AS diff_micros,
       CASE WHEN l.amount IS NULL AND d.amount IS NULL THEN 'hub_only'
            WHEN l.amount IS NULL THEN 'draft_only'
            WHEN d.amount IS NULL THEN 'post_only'
            WHEN abs(l.amount - d.amount) <= 5000 THEN 'posted_equal'
            ELSE 'posted_changed' END AS state,
       l.posted_at AS posted_at,
       d.report_id AS report_id,
       l.post_id AS post_id,
       h.amount AS hub_micros,
       CASE WHEN l.amount IS NOT NULL AND h.amount IS NOT NULL
            THEN CASE WHEN h.amount >= l.amount - 5000 AND h.amount < l.amount + 5000 THEN 0 ELSE l.amount - h.amount END END AS hub_diff_micros,
       CASE WHEN hd.person IS NULL THEN NULL
            WHEN h.amount IS NULL AND l.amount IS NULL THEN NULL
            WHEN h.amount IS NULL THEN 'hub_missing'
            WHEN l.amount IS NULL THEN 'hub_only'
            WHEN h.amount >= l.amount - 5000 AND h.amount < l.amount + 5000 THEN 'hub_equal'
            ELSE 'hub_changed' END AS hub_state,
       CASE WHEN f.day <> '' AND k.report_date >= f.day AND hd.person IS NOT NULL THEN 'hub' ELSE 'post' END AS basis,
       CASE WHEN f.day <> '' AND k.report_date >= f.day AND hd.person IS NOT NULL THEN h.amount ELSE l.amount END AS accrue_micros
  FROM day_keys k
  CROSS JOIN hub_from f
  LEFT JOIN latest l ON l.rn = 1 AND l.person = k.person AND l.report_date = k.report_date AND l.page_label = k.page_label
  LEFT JOIN draft d ON d.person = k.person AND d.report_date = k.report_date AND d.page_label = k.page_label
  LEFT JOIN hub h ON h.person = k.person AND h.report_date = k.report_date AND h.page_label = k.page_label
  LEFT JOIN hub_days hd ON hd.person = k.person AND hd.report_date = k.report_date;

INSERT INTO schema_docs(tbl, col, description) VALUES
 ('hub_shift_reports','', 'Отчёты смен, сданные в хабе (источник hub, hub#1225): каждая версия ленты хаба GET /api/v1/agent/shift-reports, как пришла. Отчёт — смена хаба (shift_id); новая версия заменяет прежнюю (current), две смены человека за день — два отчёта, страницы дня складываются (v_report_pages). Версия, которую не сохранить, хранится с problem и без страниц и строк; джоб пробует её снова каждый проход. Начисляет только с settings.payroll_basis_hub_from'),
 ('hub_shift_reports','id','id версии в хабе: курсор ленты'),
 ('hub_shift_reports','shift_id','Смена хаба (shifts.id): отчёт — смена'),
 ('hub_shift_reports','version','Номер версии отчёта смены в хабе: 1, 2, …; та же смена и версия — та же запись'),
 ('hub_shift_reports','person','Человек (people.slug) — код человека хаба, если он есть в people; NULL — нет (problem)'),
 ('hub_shift_reports','hub_person','Код человека, как прислал хаб; NULL — у логина в хабе нет человека'),
 ('hub_shift_reports','hub_user_id','id пользователя хаба'),
 ('hub_shift_reports','report_date','Дата отчёта ГГГГ-ММ-ДД, как у хаба (правило payroll.ReportDate)'),
 ('hub_shift_reports','submitted_at','Когда версия сдана, МСК'),
 ('hub_shift_reports','door','Откуда сдана: workboard, extension или telegram (агентом со слов)'),
 ('hub_shift_reports','telegram_message_ref','Сообщение Telegram, со слов которого агент сдал отчёт; NULL — сдал сам человек'),
 ('hub_shift_reports','total_micros','Итог версии, микродоллары: сумма страниц, без «мимо площадки»'),
 ('hub_shift_reports','comment','Комментарий человека'),
 ('hub_shift_reports','day_paid','Сказал ли ledger хабу, что день выплачен, когда версия делалась: 1 — да, 0 — нет, NULL — неизвестно'),
 ('hub_shift_reports','rule_version','Версия правила «чья продажа» хаба, по которой собран черновик'),
 ('hub_shift_reports','current','1 — новейшая версия своей смены (действующий отчёт); 0 — заменена'),
 ('hub_shift_reports','problem','Почему версия не сохранена (кода человека нет в people, страницы нет в hub_pages, суммы не сходятся); NULL — сохранена. Видно в ledger health'),
 ('hub_shift_reports','payload','JSON версии, как пришёл из ленты'),
 ('hub_shift_reports','received_at','Когда версия впервые прочитана, МСК'),
 ('hub_shift_reports','stored_at','Когда сохранены страницы и строки, МСК; NULL — не сохранены (problem)'),
 ('hub_shift_report_pages','', 'Страницы сданного в хабе отчёта: страница и её сумма (продажи страницы, округлённые хабом до цента)'),
 ('hub_shift_report_pages','report_id','Версия (hub_shift_reports.id)'),
 ('hub_shift_report_pages','seq','Порядок страницы в версии'),
 ('hub_shift_report_pages','page_label','Страница хаба (pageLabel)'),
 ('hub_shift_report_pages','amount_micros','Сумма страницы, микродоллары'),
 ('hub_shift_report_lines','', 'Продажи сданного в хабе отчёта: каждая продажа, что идёт в итог и почему, слово человека'),
 ('hub_shift_report_lines','report_id','Версия (hub_shift_reports.id)'),
 ('hub_shift_report_lines','seq','Порядок строки в версии'),
 ('hub_shift_report_lines','transaction_ref','Продажа хаба (transactions.id, как hub_sales.transaction_ref)'),
 ('hub_shift_report_lines','page_label','Страница хаба'),
 ('hub_shift_report_lines','grp','mine — «Ваши продажи» по правилу; near — «Рядом, но не ваши по правилу»'),
 ('hub_shift_report_lines','net_micros','Нетто продажи, микродоллары'),
 ('hub_shift_report_lines','rule_share_micros','Доля человека по правилу хаба, микродоллары'),
 ('hub_shift_report_lines','rule_home','Куда продажу отдаёт правило хаба: shift, visit, pot, waiting, decide, nobody, before_rule, unknown'),
 ('hub_shift_report_lines','rule_reason','Причина правила хаба (код)'),
 ('hub_shift_report_lines','statement','Слово человека: mine — «моё», not_mine — «не моё», share — «делим»; NULL — как по правилу'),
 ('hub_shift_report_lines','share_with_user_id','С кем делит (id пользователя хаба)'),
 ('hub_shift_report_lines','note','Заметка человека к продаже'),
 ('hub_shift_report_lines','amount_micros','Сколько строка даёт в отчёт, микродоллары'),
 ('hub_shift_report_lines','counted','1 — строка в итоге (amount_micros не 0), 0 — нет'),
 ('hub_shift_report_lines','added','1 — «долёт»: продажа дней до смены, добавленная человеком'),
 ('hub_shift_report_lines','disputed','1 — «моё» на продажу, которую правило отдаёт другому: в итоге, пока спор не решён в хабе'),
 ('hub_shift_report_adds','', 'Деньги «мимо площадки» сданного в хабе отчёта: в итог не входят, пока владелец не подтвердит (подтверждённое приходит новой версией)'),
 ('hub_shift_report_adds','report_id','Версия (hub_shift_reports.id)'),
 ('hub_shift_report_adds','seq','Порядок в версии'),
 ('hub_shift_report_adds','hub_id','id записи «мимо площадки» в хабе'),
 ('hub_shift_report_adds','amount_micros','Сумма, микродоллары'),
 ('hub_shift_report_adds','from_whom','От кого'),
 ('hub_shift_report_adds','what_for','За что'),
 ('hub_shift_report_adds','state','pending — ждёт владельца, confirmed — подтверждено, rejected — отклонено'),
 ('hub_shift_report_adds','counted','Всегда 0: в итог версии не входит'),
 ('report_accruals','hub_reports','Сданные в хабе версии, из которых день (JSON-массив hub_shift_reports.id); NULL — день из постов'),
 ('v_report_pages','hub_micros','Сумма страницы в отчётах, сданных в хабе за человека и день (действующие версии всех его смен дня сложены), микродоллары; NULL — нет'),
 ('v_report_pages','hub_diff_micros','Пост минус хаб, микродоллары, когда есть оба; 0 — пост равен хабу до цента'),
 ('v_report_pages','hub_state','NULL — за человека и день отчёта в хабе нет; hub_equal — пост равен хабу до цента; hub_changed — отличается (hub_diff_micros); hub_only — в хабе есть, в посте нет; hub_missing — в посте есть, в хабе нет'),
 ('v_report_pages','basis','Откуда начисляется день: hub — отчёт хаба (с settings.payroll_basis_hub_from, когда за человека и день он есть); post — пост, как прежде'),
 ('v_report_pages','accrue_micros','Что начисляется по странице: hub_micros при basis hub, иначе posted_micros; это пишет зеркало таблиц и читает report_accruals');

UPDATE schema_docs
   SET description = 'posted_equal — пост равен черновику до цента (расхождение не больше полцента: черновик хранит тысячные доллара, пост — центы); posted_changed — отличается (diff_micros); post_only — пост без черновика; draft_only — черновик без поста; hub_only — только отчёт хаба, ни поста, ни черновика'
 WHERE tbl = 'v_report_pages' AND col = 'state';

UPDATE schema_docs
   SET description = 'База оплаты по отчётам: человек × дата отчёта × страница, где есть сумма поста, черновика агента или отчёта, сданного в хабе. Пост — действующая версия последнего поста человека за дату со страницей в смене и без блокирующих пометок; черновик — действующая версия агента (source agent) с человеком; хаб — действующие версии смен человека за дату, сложенные. Что начисляется — accrue_micros. Ничего не начисляет сам'
 WHERE tbl = 'v_report_pages' AND col = '';
