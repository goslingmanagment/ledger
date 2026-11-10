-- Пост пишет суммы до цента, а черновик агента хранит страницу в тысячных доллара (так отдаёт хаб). Страница
-- $697.576 в черновике и $697,58 в посте — один и тот же отчёт: агент сам печатает чаттеру сумму, округлённую до
-- цента. Раньше такая страница считалась «изменённой после черновика». Теперь пост равен черновику, если они
-- расходятся не больше чем на полцента; разница при этом — ноль.

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
day_keys AS (
  SELECT person, report_date, page_label FROM latest WHERE rn = 1
  UNION
  SELECT person, report_date, page_label FROM draft
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
       CASE WHEN l.amount IS NULL THEN 'draft_only'
            WHEN d.amount IS NULL THEN 'post_only'
            WHEN abs(l.amount - d.amount) <= 5000 THEN 'posted_equal'
            ELSE 'posted_changed' END AS state,
       l.posted_at AS posted_at,
       d.report_id AS report_id,
       l.post_id AS post_id
  FROM day_keys k
  LEFT JOIN latest l ON l.rn = 1 AND l.person = k.person AND l.report_date = k.report_date AND l.page_label = k.page_label
  LEFT JOIN draft d ON d.person = k.person AND d.report_date = k.report_date AND d.page_label = k.page_label;

UPDATE schema_docs
   SET description = 'posted_equal — пост равен черновику до цента (расхождение не больше полцента: черновик хранит тысячные доллара, пост — центы); posted_changed — отличается (diff_micros); post_only — пост без черновика; draft_only — черновик без поста'
 WHERE tbl = 'v_report_pages' AND col = 'state';
