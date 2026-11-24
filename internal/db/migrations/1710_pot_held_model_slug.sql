ALTER TABLE hub_pot_distribution_held ADD COLUMN model_slug TEXT NOT NULL DEFAULT '';

INSERT INTO schema_docs(tbl,col,description) VALUES
 ('hub_pot_distribution_held','model_slug','Код модели удержанной раздачи. Пусто, если хаб его не передал.');
