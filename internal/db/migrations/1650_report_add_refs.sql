-- Добавка называет свою продажу (экосистема, «долёт»).
--
-- Сумма, добавленная в отчёт агента руками (shift_report_lines.added = 1), чаще всего — настоящая продажа хаба,
-- оплаченная позже: PPV, купленный после отчёта; продление, зачтённое по слову владельца. Строка добавки не несла
-- ссылки на продажу, и дальше никто не знал, что продажа уже оплачена отчётом: сверка хаба «чья продажа по правилу»
-- называла её «нет ни в одном отчёте», а защита от двойного учёта её не видела. Теперь добавка может назвать продажу
-- (adds[].transaction_ref в POST /v1/reports), и ссылка пишется в shift_report_lines.transaction_ref её строки —
-- любая из двух ссылок продажи: ссылка площадки, как у строк, или id хаба; их сводит hub_transactions.
--
-- Колонка есть у всех строк, индекс и ограничения добавке со ссылкой не мешают. Вид v_report_lines такие строки уже
-- отдаёт — он берёт каждую строку со ссылкой, — поэтому защита общака видит их сама. Миграция пересоздаёт вид с
-- колонкой added, чтобы отказ и запросы агентов отличали добавленную сумму от строки продажи, и уточняет описания:
-- добавка со ссылкой связана с продажей, а значит и с её фаном — стирание фана стирает и её объяснение (what).
-- Данных не меняет.

DROP VIEW v_report_lines;

CREATE VIEW v_report_lines AS
  SELECT 'agent' AS source, r.id AS report_id, l.n AS line, r.person, COALESCE(r.person, r.hub_username) AS who, r.report_date, l.transaction_ref,
         l.page_label, l.net_micros, l.counted, l.added
    FROM shift_report_lines l JOIN shift_reports r ON r.id = l.report_id
   WHERE r.status <> 'superseded' AND l.transaction_ref IS NOT NULL
  UNION ALL
  SELECT 'hub', r.id, l.seq, r.person, COALESCE(r.person, r.hub_person, ''), r.report_date, l.transaction_ref,
         l.page_label, l.amount_micros, l.counted, l.added
    FROM hub_shift_report_lines l JOIN hub_shift_reports r ON r.id = l.report_id
   WHERE r.current = 1 AND r.problem IS NULL;

INSERT INTO schema_docs(tbl, col, description) VALUES
 ('v_report_lines','added','1 — добавленная сумма: у agent — добавка, которая называет свою продажу (adds[].transaction_ref), у hub — «долёт», продажа дней до смены; 0 — продажа окна смены');

UPDATE schema_docs
   SET description = 'Строки отчётов, которые называют продажу, из любого источника: agent — черновик агента (действующая версия): продажи окна и добавленные суммы, которые называют свою продажу; hub — отчёт, сданный в хабе (действующая сохранённая версия). Защита общака от двойного учёта читает его'
 WHERE tbl = 'v_report_lines' AND col = '';
UPDATE schema_docs
   SET description = 'Номер строки в отчёте (n черновика, seq версии хаба); NULL у добавленной суммы черновика'
 WHERE tbl = 'v_report_lines' AND col = 'line';
UPDATE schema_docs
   SET description = 'Продажа: у строк agent — ссылка площадки (hub_transactions.ref), у добавленной суммы agent — любая из двух ссылок продажи, у строк hub — id хаба (hub_transactions.key); одну продажу в двух источниках и под двумя ссылками сводит hub_transactions'
 WHERE tbl = 'v_report_lines' AND col = 'transaction_ref';
UPDATE schema_docs
   SET description = 'Сумма строки, микродоллары: net черновика (у добавленной суммы — сколько добавлено), учтённое строкой версии хаба'
 WHERE tbl = 'v_report_lines' AND col = 'net_micros';
UPDATE schema_docs
   SET description = '1 — строка в итоге отчёта человека; добавленная сумма черновика в итоге всегда'
 WHERE tbl = 'v_report_lines' AND col = 'counted';

UPDATE schema_docs
   SET description = 'Продажа, как её прислал агент. У строки продажи — ссылка площадки на транзакцию (hub_transactions.ref), а не id хаба (hub_sales.transaction_ref). У добавленной суммы (added = 1) — продажа, которую она оплачивает («долёт»: продажа хаба, оплаченная позже), любой из двух ссылок: ссылкой площадки или id хаба; NULL — добавка продажу не называет (деньги мимо хаба). С hub_sales, hub_sale_decisions и покупками общака сводить через hub_transactions (key — id хаба, ref — ссылка площадки)'
 WHERE tbl = 'shift_report_lines' AND col = 'transaction_ref';
UPDATE schema_docs
   SET description = '1 — сумма добавлена руками (вне окна или мимо хаба), объяснение в what; продажу хаба, если это она, называет transaction_ref'
 WHERE tbl = 'shift_report_lines' AND col = 'added';
UPDATE schema_docs
   SET description = 'Объяснение добавленной суммы; «фан стёрт в хабе» — у добавки, которая называет продажу стёртого фана (стирание фанов, hub_erasures)'
 WHERE tbl = 'shift_report_lines' AND col = 'what';
UPDATE schema_docs
   SET description = 'transactionRef хаба — ссылка площадки на транзакцию: её несут строки отчётов агента (shift_report_lines.transaction_ref; добавленная сумма может нести и id хаба — key); по ней строка сверяется с hub_exceptions. id той же продажи в хабе — key'
 WHERE tbl = 'hub_transactions' AND col = 'ref';
