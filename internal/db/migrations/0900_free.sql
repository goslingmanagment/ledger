-- Свободные деньги: последний расчёт и представление v_free. Обязательства — таблица commitments.
CREATE TABLE free_runs (
  id          INTEGER PRIMARY KEY,
  computed_at TEXT NOT NULL,
  data        TEXT NOT NULL CHECK (json_valid(data))
);
CREATE VIEW v_free AS
  SELECT json_extract(data, '$.free_now') AS free_now,
         json_extract(data, '$.free_after') AS free_after,
         json_extract(data, '$.liquid') AS liquid,
         json_extract(data, '$.debts') AS debts,
         json_extract(data, '$.commitments_soon') AS commitments_soon,
         json_extract(data, '$.commitments_later') AS commitments_later,
         json_extract(data, '$.cushion') AS cushion,
         json_extract(data, '$.expected') AS expected_transfers,
         json_extract(data, '$.next_income') AS next_income,
         json_extract(data, '$.transit_status') AS transit_status,
         computed_at
  FROM free_runs ORDER BY id DESC LIMIT 1;
INSERT INTO schema_docs(tbl, col, description) VALUES
 ('free_runs','', 'Прогоны расчёта свободных денег: разбивка JSON'),
 ('v_free','', 'Свободные деньги: сейчас и после ближайших переводов, из чего сложились; transit_status unknown — деньги в пути не подтверждены хабом и в ожидание не вошли');
