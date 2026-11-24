CREATE TABLE hub_pot_distributions (
 id INTEGER PRIMARY KEY, seq INTEGER NOT NULL CHECK(seq>0), state TEXT NOT NULL CHECK(state IN ('posted','reversed')),
 pot_code TEXT NOT NULL, pot_name TEXT NOT NULL, model_slug TEXT NOT NULL, day TEXT NOT NULL,
 formula_kind TEXT NOT NULL CHECK(formula_kind IN ('equal','weights','manual')),
 formula_params TEXT NOT NULL CHECK(json_valid(formula_params)), base_mills INTEGER NOT NULL,
 lines_json TEXT NOT NULL CHECK(json_valid(lines_json)), at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE INDEX hub_pot_distributions_day ON hub_pot_distributions(day);
CREATE TABLE hub_pot_distribution_inflows (
 distribution_id INTEGER NOT NULL REFERENCES hub_pot_distributions(id), transaction_id INTEGER NOT NULL,
 page_label TEXT NOT NULL, base_mills INTEGER NOT NULL, PRIMARY KEY(distribution_id,transaction_id)
);
CREATE INDEX hub_pot_inflows_tx ON hub_pot_distribution_inflows(transaction_id);
CREATE TABLE hub_pot_distribution_held (
 seq INTEGER PRIMARY KEY, distribution_id INTEGER NOT NULL, page_labels_json TEXT NOT NULL CHECK(json_valid(page_labels_json)), reason TEXT NOT NULL
);
CREATE TABLE hub_pot_sync (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1), generation INTEGER NOT NULL DEFAULT 0,
 state TEXT NOT NULL DEFAULT 'new' CHECK(state IN ('new','absent','reading','error','ok')), last_ok TEXT, error TEXT NOT NULL DEFAULT '', dirty INTEGER NOT NULL DEFAULT 0 CHECK(dirty IN (0,1))
);
INSERT INTO hub_pot_sync(singleton) VALUES(1);
INSERT INTO settings(key,value,note,updated_at) VALUES('pot_shares_hub_from','','Первый день раздач Workboard. Пусто — доли ledger.',CURRENT_TIMESTAMP);

ALTER TABLE report_accrual_entries ADD COLUMN hub_reports TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(hub_reports));
ALTER TABLE report_accrual_entries ADD COLUMN sale_refs TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(sale_refs));
ALTER TABLE report_accrual_entries ADD COLUMN hub_pots TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(hub_pots));

-- Preserve every owner's decision while extending the original reason constraint.
ALTER TABLE payroll_pending_changes RENAME TO payroll_pending_old;
CREATE TABLE payroll_pending_changes (
 id INTEGER PRIMARY KEY, person TEXT NOT NULL, model TEXT NOT NULL, name TEXT NOT NULL, date TEXT NOT NULL,
 posted_micros INTEGER NOT NULL, new_micros INTEGER NOT NULL,
 reason TEXT NOT NULL CHECK(reason IN ('post_edited','new_post','terms_changed','общак')),
 noticed_at TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'open' CHECK(state IN ('open','applied','dismissed')),
 decided_by TEXT, decided_at TEXT, note TEXT,
 sale_refs TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(sale_refs)), pot_change TEXT NOT NULL DEFAULT ''
);
INSERT INTO payroll_pending_changes(id,person,model,name,date,posted_micros,new_micros,reason,noticed_at,state,decided_by,decided_at,note)
 SELECT * FROM payroll_pending_old;
DROP TABLE payroll_pending_old;
CREATE UNIQUE INDEX payroll_pending_changes_open ON payroll_pending_changes(model,name,date) WHERE state='open';
CREATE INDEX payroll_pending_changes_person ON payroll_pending_changes(person,state);

-- Link to the exact tab day, including names containing SQL wildcard characters.
CREATE VIEW v_booked_report_days AS
 SELECT a.*, e.effective_at AS day, substr(l.account,9) AS person,
        substr(e.source_key, length('accrual:payroll:')+1,
               instr(e.source_key, ':'||e.effective_at||'#')-length('accrual:payroll:')-1) AS tab
 FROM report_accrual_entries a JOIN entries e ON e.id=a.entry_id
 JOIN lines l ON l.entry_id=e.id AND l.account LIKE 'payable:%'
 WHERE NOT EXISTS(SELECT 1 FROM entries r WHERE r.reverses_id=e.id);

-- Before this migration the exact version was not recorded. Reserve all historical candidates on the
-- booked person's tab day, including versions removed before an owner dismissed a change.
CREATE VIEW v_pot_report_history AS
 WITH history AS (
   SELECT r.person, r.report_date AS day, l.page_label, l.transaction_ref
   FROM hub_shift_reports r JOIN hub_shift_report_lines l ON l.report_id=r.id WHERE l.counted=1
   UNION ALL
   SELECT r.person,r.report_date,l.page_label,l.transaction_ref
   FROM shift_reports r JOIN shift_report_lines l ON l.report_id=r.id WHERE l.counted=1 AND l.transaction_ref IS NOT NULL
 )
 SELECT h.*, coalesce((SELECT j.value FROM settings s,json_each(s.value) j
                        WHERE s.key='hub_model_slugs' AND j.key=p.model_slug),p.model_slug) AS model
 FROM history h LEFT JOIN hub_pages p ON p.page_label=h.page_label;

UPDATE report_accrual_entries AS a SET sale_refs=(
 SELECT json_group_array(DISTINCT h.transaction_ref) FROM v_booked_report_days b
 JOIN v_pot_report_history h ON h.person=b.person AND h.day=b.day WHERE b.entry_id=a.entry_id
 AND (h.model IS NULL OR h.model=substr(b.tab,1,instr(b.tab,'/')-1) COLLATE NOCASE)
) WHERE EXISTS(SELECT 1 FROM v_booked_report_days b WHERE b.entry_id=a.entry_id);
UPDATE payroll_pending_changes AS c SET sale_refs=(
 SELECT json_group_array(DISTINCT h.transaction_ref) FROM v_pot_report_history h WHERE h.person=c.person AND h.day=c.date AND (h.model IS NULL OR h.model=c.model COLLATE NOCASE)
) WHERE state='open';
DROP VIEW v_pot_report_history;

INSERT INTO schema_docs(tbl,col,description) VALUES
 ('hub_pot_distributions','','Последнее проведение или сторно раздачи Workboard'),
 ('hub_pot_distributions','id','Номер раздачи в хабе'), ('hub_pot_distributions','seq','Последнее место этой раздачи в ленте'),
 ('hub_pot_distributions','state','posted — проведена, reversed — сторнирована'), ('hub_pot_distributions','pot_code','Код общака'),
 ('hub_pot_distributions','pot_name','Название общака'), ('hub_pot_distributions','model_slug','Код модели хаба'),
 ('hub_pot_distributions','day','День начисления долей'), ('hub_pot_distributions','formula_kind','Формула раздачи'),
 ('hub_pot_distributions','formula_params','Параметры формулы JSON'), ('hub_pot_distributions','base_mills','База в тысячных доллара'),
 ('hub_pot_distributions','lines_json','Доли участников, база в тысячных доллара и основание'),
 ('hub_pot_distributions','at','Время изменения в Workboard'), ('hub_pot_distributions','updated_at','Время чтения ledger'),
 ('hub_pot_distribution_inflows','','Поступления, использованные раздачей'), ('hub_pot_distribution_inflows','distribution_id','Номер раздачи'),
 ('hub_pot_distribution_inflows','transaction_id','Номер продажи хаба'), ('hub_pot_distribution_inflows','page_label','Страница продажи'),
 ('hub_pot_distribution_inflows','base_mills','Часть поступления в тысячных доллара'),
 ('hub_pot_distribution_held','','Раздачи, придержанные хабом'), ('hub_pot_distribution_held','seq','Место в ленте'),
 ('hub_pot_distribution_held','distribution_id','Номер раздачи'), ('hub_pot_distribution_held','page_labels_json','Страницы вне ключа'),
 ('hub_pot_distribution_held','reason','Причина удержания'),
 ('hub_pot_sync','','Чтение ленты раздач хаба'), ('hub_pot_sync','singleton','Единственная строка'),
 ('hub_pot_sync','generation','Поколение чтения'), ('hub_pot_sync','state','Состояние чтения'),
 ('hub_pot_sync','last_ok','Последнее полное чтение'), ('hub_pot_sync','error','Ошибка чтения'),
 ('hub_pot_sync','dirty','После изменения зеркала ожидается пересчёт начислений'),
 ('report_accrual_entries','hub_reports','Версии отчётов хаба на момент проводки'),
 ('report_accrual_entries','sale_refs','Ссылки учтённых продаж на момент проводки'),
 ('report_accrual_entries','hub_pots','Доли раздач на момент проводки, с номерами, базой и основанием'),
 ('payroll_pending_changes','sale_refs','Продажи предлагаемой версии дня'),
 ('payroll_pending_changes','pot_change','Состав изменения общака, к которому относится решение'),
 ('v_booked_report_days','','Действующие проводки по отчётам с сохранённым составом дня'),
 ('v_booked_report_days','day','День начисления'), ('v_booked_report_days','person','Код человека'),
 ('v_booked_report_days','tab','Модель и название вкладки');
INSERT INTO schema_docs(tbl,col,description)
 SELECT 'v_booked_report_days',col,description FROM schema_docs WHERE tbl='report_accrual_entries' AND col<>'';
UPDATE schema_docs SET description=description||'. общак — изменились проведённые доли Workboard'
 WHERE tbl='payroll_pending_changes' AND col='reason';
