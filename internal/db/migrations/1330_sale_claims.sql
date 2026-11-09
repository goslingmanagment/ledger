-- Выпуск 2, решение владельца 3 (план 2026-11-09): продажу, которую по правилу дележа не держала ни одна смена («ничей»),
-- человек может заявить сам; заявка идёт через владельца и пишется сюда («продажа N — человеку X, по слову владельца»).
-- Строки не меняются и не удаляются: снятая заявка — новая строка unclaim, действует последняя строка продажи.
-- Теневая пересборка (internal/accruals) отдаёт заявленную ничью продажу человеку целиком: день продажи по МСК, ставка
-- его вкладки модели страницы, причина owner_claim. Продажа, которая не ничья (смена, отправитель PPV, общак, решение,
-- по-старому), заявкой не меняется.

CREATE TABLE sale_claims (
  id              INTEGER PRIMARY KEY,
  transaction_ref TEXT NOT NULL,
  action          TEXT NOT NULL CHECK (action IN ('claim','unclaim')),
  person          TEXT REFERENCES people(slug),
  note            TEXT NOT NULL CHECK (trim(note) <> ''),
  recorded_by     TEXT NOT NULL,
  recorded_at     TEXT NOT NULL,
  CHECK ((action = 'claim') = (person IS NOT NULL))
);
CREATE INDEX sale_claims_ref ON sale_claims(transaction_ref, id);
CREATE TRIGGER sale_claims_immutable_u BEFORE UPDATE ON sale_claims BEGIN SELECT RAISE(ABORT, 'sale claims are immutable: add an unclaim'); END;
CREATE TRIGGER sale_claims_immutable_d BEFORE DELETE ON sale_claims BEGIN SELECT RAISE(ABORT, 'sale claims are immutable'); END;

CREATE VIEW v_sale_claims AS
  SELECT c.transaction_ref AS transaction_ref,
         c.person AS person,
         COALESCE(p.name, c.person) AS name,
         c.note AS note,
         c.recorded_by AS recorded_by,
         c.recorded_at AS recorded_at,
         c.id AS id,
         (SELECT h.home FROM fact_homes h WHERE h.transaction_ref = c.transaction_ref ORDER BY h.seq LIMIT 1) AS home,
         (SELECT h.reason FROM fact_homes h WHERE h.transaction_ref = c.transaction_ref ORDER BY h.seq LIMIT 1) AS reason,
         (SELECT h.day FROM fact_homes h WHERE h.transaction_ref = c.transaction_ref ORDER BY h.seq LIMIT 1) AS day
  FROM sale_claims c
  LEFT JOIN people p ON p.slug = c.person
  WHERE c.action = 'claim'
    AND c.id = (SELECT MAX(x.id) FROM sale_claims x WHERE x.transaction_ref = c.transaction_ref);

INSERT INTO schema_docs(tbl, col, description) VALUES
 ('sale_claims','', 'Заявки на ничьи продажи хаба по слову владельца: продажа — человеку. Строки не меняются: снятие — строка unclaim, действует последняя строка продажи. Учитывает теневая пересборка (fact_homes reason owner_claim)'),
 ('sale_claims','id','Номер строки; у продажи действует строка с наибольшим'),
 ('sale_claims','transaction_ref','Продажа (hub_sales.transaction_ref)'),
 ('sale_claims','action','claim — продажа человеку; unclaim — заявка снята, продажа снова по правилу'),
 ('sale_claims','person','Кому (people.slug); NULL у unclaim'),
 ('sale_claims','note','Слово владельца: кто заявил и почему'),
 ('sale_claims','recorded_by','Кто записал: ключ агента или LEDGER_AUTHOR'),
 ('sale_claims','recorded_at','Когда записано, МСК'),
 ('v_sale_claims','', 'Действующие заявки (последняя строка продажи — claim) и что с продажей в тени сейчас: reason owner_claim — учтена; иное — продажа не ничья или нет в доказанных фактах (home NULL), заявка не учтена'),
 ('v_sale_claims','transaction_ref','Продажа'),
 ('v_sale_claims','person','Кому заявлена (people.slug)'),
 ('v_sale_claims','name','Имя'),
 ('v_sale_claims','note','Слово владельца'),
 ('v_sale_claims','recorded_by','Кто записал'),
 ('v_sale_claims','recorded_at','Когда записано, МСК'),
 ('v_sale_claims','id','Строка sale_claims'),
 ('v_sale_claims','home','Чья продажа в fact_homes сейчас (первая доля); NULL — продажи нет в доказанных фактах'),
 ('v_sale_claims','reason','Почему: owner_claim — заявка учтена'),
 ('v_sale_claims','day','День МСК, на который продажа в fact_homes');

UPDATE schema_docs
   SET description = 'Почему так (split.Reason…: ppv_sender, last_writer, holder, post_tip, campaign, no_last_writer, marks, owner_split, owner_claim — ничья продажа по заявке, sale_claims…)'
 WHERE tbl = 'fact_homes' AND col = 'reason';
UPDATE schema_docs
   SET description = 'Смена или заход, которому доля (hub_works.ref); NULL — доля без держащей работы (решение владельца, заявка)'
 WHERE tbl = 'fact_homes' AND col = 'work_ref';
UPDATE schema_docs
   SET description = 'Права через запятую: read — SQL и схема, propose — предложения и споры, decide — решения владельца: бриф, правила, споры, заявки на ничьи продажи (только openclaw-main), collect — сборщики, import — разовые импорты, payouts — только GET /v1/payouts и GET /v1/payouts/prepare (выплаты чаттерам для хаба), больше ничего'
 WHERE tbl = 'keys' AND col = 'scopes';
