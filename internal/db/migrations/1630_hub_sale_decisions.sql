-- Решения «чья продажа» хаба в ledger (ревью экосистемы, круг 2; хаб — hub#1280).
--
-- Джоб hub-decisions раз в 5 минут читает ленту хаба GET /api/v1/agent/sale-decisions/feed по курсору
-- (cursors 'hub:sale_decisions') ключом ledger в хабе (read:money): каждое изменение решения о том, чья продажа, от любого
-- писателя — спор владельца, дверь агента, перенос из ledger, привязка, отмена. Изменение несёт решение, как оно есть
-- сейчас; hub_sale_decisions — зеркало: каждое решение по id хаба, только чтение. Изменение решения на странице вне
-- ключа хаб придерживает и называет на каждом чтении (hub_sale_decisions_held), потом ставит на новое место в конце
-- ленты, и оно приходит обычным курсором.
--
-- Что читает зеркало, у продаж дня settings.sale_decisions_hub_from и позже (пусто — ничего):
--   общаки — действующее решение вида pot о продаже из hub_sales становится покупкой общака (pot_sales.hub_decision_id),
--   отменённое или заменённое решение снимает её; ledger pot add-sale такую продажу вручную больше не берёт;
--   договорённость у строки GET /v1/reports — действующее решение хаба о продаже.
-- У продаж раньше этого дня всё по-прежнему: покупки общака пишутся вручную, договорённость — из sale_decisions.
--
-- Миграция только добавляет: две таблицы, колонку (NULL), индекс и описания. Денег не двигает.

CREATE TABLE hub_sale_decisions (
  id                  INTEGER PRIMARY KEY,
  transaction_ref     TEXT,
  page_label          TEXT,
  kind                TEXT NOT NULL,
  shares              TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(shares)),
  pot                 TEXT,
  outcome             TEXT NOT NULL DEFAULT '',
  rule_home           TEXT,
  state               TEXT NOT NULL,
  source              TEXT NOT NULL DEFAULT '',
  said_by             TEXT,
  said_at             TEXT,
  decided_at          TEXT,
  note                TEXT,
  applied_person      TEXT,
  applied_report_date TEXT,
  ledger_decision_id  INTEGER,
  seq                 INTEGER NOT NULL,
  change              TEXT NOT NULL,
  changed_at          TEXT NOT NULL,
  read_at             TEXT NOT NULL
);
CREATE INDEX hub_sale_decisions_ref ON hub_sale_decisions(transaction_ref) WHERE transaction_ref IS NOT NULL;

CREATE TABLE hub_sale_decisions_held (
  seq           INTEGER PRIMARY KEY,
  decision_id   INTEGER NOT NULL,
  page_label    TEXT,
  reason        TEXT NOT NULL DEFAULT '',
  first_seen_at TEXT NOT NULL,
  cleared_at    TEXT
);

ALTER TABLE pot_sales ADD COLUMN hub_decision_id INTEGER;
CREATE UNIQUE INDEX pot_sales_hub_decision ON pot_sales(hub_decision_id) WHERE hub_decision_id IS NOT NULL AND removed_at IS NULL;

INSERT INTO schema_docs(tbl, col, description) VALUES
 ('hub_sale_decisions','', 'Зеркало решений «чья продажа» хаба (лента GET /api/v1/agent/sale-decisions/feed, hub#1280; джоб hub-decisions): каждое решение по id хаба, как оно есть сейчас, только чтение — пишет его хаб (спор владельца, hub sale decide, перенос из ledger). Имён фанов нет: продажа — по id хаба. У продаж с settings.sale_decisions_hub_from по нему ведутся покупки общаков (kind pot) и договорённость у строки отчёта'),
 ('hub_sale_decisions','id','id решения в хабе'),
 ('hub_sale_decisions','transaction_ref','Продажа хаба (transactions.id, как hub_sales.transaction_ref); NULL — решение не о продаже (off_platform) или продажа ещё не привязана'),
 ('hub_sale_decisions','page_label','Страница хаба; NULL — не названа'),
 ('hub_sale_decisions','kind','owner — вся продажа одного человека; split — доли; pot — в общак (pot); off_platform — деньги мимо платформы; exclude — никому'),
 ('hub_sale_decisions','shares','Кому и сколько: JSON [{"person": "<people.slug>" | null, "userId": <логин хаба>, "percent": <целое>}]; у pot и exclude пусто'),
 ('hub_sale_decisions','pot','Общак у kind = pot: название общака в хабе — код общака ledger (pots.slug)'),
 ('hub_sale_decisions','outcome','Как решено в хабе: first, second, half, nobody — исход спора; imported — перенесено из ledger; said — записано со слов'),
 ('hub_sale_decisions','rule_home','Чьей продажа была по правилу хаба, когда решали (shift, nobody, pot…); NULL — неизвестно'),
 ('hub_sale_decisions','state','active — действует; superseded — заменено новым решением; cancelled — отменено'),
 ('hub_sale_decisions','source','dispute — владелец в споре; ledger — перенесено из ledger (ledger_decision_id); agent — записано агентом со слов (hub sale decide)'),
 ('hub_sale_decisions','said_by','Кто решил: владелец или какие чаттеры, свободный текст; NULL — не записано'),
 ('hub_sale_decisions','said_at','Когда сказано, МСК; NULL — не записано'),
 ('hub_sale_decisions','decided_at','Когда решение записано в хабе, МСК'),
 ('hub_sale_decisions','note','Почему так (заметка решения); стирание фана в хабе очищает её'),
 ('hub_sale_decisions','applied_person','В чьём отчёте по посту решение учтено (people.slug); NULL — не отмечено'),
 ('hub_sale_decisions','applied_report_date','Дата того отчёта ГГГГ-ММ-ДД'),
 ('hub_sale_decisions','ledger_decision_id','Номер решения ledger (sale_decisions.id), из которого оно перенесено; NULL — записано в хабе'),
 ('hub_sale_decisions','seq','Место последнего прочитанного изменения решения в ленте хаба'),
 ('hub_sale_decisions','change','Последнее прочитанное изменение: made, superseded, cancelled, linked (привязано к продаже), applied, changed'),
 ('hub_sale_decisions','changed_at','Когда это изменение произошло в хабе, МСК'),
 ('hub_sale_decisions','read_at','Когда строка последний раз изменилась здесь, МСК'),
 ('hub_sale_decisions_held','', 'Изменения решений «чья продажа», которые хаб придержал от ключа ledger: решение на странице вне страниц ключа или без страницы. Пока изменение не пришло (cleared_at пуст), ledger этого решения не видит: с settings.sale_decisions_hub_from — проблема проверки «решения хаба о продажах доходят»'),
 ('hub_sale_decisions_held','seq','Место изменения в ленте хаба, пока оно придержано (отпущенное хаб ставит на новое место)'),
 ('hub_sale_decisions_held','decision_id','Решение хаба (hub_sale_decisions.id, когда придёт)'),
 ('hub_sale_decisions_held','page_label','Страница решения; NULL — не названа'),
 ('hub_sale_decisions_held','reason','Почему придержано: page_outside_key — страница вне страниц ключа ledger'),
 ('hub_sale_decisions_held','first_seen_at','Когда ledger впервые увидел изменение придержанным, МСК'),
 ('hub_sale_decisions_held','cleared_at','Когда хаб перестал называть изменение придержанным, МСК; NULL — придержано сейчас. Отпущенное изменение хаб ставит в конец ленты, и решение приходит следующим чтением (hub_sale_decisions); решения, которого в хабе больше нет, не будет'),
 ('pot_sales','hub_decision_id','Решение хаба вида pot (hub_sale_decisions.id), по которому покупку записал джоб hub-decisions; такая покупка снимается отменой или заменой решения в хабе, не вручную. NULL — покупка записана вручную (ledger pot add-sale)');

UPDATE schema_docs
   SET description = 'Покупки общаков (R3): одна строка — одна покупка; снятая остаётся строкой с removed_at. Пишет агент (reports) или владелец (decide): ledger pot add-sale, remove-sale. Продажу хаба дня settings.sale_decisions_hub_from и позже в общак отдаёт только решение хаба вида pot: покупку пишет и снимает джоб hub-decisions (hub_decision_id), вручную остаются деньги, которых хаб не видит, и продажи раньше этого дня'
 WHERE tbl = 'pot_sales' AND col = '';
UPDATE schema_docs
   SET description = 'agent — агент; report_line — из строки сохранённого отчёта (report_id, report_n); owner — владелец. У покупки по решению хаба (hub_decision_id) — кто записал решение: owner — владелец в споре, agent — агент со слов или перенос из ledger'
 WHERE tbl = 'pot_sales' AND col = 'source';
UPDATE schema_docs
   SET description = 'Ключ API, которым записано; hub-decisions — джоб по решению хаба'
 WHERE tbl = 'pot_sales' AND col = 'key_name';
UPDATE schema_docs
   SET description = 'Договорённости «чья продажа» (R3): решение владельца или договорённость чаттеров о продаже или ожидаемой продаже — кому и в каких долях, кто сказал, когда, ссылка на сообщение, в каком отчёте учтено. Пишет агент (право reports) или владелец (decide); прежний файл памяти агента — зеркало: ledger decision list --md. Ничего не проводит. С settings.sale_decisions_hub_from новых строк нет: решения пишет хаб, они приходят в hub_sale_decisions; эта таблица — история'
 WHERE tbl = 'sale_decisions' AND col = '';
UPDATE settings
   SET note = 'С какого дня (МСК) у «чья продажа» один дом — хаб. С этого дня решения, вопросы о ней и слово владельца о ничьей продаже пишет только хаб (hub sale decide|ask|answer): ledger decision add|applied|cancel, question add|answer|drop и accruals claim|unclaim отказывают, списки остаются историей. У продаж этого дня и позже: правило фактов читает решение хаба (никому, в общак, доли, ничья — человеку), покупки общаков ведёт джоб hub-decisions по решениям хаба вида pot (ledger pot add-sale такую продажу не берёт), договорённость у строки отчёта — решение хаба (hub_sale_decisions). У продаж раньше действуют записи ledger. Пусто — пишет и ledger, решения хаба ни на что не влияют. Ставит ledger decision hub-from'
 WHERE key = 'sale_decisions_hub_from';
