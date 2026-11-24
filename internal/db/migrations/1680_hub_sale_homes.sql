CREATE TABLE hub_sale_homes (
  transaction_id INTEGER PRIMARY KEY,
  seq INTEGER NOT NULL CHECK (seq > 0),
  retracted INTEGER NOT NULL CHECK (retracted IN (0,1)),
  payload TEXT NOT NULL CHECK (json_valid(payload))
);

CREATE TABLE hub_sale_homes_held (
  seq INTEGER PRIMARY KEY,
  transaction_id INTEGER NOT NULL,
  reason TEXT NOT NULL
);

-- A generation guards an outstanding HTTP request against a concurrent CLI or job read.
CREATE TABLE hub_sale_homes_sync (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
  generation INTEGER NOT NULL DEFAULT 0,
  last_ok TEXT,
  state TEXT NOT NULL DEFAULT 'new' CHECK (state IN ('new','absent','reading','ok','error')),
  error TEXT NOT NULL DEFAULT ''
);
INSERT INTO hub_sale_homes_sync(singleton) VALUES (1);

-- An unresolved share keeps its login and mills, and has no payable day.
ALTER TABLE fact_homes ALTER COLUMN day DROP NOT NULL;

-- Previously proven facts need a complete read of the new source before payroll uses them.
UPDATE fact_weeks SET status = 'unknown', reasons = '["лента домов хаба ещё не читалась"]';

INSERT INTO schema_docs(tbl,col,description) VALUES
 ('hub_sale_homes','','Последнее изменение дома продажи из ленты хаба, включая отзывы'),
 ('hub_sale_homes','transaction_id','Номер транзакции хаба; в hub_sales это transaction_ref'),
 ('hub_sale_homes','seq','Наибольшее применённое место изменения этой продажи в ленте'),
 ('hub_sale_homes','retracted','1 — дом отозван, его место защищает от повтора старого изменения'),
 ('hub_sale_homes','payload','Изменение хаба целиком: страница, время, сумма в mills, дом, причина и доли с человеком и днём отчёта'),
 ('hub_sale_homes_held','','Текущий список изменений, придержанных хабом по гранту ключа'),
 ('hub_sale_homes_held','seq','Место придержанного изменения в ленте'),
 ('hub_sale_homes_held','transaction_id','Номер транзакции хаба'),
 ('hub_sale_homes_held','reason','Причина, по которой хаб придержал изменение'),
 ('hub_sale_homes_sync','','Состояние чтения ленты домов; курсор хранится в cursors под именем hub:sale_homes'),
 ('hub_sale_homes_sync','singleton','Единственная строка состояния'),
 ('hub_sale_homes_sync','generation','Номер записи состояния для согласования одновременных чтений'),
 ('hub_sale_homes_sync','last_ok','Последнее чтение до конца ленты (МСК)'),
 ('hub_sale_homes_sync','state','Новое, лента ещё не выкачена, чтение страниц, успех или ошибка'),
 ('hub_sale_homes_sync','error','Причина последней ошибки чтения');
UPDATE schema_docs SET description = 'История заявок ledger до перехода на дома продаж хаба' WHERE tbl = 'sale_claims' AND col = '';
UPDATE schema_docs SET description = 'История правил дележа ledger до перехода на дома продаж хаба' WHERE tbl = 'split_rules' AND col = '';
UPDATE schema_docs SET description = 'Доли продаж из hub_sale_homes, с человеком, логином и днём из хаба' WHERE tbl = 'fact_homes' AND col = '';
UPDATE schema_docs SET description = 'Дата отчёта доли по хабу; NULL — хаб ещё не установил день. У дома без долей — день продажи (МСК)' WHERE tbl='fact_homes' AND col='day';
UPDATE schema_docs SET description = 'Код человека доли из хаба; у старого логина без кода — по aliases карточки' WHERE tbl='fact_homes' AND col='person';
UPDATE schema_docs SET description = 'Причина дома, вычисленная хабом' WHERE tbl='fact_homes' AND col='reason';
UPDATE schema_docs SET description = 'Дата отчёта работы из хаба; в старых снимках — день середины работы (МСК)' WHERE tbl='hub_works' AND col='day';
UPDATE settings SET note = 'День перехода решений, вопросов, покупок общаков и договорённостей строк отчётов в хаб. Факты всегда читают дома продаж хаба' WHERE key='sale_decisions_hub_from';
UPDATE schema_docs SET description = 'Активные продажи недельных снимков хаба. Используются для проверки полноты домов продаж и контекста смен' WHERE tbl='hub_sales' AND col='';
