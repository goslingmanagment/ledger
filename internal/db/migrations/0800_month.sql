-- Карточка месяца в базе: последний расчёт по каждому месяцу, план оборота месяца и представление v_month.
CREATE TABLE month_cards (
  month       TEXT PRIMARY KEY,
  as_of       TEXT NOT NULL,
  computed_at TEXT NOT NULL,
  status      TEXT NOT NULL CHECK (status IN ('ok','unknown')),
  reason      TEXT NOT NULL DEFAULT '',
  card        TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(card))
);
CREATE TABLE month_plans (
  month      TEXT NOT NULL,
  page_label TEXT NOT NULL,
  per_day    REAL NOT NULL,
  created_at TEXT NOT NULL,
  source     TEXT NOT NULL,
  PRIMARY KEY (month, page_label)
);
CREATE VIEW v_month AS
  SELECT c.month,
         CASE WHEN p.month IS NULL THEN 'открыт' ELSE 'закрыт' END AS state,
         c.status,
         json_extract(c.card, '$.totals.net') AS net,
         json_extract(c.card, '$.totals.earned') AS earned,
         json_extract(c.card, '$.totals.arrived') AS arrived,
         json_extract(c.card, '$.totals.hold') AS hold,
         json_extract(c.card, '$.totals.page') AS on_page,
         json_extract(c.card, '$.totals.riding') AS riding,
         json_extract(c.card, '$.totals.stuck') AS stuck,
         json_extract(c.card, '$.totals.transit') AS transit,
         json_extract(c.card, '$.expenses.total') AS expenses,
         json_extract(c.card, '$.decision') AS decision,
         CASE WHEN p.decision IS NOT NULL THEN p.decision / 1e6 END AS decision_at_close,
         p.verdict AS verdict_at_close,
         p.locked_at,
         (SELECT r.decision / 1e6 FROM period_reports r WHERE r.month = c.month ORDER BY r.id DESC LIMIT 1) AS decision_reported,
         c.as_of, c.computed_at, c.reason
  FROM month_cards c LEFT JOIN periods p ON p.month = c.month;
INSERT INTO schema_docs(tbl, col, description) VALUES
 ('month_cards','', 'Последний расчёт карточки по месяцу: JSON с итогами, строками, расходами, переводами; status unknown — данных не хватило, reason объясняет'),
 ('month_plans','', 'План оборота месяца по страницам ($ в день)'),
 ('v_month','', 'Карточка месяца: оборот, заработано, пришло, холд, на странице, едет, застряло, в пути, расходы, цифра решения «сейчас»; у закрытых месяцев рядом цифра на закрытии и сообщённая владельцу');
