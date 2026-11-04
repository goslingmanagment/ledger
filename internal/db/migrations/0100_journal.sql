-- Журнал ledger.md: шаблон файла (текстовые блоки и места строк) для точного зеркала, разбивки денег
-- строк, которые общее сопоставление выразить не может, и счета, нужные сопоставлению строк.
CREATE TABLE journal_templates (
  id         INTEGER PRIMARY KEY,
  created_at TEXT NOT NULL,
  origin     TEXT NOT NULL,
  body       TEXT NOT NULL
);
CREATE TABLE journal_overrides (
  num        INTEGER PRIMARY KEY,
  data       TEXT NOT NULL CHECK (json_valid(data)),
  source     TEXT NOT NULL,
  created_at TEXT NOT NULL
);
INSERT INTO accounts(code, name, kind, grp, liquid, opened_at) VALUES
 ('bybit:btc',       'Bybit, BTC (в долларах на дату движения)',      'asset', 'money',    0, '2026-01-01'),
 ('clearing:period', 'Переходящие суммы: часть платежа другого месяца', 'asset', 'clearing', 0, '2026-01-01');
INSERT INTO schema_docs(tbl, col, description) VALUES
 ('journal_templates','', 'Шаблон ledger.md: текст между таблицами и места строк журнала; из него и документов печатается зеркало'),
 ('journal_overrides','', 'Разбивка денежной стороны строки журнала по нескольким счетам (одна комиссия на две карты и т. п.)');
CREATE TABLE journal_imports (
  ref         TEXT PRIMARY KEY,
  at          TEXT NOT NULL,
  imported_at TEXT NOT NULL
);
INSERT INTO schema_docs(tbl, col, description) VALUES
 ('journal_imports','', 'Какие версии ledger.md (коммиты /OS) уже применены импортом');
