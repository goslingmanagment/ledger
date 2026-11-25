-- Align v_report_pages hub_equal / hub_diff_micros tolerance with SameCents (±5000 micros inclusive).
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
            THEN CASE WHEN abs(l.amount - h.amount) <= 5000 THEN 0 ELSE l.amount - h.amount END END AS hub_diff_micros,
       CASE WHEN hd.person IS NULL THEN NULL
            WHEN h.amount IS NULL AND l.amount IS NULL THEN NULL
            WHEN h.amount IS NULL THEN 'hub_missing'
            WHEN l.amount IS NULL THEN 'hub_only'
            WHEN abs(l.amount - h.amount) <= 5000 THEN 'hub_equal'
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
