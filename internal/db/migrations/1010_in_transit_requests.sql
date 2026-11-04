-- «Заявлено» в деньгах в пути: заявки обеих платформ, которые ещё в обработке, за последние 14 дней от среза.
-- Отклонённые и исполненные не считаются; старше 14 дней заявка уже не «едет», её разбирает проверка просрочки.
DROP VIEW v_in_transit;
CREATE VIEW v_in_transit AS
  SELECT c.page_label AS page, c.model, c.channel,
         ROUND(c.hold_net, 2) AS hold_net, ROUND(c.hold, 2) AS hold,
         ROUND(c.avail_net, 2) AS available_net, ROUND(c.available, 2) AS available,
         ROUND(COALESCE((SELECT SUM(p.amount_mills) / 1000.0 FROM hub_payouts p
                          WHERE p.page_label = c.page_label
                            AND lower(COALESCE(p.status, '')) NOT IN ('processed', 'rejected', 'declined', 'failed', 'canceled', 'cancelled')
                            AND p.requested_at >= date(substr(c.as_of, 1, 10), '-14 days')), 0)
             + COALESCE((SELECT SUM(p.amount_mills) / 1000.0 FROM hub_of_payouts p
                          WHERE p.page_label = c.page_label
                            AND lower(COALESCE(p.state, '')) NOT IN ('approved', 'rejected', 'declined', 'failed', 'canceled', 'cancelled')
                            AND p.requested_at >= date(substr(c.as_of, 1, 10), '-14 days')), 0), 2) AS requested_net,
         ROUND(c.riding_net, 2) AS at_model_net, ROUND(c.riding, 2) AS at_model,
         ROUND(c.stuck, 2) AS stuck, c.min_payout,
         ROUND(c.hold + c.available + c.riding + c.stuck, 2) AS total,
         (SELECT status FROM coverage_runs ORDER BY id DESC LIMIT 1) AS status,
         (SELECT reason FROM coverage_runs ORDER BY id DESC LIMIT 1) AS reason,
         c.as_of
  FROM coverage_pages c;
