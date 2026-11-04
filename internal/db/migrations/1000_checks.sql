-- Срочные сигналы: что уже сообщено, чтобы один сбой не приходил каждые 15 минут. Путь доллара по переводам.
CREATE TABLE alerts (
  name       TEXT PRIMARY KEY,
  first_seen TEXT NOT NULL,
  last_sent  TEXT NOT NULL,
  summary    TEXT NOT NULL
);
CREATE VIEW v_checks AS
  SELECT c.name, c.ok, c.severity, c.summary, c.details, c.finished_at
  FROM checks c WHERE c.id = (SELECT max(x.id) FROM checks x WHERE x.name = c.name);
-- Путь доллара: по каждому переводу — платежи фанов, заявки, депозиты из пометки, сумма в журнале, k.
CREATE VIEW v_trail AS
  SELECT t.num AS transfer, t.model, t.date, t.cut, t.amount, t.held, t.fee, ROUND(t.est, 2) AS estimate, t.k, t.deposits, t.bad,
         (SELECT COUNT(*) FROM coverage_requests q WHERE q.transfer_num = t.num) AS requests,
         (SELECT ROUND(SUM(q.amount), 2) FROM coverage_requests q WHERE q.transfer_num = t.num) AS requested,
         (SELECT COUNT(DISTINCT p.payment_key) FROM coverage_parts p WHERE p.transfer_num = t.num) AS payments,
         (SELECT ROUND(SUM(p.part), 2) FROM coverage_parts p WHERE p.transfer_num = t.num) AS payments_net
  FROM coverage_transfers t;
INSERT INTO schema_docs(tbl, col, description) VALUES
 ('alerts','', 'Срочные сигналы: когда сбой замечен и когда о нём последний раз сообщено'),
 ('v_checks','', 'Последний результат каждой проверки: ok, серьёзность (info, warn, urgent), итог и детали'),
 ('v_trail','', 'Путь доллара по переводам: сколько платежей фанов и заявок покрыл перевод, net платежей, сумма заявок, депозиты из пометки, запись в журнале, удержано, k');
