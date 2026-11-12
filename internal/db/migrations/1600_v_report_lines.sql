-- Строки отчётов, откуда бы ни пришёл отчёт (ревью экосистемы, R5б): черновик агента (shift_report_lines действующих
-- версий) и отчёт, сданный в хабе (hub_shift_report_lines действующих и сохранённых версий), одним видом. По нему защита
-- общака от двойного учёта узнаёт продажу, уже учтённую в отчёте человека, из любого источника. Пост «Отчётности» строк
-- продаж не имеет. Миграция только добавляет вид.

CREATE VIEW v_report_lines AS
  SELECT 'agent' AS source, r.id AS report_id, l.n AS line, r.person, COALESCE(r.person, r.hub_username) AS who, r.report_date, l.transaction_ref,
         l.page_label, l.net_micros, l.counted
    FROM shift_report_lines l JOIN shift_reports r ON r.id = l.report_id
   WHERE r.status <> 'superseded' AND l.transaction_ref IS NOT NULL
  UNION ALL
  SELECT 'hub', r.id, l.seq, r.person, COALESCE(r.person, r.hub_person, ''), r.report_date, l.transaction_ref,
         l.page_label, l.amount_micros, l.counted
    FROM hub_shift_report_lines l JOIN hub_shift_reports r ON r.id = l.report_id
   WHERE r.current = 1 AND r.problem IS NULL;

INSERT INTO schema_docs(tbl, col, description) VALUES
 ('v_report_lines','', 'Строки продаж отчётов из любого источника: agent — черновик агента (действующая версия), hub — отчёт, сданный в хабе (действующая сохранённая версия). Защита общака от двойного учёта читает его'),
 ('v_report_lines','source','agent — черновик агента (shift_reports), hub — отчёт, сданный в хабе (hub_shift_reports)'),
 ('v_report_lines','report_id','Отчёт: shift_reports.id или hub_shift_reports.id (версия хаба)'),
 ('v_report_lines','line','Номер строки в отчёте (n черновика, seq версии хаба)'),
 ('v_report_lines','person','Человек (people.slug); NULL — не узнан'),
 ('v_report_lines','who','Кто: код человека, а у черновика без человека — логин хаба'),
 ('v_report_lines','report_date','Дата отчёта (МСК)'),
 ('v_report_lines','transaction_ref','Продажа хаба (transactions.id)'),
 ('v_report_lines','page_label','Страница'),
 ('v_report_lines','net_micros','Сумма строки, микродоллары: net черновика, учтённое строкой версии хаба'),
 ('v_report_lines','counted','1 — строка в итоге отчёта человека');
