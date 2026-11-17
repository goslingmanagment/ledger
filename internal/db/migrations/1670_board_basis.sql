-- Доска «Отчёт» как основа дня (П2): кто сдаёт смену доской, приходит из реестра людей хаба, а день такого человека
-- с settings.payroll_basis_hub_from начисляется только по его учтённому отчёту хаба — пост его не подменяет.
--
-- Право человека submits_shift («сдаёт смену доской») и время его включения отдаёт лента реестра хаба
-- GET /api/v1/agent/people (rightGrants, hub#1395); джоб hub-terms хранит их в hub_person_rights. Доска действует для
-- смен, начатых после включения, поэтому день доски — со следующего дня (МСК) после включения: в сам день включения
-- смена могла начаться раньше и сдаётся постом, день идёт по прежнему правилу (отчёт хаба, если есть, иначе пост).
-- Снятое право помечается gone_at, строка остаётся: дни до снятия остаются днями доски.
--
-- v_report_pages пересоздаётся с прежними колонками в прежнем порядке и новой board в конце: basis hub и у дня доски
-- без отчёта хаба — accrue_micros у него NULL, пост в начисление не идёт; что с таким днём, решает planReports
-- (не начислен, стоит в «не занесено»). Пока payroll_basis_hub_from пуст, ничего не меняется.
--
-- Курсор ленты людей сбрасывается: следующий проход джоба прочитает реестр с начала и запишет права тех, кого лента
-- уже отдавала без них. Люди и условия при повторе не меняются (upsert только изменившегося).

CREATE TABLE hub_person_rights (
  hub_person_id INTEGER NOT NULL,
  person        TEXT NOT NULL,
  right_name    TEXT NOT NULL,
  granted_at    TEXT NOT NULL,
  granted_day   TEXT NOT NULL,
  stored_at     TEXT NOT NULL,
  gone_at       TEXT,
  gone_day      TEXT,
  PRIMARY KEY (hub_person_id, right_name, granted_at)
);
CREATE INDEX hub_person_rights_person ON hub_person_rights(person, right_name);

-- Дни доски человека: со следующего дня после включения submits_shift по день перед снятием (to_day NULL — действует).
CREATE VIEW v_board_spans AS
SELECT person,
       date(granted_day, '+1 day') AS from_day,
       CASE WHEN gone_day IS NULL THEN NULL ELSE date(gone_day, '-1 day') END AS to_day,
       granted_at,
       gone_at
  FROM hub_person_rights
 WHERE right_name = 'submits_shift';

UPDATE cursors SET value = '' WHERE name = 'hub:people';

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
),
board_days AS (
  SELECT DISTINCT k.person, k.report_date
    FROM day_keys k JOIN v_board_spans b ON b.person = k.person
   WHERE k.report_date >= b.from_day AND (b.to_day IS NULL OR k.report_date <= b.to_day)
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
       CASE WHEN f.day <> '' AND k.report_date >= f.day AND (hd.person IS NOT NULL OR bd.person IS NOT NULL) THEN 'hub' ELSE 'post' END AS basis,
       CASE WHEN f.day <> '' AND k.report_date >= f.day AND (hd.person IS NOT NULL OR bd.person IS NOT NULL) THEN h.amount ELSE l.amount END AS accrue_micros,
       CASE WHEN bd.person IS NULL THEN 0 ELSE 1 END AS board
  FROM day_keys k
  CROSS JOIN hub_from f
  LEFT JOIN latest l ON l.rn = 1 AND l.person = k.person AND l.report_date = k.report_date AND l.page_label = k.page_label
  LEFT JOIN draft d ON d.person = k.person AND d.report_date = k.report_date AND d.page_label = k.page_label
  LEFT JOIN hub h ON h.person = k.person AND h.report_date = k.report_date AND h.page_label = k.page_label
  LEFT JOIN hub_days hd ON hd.person = k.person AND hd.report_date = k.report_date
  LEFT JOIN board_days bd ON bd.person = k.person AND bd.report_date = k.report_date;

INSERT INTO schema_docs(tbl, col, description) VALUES
 ('hub_person_rights','', 'Права людей из реестра хаба с временем включения (лента GET /api/v1/agent/people, rightGrants; hub#1395). Пишет джоб hub-terms вместе с людьми; право, которое хаб больше не отдаёт, помечается gone_at, строка остаётся. Сейчас читается одно: submits_shift — человек сдаёт смену доской «Отчёт» (v_board_spans)'),
 ('hub_person_rights','hub_person_id','id человека в реестре хаба (hub_people.hub_id)'),
 ('hub_person_rights','person','Код человека — people.slug'),
 ('hub_person_rights','right_name','Право хаба: submits_shift, sees_own_pay, customs_lead, records_customs'),
 ('hub_person_rights','granted_at','Когда право включено в хабе (grantedAt, как прислал хаб); включено заново — новая строка'),
 ('hub_person_rights','granted_day','День включения по МСК'),
 ('hub_person_rights','stored_at','Когда ledger записал строку'),
 ('hub_person_rights','gone_at','Когда ledger увидел, что хаб больше не отдаёт право у человека, которого отдаёт; NULL — действует'),
 ('hub_person_rights','gone_day','День gone_at по МСК'),
 ('v_board_spans','', 'Дни, когда человек сдаёт смену доской «Отчёт»: со следующего дня (МСК) после включения submits_shift по день перед его снятием. С settings.payroll_basis_hub_from такой день начисляется только по учтённому отчёту хаба: без него не начислен и стоит в «не занесено», пост его не подменяет. День включения — по прежнему правилу (смена могла начаться раньше включения)'),
 ('v_board_spans','person','Код человека — people.slug'),
 ('v_board_spans','from_day','Первый день доски: следующий день после включения права'),
 ('v_board_spans','to_day','Последний день доски: день перед снятием права; NULL — действует'),
 ('v_board_spans','granted_at','Когда право включено в хабе'),
 ('v_board_spans','gone_at','Когда ledger увидел снятие права; NULL — действует'),
 ('v_report_pages','board','1 — день человека на доске «Отчёт» (v_board_spans): с payroll_basis_hub_from день только по отчёту хаба (basis hub), без отчёта accrue_micros NULL; 0 — нет');

UPDATE schema_docs
   SET description = 'Откуда начисляется день: hub — отчёт хаба (с settings.payroll_basis_hub_from, когда за человека и день он есть или день на доске, board 1); post — пост, как прежде'
 WHERE tbl = 'v_report_pages' AND col = 'basis';

UPDATE schema_docs
   SET description = 'Что начисляется по странице: hub_micros при basis hub (у дня доски без отчёта хаба — NULL: ничего), иначе posted_micros; это пишет зеркало таблиц и читает report_accruals'
 WHERE tbl = 'v_report_pages' AND col = 'accrue_micros';
