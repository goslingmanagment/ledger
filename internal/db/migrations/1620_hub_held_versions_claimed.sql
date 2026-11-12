-- Ревью экосистемы, круг 2 (хаб — hub#1279).
--
-- Придержанные версии. Лента сданных отчётов хаба называет в held версии, которые она придержала от ключа ledger (строка
-- версии — на странице вне страниц ключа), на каждом чтении, пока ключ не сможет их прочитать; потом ставит версию на
-- новое место в конце ленты, и она приходит в reports обычным курсором. hub_shift_report_held — каждая такая версия, пока
-- хаб её называет: проблема проверки «сданные в хабе отчёты сохраняются». Версия пришла или пропала из списка хаба —
-- строка остаётся с cleared_at. Версию ledger узнаёт по id, а не по месту в ленте.
--
-- Спорное «моё». Строка «моё» на продажу, которую правило отдаёт другому, в версии хаба теперь считает 0 (amountMills) и
-- говорит, сколько заявлено (claimedMills): hub_shift_report_lines.claimed_micros. У версий, прочитанных до hub#1279,
-- такая строка входила в итог целиком.
--
-- Решение хаба в фактах. Факты продаж хаба несут его действующее решение «чья продажа»: resolution — доли решения,
-- которое отдаёт продажу людям (и ничью продажу, отданную человеку), decision {kind, pot} — split, nobody («никому») или
-- pot. Правило фактов (internal/split) читает их у продаж дня settings.sale_decisions_hub_from и позже; у продаж раньше
-- этого дня и пока настройка пуста действуют записи ledger, как прежде. Здесь — только описания.
--
-- Миграция только добавляет: таблицу, колонку (NULL) и описания.

CREATE TABLE hub_shift_report_held (
  id            INTEGER PRIMARY KEY,
  seq           INTEGER NOT NULL DEFAULT 0,
  shift_id      INTEGER NOT NULL DEFAULT 0,
  person        TEXT,
  report_date   TEXT NOT NULL DEFAULT '',
  reason        TEXT NOT NULL DEFAULT '',
  page_labels   TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(page_labels)),
  first_seen_at TEXT NOT NULL,
  cleared_at    TEXT
);
CREATE INDEX hub_shift_report_held_open ON hub_shift_report_held(id) WHERE cleared_at IS NULL;

ALTER TABLE hub_shift_report_lines ADD COLUMN claimed_micros INTEGER;

INSERT INTO schema_docs(tbl, col, description) VALUES
 ('hub_shift_report_held','', 'Версии сданных в хабе отчётов, которые хаб придержал от ключа ledger: строка версии — на странице вне страниц ключа (hub#1279). Пока версия не пришла (cleared_at пуст), она — проблема проверки «сданные в хабе отчёты сохраняются»; ключу дают страницу в хабе, и версия приходит сама. Денег и строк версии здесь нет'),
 ('hub_shift_report_held','id','id версии в хабе (hub_shift_reports.id, когда придёт)'),
 ('hub_shift_report_held','seq','Место версии в ленте хаба, пока она придержана (отпущенную хаб ставит на новое место)'),
 ('hub_shift_report_held','shift_id','Смена хаба'),
 ('hub_shift_report_held','person','Код человека, как прислал хаб (people.slug); NULL — у логина в хабе нет человека'),
 ('hub_shift_report_held','report_date','Дата отчёта версии ГГГГ-ММ-ДД (МСК)'),
 ('hub_shift_report_held','reason','Почему придержана: page_outside_key — страница вне страниц ключа ledger'),
 ('hub_shift_report_held','page_labels','Страницы версии вне ключа: JSON-массив page_label'),
 ('hub_shift_report_held','first_seen_at','Когда ledger впервые увидел версию придержанной, МСК'),
 ('hub_shift_report_held','cleared_at','Когда хаб перестал называть версию придержанной или она пришла, МСК; NULL — придержана сейчас. Пришла ли она — есть ли её id в hub_shift_reports: отпущенную версию хаб ставит в конец ленты, и она приходит следующим чтением; версии, которой в хабе больше нет, не будет'),
 ('hub_shift_report_lines','claimed_micros','Сколько заявляет спорное «моё» (claimedMills хаба), микродоллары: такая строка в итог не входит (amount_micros 0), пока спор не решён; NULL — строка не спорная, или версия прочитана до hub#1279');

UPDATE schema_docs
   SET description = '1 — «моё» на продажу, которую правило отдаёт другому. В итог такая строка не входит (amount_micros 0, counted 0), сколько заявлено — claimed_micros; решение спора приходит новой версией. У версий, прочитанных до hub#1279, спорная строка входила в итог целиком (claimed_micros NULL)'
 WHERE tbl = 'hub_shift_report_lines' AND col = 'disputed';

UPDATE schema_docs
   SET description = 'Почему так (split.Reason…: ppv_sender, last_writer, holder, post_tip, campaign, no_last_writer, marks, owner_split, owner_claim — ничья продажа, отданная человеку: по заявке ledger (sale_claims) или, у продаж с settings.sale_decisions_hub_from, по решению хаба; owner_nobody — решение хаба «никому»; owner_pot — решение хаба «в общак» (pot)…)'
 WHERE tbl = 'fact_homes' AND col = 'reason';
UPDATE schema_docs
   SET description = 'Какой общак: obshak — чаевые под постом и Campaign по правилу; у reason owner_pot — код общака ledger (pots.slug), которому продажу отдало решение хаба'
 WHERE tbl = 'fact_homes' AND col = 'pot';
UPDATE schema_docs
   SET description = 'Продажа как в ответе хаба (JSON): держатели, писавшие, метки, решение владельца (resolution) и действующее решение хаба decision {kind: split | nobody | pot, pot}. Решение хаба правило фактов читает у продаж с settings.sale_decisions_hub_from'
 WHERE tbl = 'hub_sales' AND col = 'payload';
UPDATE settings
   SET note = 'С какого дня (МСК) у «чья продажа» один дом — хаб. С этого дня решения, вопросы о ней и слово владельца о ничьей продаже пишет только хаб (hub sale decide|ask|answer): ledger decision add|applied|cancel, question add|answer|drop и accruals claim|unclaim отказывают, списки остаются историей. У продаж этого дня и позже правило фактов читает решение хаба (никому, в общак, доли, ничья — человеку); у продаж раньше действуют записи ledger. Пусто — пишет и ledger, решения хаба не читаются. Ставит ledger decision hub-from'
 WHERE key = 'sale_decisions_hub_from';
