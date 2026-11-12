-- Одна продажа — две ссылки (ревью экосистемы, круг 3).
--
-- Хаб называет продажу своим id транзакции (hub_transactions.key): его несут факты продаж (hub_sales.transaction_ref),
-- решения «чья продажа» (hub_sale_decisions.transaction_ref) и строки отчётов, сданных в хабе. Площадка называет ту же
-- продажу своей ссылкой на транзакцию (hub_transactions.ref): её несут строки отчётов, которые агент сохраняет по постам
-- (shift_report_lines.transaction_ref). Покупка общака, решение и вопрос ledger несут ту ссылку, с которой записаны.
-- Защиты общака, договорённость строки отчёта и стирание фана сравнивали одну ссылку с другой и продажу не узнавали.
-- Обе ссылки одной продажи держит только зеркало транзакций хаба, и теперь всё сравнивается через него
-- (internal/hub/salerefs.go). Сохранённые ссылки не переписываются.
--
-- Миграция только добавляет: два индекса для поиска продажи по любой из ссылок и уточнения описаний. Данных не меняет.

CREATE INDEX hub_transactions_key ON hub_transactions(key);
CREATE INDEX hub_transactions_ref ON hub_transactions(ref) WHERE ref IS NOT NULL;

UPDATE schema_docs
   SET description = 'Ключ строки в хабе (стабильный внутри датасета) — он же id продажи в хабе: его несут hub_sales.transaction_ref, hub_sale_decisions.transaction_ref, hub_shift_report_lines.transaction_ref. Вторая ссылка той же продажи — ref'
 WHERE tbl = 'hub_transactions' AND col = 'key';
UPDATE schema_docs
   SET description = 'transactionRef хаба — ссылка площадки на транзакцию: её несут строки отчётов агента (shift_report_lines.transaction_ref); по ней строка сверяется с hub_exceptions. id той же продажи в хабе — key'
 WHERE tbl = 'hub_transactions' AND col = 'ref';
UPDATE schema_docs
   SET description = 'id продажи в хабе (transactions.id) — hub_transactions.key. Ссылка площадки на ту же продажу, которую несут строки отчётов агента, — hub_transactions.ref'
 WHERE tbl = 'hub_sales' AND col = 'transaction_ref';
UPDATE schema_docs
   SET description = 'Продажа, как её прислал агент: ссылка площадки на транзакцию (hub_transactions.ref), а не id хаба (hub_sales.transaction_ref). С hub_sales, hub_sale_decisions и покупками общака сводить через hub_transactions (key — id хаба, ref — ссылка площадки). NULL у добавленной суммы'
 WHERE tbl = 'shift_report_lines' AND col = 'transaction_ref';
UPDATE schema_docs
   SET description = 'Продажа: у строк agent — ссылка площадки (hub_transactions.ref), у строк hub — id хаба (hub_transactions.key); одну продажу в двух источниках сводит hub_transactions'
 WHERE tbl = 'v_report_lines' AND col = 'transaction_ref';
UPDATE schema_docs
   SET description = 'Продажа — той ссылкой, с которой покупка записана: id хаба (джоб hub-decisions, --tx <id хаба>) или ссылка площадки (--from-line, --tx <ссылка площадки>); обе ссылки одной продажи — в hub_transactions (key, ref). Уникальна среди не снятых; NULL — покупки нет в хабе'
 WHERE tbl = 'pot_sales' AND col = 'transaction_ref';
UPDATE schema_docs
   SET description = 'Продажа покупки, как в pot_sales: id хаба или ссылка площадки; NULL — покупки нет в хабе'
 WHERE tbl = 'v_pot_shares' AND col = 'transaction_ref';
UPDATE schema_docs
   SET description = 'Продажа — той ссылкой, с которой решение записано: id хаба или ссылка площадки (обе — в hub_transactions: key, ref); NULL — продажи ещё нет («обещал купить в пятницу») или она мимо хаба'
 WHERE tbl = 'sale_decisions' AND col = 'transaction_ref';
UPDATE schema_docs
   SET description = 'Продажа — той ссылкой, с которой вопрос записан: id хаба или ссылка площадки (обе — в hub_transactions: key, ref); NULL — продажи ещё нет или она неизвестна'
 WHERE tbl = 'sale_questions' AND col = 'transaction_ref';
UPDATE schema_docs
   SET description = 'Сколько строк изменено по таблицам при первом применении (JSON); ключ ambiguous_names — сколько имён фана не взято для поиска его строк, потому что строки ledger связывают их и с другим фаном'
 WHERE tbl = 'hub_erasures' AND col = 'rows';
