-- Прибыль по дням: что проводки дали за каждый день по счетам доходов и расходов.
CREATE VIEW v_daily AS
  SELECT substr(e.effective_at, 1, 10) AS day,
         ROUND(-SUM(CASE WHEN a.kind = 'income' THEN l.amount ELSE 0 END) / 1e6, 2) AS income,
         ROUND(SUM(CASE WHEN a.kind = 'expense' THEN l.amount ELSE 0 END) / 1e6, 2) AS expense,
         ROUND(-SUM(CASE WHEN a.kind IN ('income', 'expense') THEN l.amount ELSE 0 END) / 1e6, 2) AS profit
  FROM lines l
  JOIN entries e ON e.id = l.entry_id
  JOIN accounts a ON a.code = l.account
  GROUP BY substr(e.effective_at, 1, 10);
INSERT INTO schema_docs(tbl, col, description) VALUES
 ('v_daily', '', 'Прибыль по дням: доходы, расходы и их разница по проводкам, день — дата проводки, МСК. Месяц: WHERE day LIKE ''ГГГГ-ММ%'''),
 ('v_daily', 'day', 'День'),
 ('v_daily', 'income', 'Доходы за день'),
 ('v_daily', 'expense', 'Расходы за день'),
 ('v_daily', 'profit', 'Доходы минус расходы');
