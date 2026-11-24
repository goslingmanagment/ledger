-- Keep future formula names verbatim. Copy both sides of the foreign key together.
ALTER TABLE hub_pot_distribution_inflows RENAME TO old_pot_inflows;
ALTER TABLE hub_pot_distributions RENAME TO old_pot_distributions;
DROP INDEX hub_pot_inflows_tx;
DROP INDEX hub_pot_distributions_day;
CREATE TABLE hub_pot_distributions (
 id INTEGER PRIMARY KEY, seq INTEGER NOT NULL CHECK(seq>0), state TEXT NOT NULL CHECK(state IN ('posted','reversed')),
 pot_code TEXT NOT NULL, pot_name TEXT NOT NULL, model_slug TEXT NOT NULL, day TEXT NOT NULL,
 formula_kind TEXT NOT NULL CHECK(formula_kind <> ''),
 formula_params TEXT NOT NULL CHECK(json_valid(formula_params)), base_mills INTEGER NOT NULL,
 lines_json TEXT NOT NULL CHECK(json_valid(lines_json)), at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE INDEX hub_pot_distributions_day ON hub_pot_distributions(day);
CREATE TABLE hub_pot_distribution_inflows (
 distribution_id INTEGER NOT NULL REFERENCES hub_pot_distributions(id), transaction_id INTEGER NOT NULL,
 page_label TEXT NOT NULL, base_mills INTEGER NOT NULL, PRIMARY KEY(distribution_id,transaction_id)
);
CREATE INDEX hub_pot_inflows_tx ON hub_pot_distribution_inflows(transaction_id);
INSERT INTO hub_pot_distributions SELECT * FROM old_pot_distributions;
INSERT INTO hub_pot_distribution_inflows SELECT * FROM old_pot_inflows;
DROP TABLE old_pot_inflows;
DROP TABLE old_pot_distributions;

-- NULL distinguishes postings made before the page snapshot was recorded.
ALTER TABLE report_accrual_entries ADD COLUMN pages TEXT CHECK(pages IS NULL OR json_valid(pages));
-- Older entries never recorded their page list. Identify only pot-only bookings
-- with no shift evidence. In particular a zero-fix, zero-revenue shift still counts.
UPDATE report_accrual_entries SET pages='[]' WHERE entry_id IN (
 SELECT b.entry_id FROM v_booked_report_days b WHERE b.hub_pots<>'[]' AND b.fix_micros=0
 AND b.hub_reports='[]' AND b.sale_refs='[]'
 AND b.net_micros=(SELECT sum(j.value->>'micros') FROM json_each(b.hub_pots) j)
 AND NOT EXISTS(SELECT 1 FROM shift_report_posts p JOIN shift_report_post_pages pg ON pg.post_id=p.id
                WHERE p.person=b.person AND p.report_date=b.day AND pg.in_shift=1)
 AND NOT EXISTS(SELECT 1 FROM hub_shift_reports r WHERE r.person=b.person AND r.report_date=b.day)
);

INSERT INTO schema_docs(tbl,col,description) VALUES
 ('report_accrual_entries','pages','Страницы смены на момент проводки; пустой массив — только общак, NULL — старый снимок'),
 ('v_booked_report_days','pages','Страницы смены на момент проводки');

DROP VIEW v_report_pages;
CREATE VIEW v_report_pages AS
WITH posted AS (
  SELECT p.person, p.report_date, pp.page_label, p.id AS post_id, p.posted_at, p.model AS post_model,
         SUM(pp.amount_micros) AS amount
    FROM shift_report_posts p JOIN shift_report_post_pages pp ON pp.post_id = p.id
   WHERE p.current = 1 AND p.person IS NOT NULL AND p.report_date IS NOT NULL
     AND pp.page_label IS NOT NULL AND pp.in_shift = 1
     AND NOT EXISTS (SELECT 1 FROM json_each(p.flags) f
                      WHERE f.value IN ('unknown_author','no_date','sum_mismatch','date_mismatch','parse_error'))
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
                                   WHERE s.key = 'hub_model_slugs' AND json_valid(s.value) AND j.key = hp.model_slug COLLATE NOCASE),
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

UPDATE settings SET value=(SELECT value FROM migration_pot_model_map)
 WHERE key='hub_model_slugs' AND value='{}' AND EXISTS(SELECT 1 FROM migration_pot_model_map);
DROP TABLE migration_pot_model_map;
