ALTER TABLE hub_sale_decisions ADD COLUMN applied_deliveries TEXT NOT NULL DEFAULT '[]';

INSERT OR REPLACE INTO schema_docs(tbl, col, description) VALUES
 ('hub_sale_decisions','applied_deliveries','JSON-массив подтверждённых доставок по получателям [{person, reportDate}] из ленты решений хаба');
