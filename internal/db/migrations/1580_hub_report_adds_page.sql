-- «Мимо площадки», подтверждённое владельцем, в сданных в хабе отчётах (хаб — hub#1252, пакет G): на слово владельца хаб
-- делает новую версию отчёта, где подтверждённая сумма входит в сумму своей страницы (offPlatform[].pageLabel). ledger
-- сверяет страницу со строками вместе с такими суммами и хранит их засчитанными на этой странице (counted = 1, page_label),
-- а не как незасчитанную добавку. Начислений это не меняет: отчёт хаба начисляет только с settings.payroll_basis_hub_from.
-- Миграция только добавляет: колонку (NULL) и описания.

ALTER TABLE hub_shift_report_adds ADD COLUMN page_label TEXT;

INSERT INTO schema_docs(tbl, col, description) VALUES
 ('hub_shift_report_adds','page_label','Страница, в сумму которой входит подтверждённая владельцем сумма (offPlatform[].pageLabel); NULL — не подтверждена или без страницы');

UPDATE schema_docs
   SET description = '1 — подтверждено владельцем и входит в сумму страницы page_label этой версии; 0 — в итог версии не входит'
 WHERE tbl = 'hub_shift_report_adds' AND col = 'counted';

UPDATE schema_docs
   SET description = 'Деньги «мимо площадки» сданного в хабе отчёта: в итог не входят, пока владелец не подтвердит; подтверждённое приходит новой версией и входит в сумму своей страницы (counted, page_label)'
 WHERE tbl = 'hub_shift_report_adds' AND col = '';
