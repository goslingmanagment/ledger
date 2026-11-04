-- Ядро учёта: события, документы журнала, план счетов, проводки и строки проводок, предложения,
-- споры, закрытые месяцы, проверки, ключи агентов. Под выпуск 2 заведены правила дележа, общаки и
-- сделки. Время — МСК со смещением '+03:00' (ГГГГ-ММ-ДДTЧЧ:ММ:СС+03:00), даты — ГГГГ-ММ-ДД по МСК,
-- суммы — целые микродоллары (1 USD = 1 000 000), по правилам учёта 1 USDT = 1 USD.

-- Русские описания таблиц и колонок для `ledger schema` (col = '' — описание таблицы).
CREATE TABLE schema_docs (
  tbl         TEXT NOT NULL,
  col         TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL,
  PRIMARY KEY (tbl, col)
);

CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  note       TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL
);

CREATE TABLE events (
  id          INTEGER PRIMARY KEY,
  source      TEXT NOT NULL,
  source_id   TEXT NOT NULL,
  kind        TEXT NOT NULL,
  occurred_at TEXT,
  received_at TEXT NOT NULL,
  payload     TEXT NOT NULL CHECK (json_valid(payload)),
  UNIQUE (source, source_id)
);
CREATE INDEX events_kind ON events(source, kind, occurred_at);
CREATE TRIGGER events_immutable_u BEFORE UPDATE ON events BEGIN SELECT RAISE(ABORT, 'events are immutable'); END;
CREATE TRIGGER events_immutable_d BEFORE DELETE ON events BEGIN SELECT RAISE(ABORT, 'events are immutable'); END;

CREATE TABLE accounts (
  code      TEXT PRIMARY KEY,
  name      TEXT NOT NULL,
  kind      TEXT NOT NULL CHECK (kind IN ('asset','liability','equity','income','expense')),
  grp       TEXT NOT NULL,
  subject   TEXT,
  liquid    INTEGER NOT NULL DEFAULT 0 CHECK (liquid IN (0,1)),
  opened_at TEXT NOT NULL,
  closed_at TEXT,
  note      TEXT NOT NULL DEFAULT ''
);

CREATE TABLE disputes (
  id          INTEGER PRIMARY KEY,
  opened_at   TEXT NOT NULL,
  opened_by   TEXT NOT NULL,
  subject     TEXT NOT NULL,
  account     TEXT REFERENCES accounts(code),
  amount      INTEGER,
  evidence    TEXT NOT NULL DEFAULT '',
  status      TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','resolved','rejected')),
  resolution  TEXT,
  resolved_at TEXT,
  resolved_by TEXT
);

CREATE TABLE proposals (
  id            INTEGER PRIMARY KEY,
  key           TEXT NOT NULL,
  version       INTEGER NOT NULL DEFAULT 1,
  kind          TEXT NOT NULL,
  block         TEXT NOT NULL DEFAULT 'unknown' CHECK (block IN ('auto','likely','unknown','info')),
  summary       TEXT NOT NULL,
  content       TEXT NOT NULL CHECK (json_valid(content)),
  status        TEXT NOT NULL DEFAULT 'open'
                CHECK (status IN ('open','approved','rejected','superseded','auto','deferred')),
  created_at    TEXT NOT NULL,
  author        TEXT NOT NULL,
  decided_at    TEXT,
  decided_by    TEXT,
  decision      TEXT,
  decision_text TEXT,
  brief_id      INTEGER,
  brief_item    INTEGER,
  UNIQUE (key, version)
);
CREATE INDEX proposals_open ON proposals(status, key);

CREATE TABLE documents (
  id            INTEGER PRIMARY KEY,
  num           INTEGER NOT NULL CHECK (num > 0),
  version       INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
  date          TEXT NOT NULL CHECK (date GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
  type          TEXT NOT NULL CHECK (type IN ('income','expense','transfer')),
  category      TEXT NOT NULL,
  counterparty  TEXT NOT NULL,
  model         TEXT NOT NULL,
  amount        TEXT NOT NULL,
  amount_micros INTEGER NOT NULL,
  currency      TEXT NOT NULL,
  method        TEXT NOT NULL,
  status        TEXT NOT NULL CHECK (status IN ('paid','pending','cancelled')),
  note          TEXT NOT NULL,
  tags          TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(tags)),
  incomplete    INTEGER NOT NULL DEFAULT 0 CHECK (incomplete IN (0,1)),
  recorded_at   TEXT NOT NULL,
  author        TEXT NOT NULL,
  origin        TEXT NOT NULL,
  proposal_id   INTEGER REFERENCES proposals(id),
  dispute_id    INTEGER REFERENCES disputes(id),
  UNIQUE (num, version)
);
CREATE TRIGGER documents_immutable_u BEFORE UPDATE ON documents BEGIN SELECT RAISE(ABORT, 'documents are immutable: add a version'); END;
CREATE TRIGGER documents_immutable_d BEFORE DELETE ON documents BEGIN SELECT RAISE(ABORT, 'documents are immutable'); END;
CREATE VIEW v_documents AS
  SELECT d.* FROM documents d
  WHERE d.version = (SELECT max(version) FROM documents x WHERE x.num = d.num);

CREATE TABLE entries (
  id           INTEGER PRIMARY KEY,
  effective_at TEXT NOT NULL,
  recorded_at  TEXT NOT NULL,
  kind         TEXT NOT NULL,
  memo         TEXT NOT NULL DEFAULT '',
  document_id  INTEGER REFERENCES documents(id),
  proposal_id  INTEGER REFERENCES proposals(id),
  dispute_id   INTEGER REFERENCES disputes(id),
  reverses_id  INTEGER UNIQUE REFERENCES entries(id),
  author       TEXT NOT NULL,
  source_key   TEXT UNIQUE
);
CREATE INDEX entries_effective ON entries(effective_at);
CREATE INDEX entries_document ON entries(document_id);
CREATE TRIGGER entries_immutable_u BEFORE UPDATE ON entries BEGIN SELECT RAISE(ABORT, 'entries are immutable: reverse instead'); END;
CREATE TRIGGER entries_immutable_d BEFORE DELETE ON entries BEGIN SELECT RAISE(ABORT, 'entries are immutable: reverse instead'); END;

CREATE TABLE lines (
  id          INTEGER PRIMARY KEY,
  entry_id    INTEGER NOT NULL REFERENCES entries(id),
  account     TEXT NOT NULL REFERENCES accounts(code),
  amount      INTEGER NOT NULL CHECK (amount <> 0),
  currency    TEXT,
  orig_amount TEXT,
  memo        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX lines_entry ON lines(entry_id);
CREATE INDEX lines_account ON lines(account);
CREATE TRIGGER lines_immutable_u BEFORE UPDATE ON lines BEGIN SELECT RAISE(ABORT, 'lines are immutable'); END;
CREATE TRIGGER lines_immutable_d BEFORE DELETE ON lines BEGIN SELECT RAISE(ABORT, 'lines are immutable'); END;

CREATE TABLE entry_events (
  entry_id INTEGER NOT NULL REFERENCES entries(id),
  event_id INTEGER NOT NULL REFERENCES events(id),
  PRIMARY KEY (entry_id, event_id)
);

-- Правила разноски (контрагенты Bybit, игнор, мерчанты карт) и заметки атрибуции.
CREATE TABLE rules (
  id             INTEGER PRIMARY KEY,
  kind           TEXT NOT NULL CHECK (kind IN ('counterparty','ignore','merchant','merchant_bot')),
  match_uid      TEXT,
  match_address  TEXT,
  match_prefix   TEXT,
  match_suffix   TEXT,
  match_merchant TEXT,
  direction      TEXT CHECK (direction IN ('income','expense','any')),
  slug           TEXT,
  name           TEXT NOT NULL,
  category       TEXT,
  model          TEXT,
  note           TEXT NOT NULL DEFAULT '',
  params         TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(params)),
  valid_from     TEXT,
  valid_to       TEXT,
  created_at     TEXT NOT NULL,
  source         TEXT NOT NULL
);
CREATE TABLE notes (
  id         INTEGER PRIMARY KEY,
  text       TEXT NOT NULL,
  status     TEXT NOT NULL DEFAULT 'info' CHECK (status IN ('confirmed','hypothesis','refuted','info')),
  dated      TEXT,
  created_at TEXT NOT NULL,
  source     TEXT NOT NULL
);

-- Маршруты вывода страниц и коэффициенты (ставка маршрута: какая доля net доходит до перевода).
CREATE TABLE routes (
  page_label  TEXT PRIMARY KEY,
  model       TEXT NOT NULL,
  channel     TEXT NOT NULL,
  rate        TEXT NOT NULL,
  min_payout  INTEGER NOT NULL DEFAULT 0,
  payee       TEXT NOT NULL CHECK (payee IN ('model','agency')),
  lag_days    INTEGER NOT NULL DEFAULT 3,
  valid_from  TEXT,
  valid_to    TEXT,
  note        TEXT NOT NULL DEFAULT ''
);
CREATE TABLE route_rates (
  id          INTEGER PRIMARY KEY,
  page_label  TEXT NOT NULL REFERENCES routes(page_label),
  method      TEXT NOT NULL,
  rate        TEXT NOT NULL,
  valid_from  TEXT NOT NULL,
  note        TEXT NOT NULL DEFAULT '',
  UNIQUE (page_label, method, valid_from)
);
CREATE TABLE hub_exceptions (
  id              INTEGER PRIMARY KEY,
  page_label      TEXT NOT NULL,
  kind            TEXT NOT NULL CHECK (kind IN ('row','start','floor')),
  transaction_ref TEXT,
  at              TEXT,
  net             TEXT,
  decision        TEXT,
  basis           TEXT NOT NULL DEFAULT '',
  created_at      TEXT NOT NULL,
  source          TEXT NOT NULL,
  UNIQUE (page_label, kind, transaction_ref)
);

CREATE TABLE commitments (
  id         INTEGER PRIMARY KEY,
  kind       TEXT NOT NULL CHECK (kind IN ('subscription','contractor','owner_salary','reinvest','other')),
  name       TEXT NOT NULL,
  category   TEXT,
  amount     INTEGER NOT NULL,
  period     TEXT NOT NULL,
  next_due   TEXT,
  until      TEXT,
  account    TEXT,
  note       TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  source     TEXT NOT NULL
);
CREATE TABLE rates (
  id         INTEGER PRIMARY KEY,
  person     TEXT NOT NULL,
  kind       TEXT NOT NULL,
  value      TEXT NOT NULL,
  valid_from TEXT NOT NULL,
  valid_to   TEXT,
  source     TEXT NOT NULL,
  checked_at TEXT
);

-- Выпуск 2: правила дележа, общаки и сделки. Таблицы заведены заранее, наполняются позже.
CREATE TABLE split_rules (
  id           INTEGER PRIMARY KEY,
  kind         TEXT NOT NULL,
  participants TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(participants)),
  params       TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(params)),
  valid_from   TEXT NOT NULL,
  valid_to     TEXT,
  decision_ref TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL
);
CREATE TABLE pots (
  id           INTEGER PRIMARY KEY,
  name         TEXT NOT NULL,
  scope        TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(scope)),
  participants TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(participants)),
  method       TEXT NOT NULL DEFAULT 'equal',
  status       TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','closed')),
  opened_at    TEXT NOT NULL,
  closed_at    TEXT,
  settlement   TEXT CHECK (settlement IS NULL OR json_valid(settlement)),
  decision_ref TEXT NOT NULL DEFAULT ''
);
CREATE TABLE deals (
  id           INTEGER PRIMARY KEY,
  ref          TEXT NOT NULL UNIQUE,
  model        TEXT NOT NULL,
  price        INTEGER NOT NULL,
  model_share  INTEGER NOT NULL,
  status       TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','paid','closed')),
  opened_at    TEXT NOT NULL,
  closed_at    TEXT,
  note         TEXT NOT NULL DEFAULT ''
);

-- Наблюдённые остатки: Bybit API, анкоры карт, скрины, предоплаты, балансы страниц из хаба.
CREATE TABLE observations (
  id          INTEGER PRIMARY KEY,
  kind        TEXT NOT NULL,
  subject     TEXT NOT NULL,
  value       INTEGER NOT NULL,
  observed_at TEXT NOT NULL,
  source      TEXT NOT NULL,
  event_id    INTEGER REFERENCES events(id),
  payload     TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(payload)),
  UNIQUE (kind, subject, observed_at, source)
);

-- Закрытые месяцы: снимок «на закрытии» неизменяем; «сообщено владельцу» — отдельной историей.
CREATE TABLE periods (
  month     TEXT PRIMARY KEY CHECK (month GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]'),
  locked_at TEXT NOT NULL,
  as_of     TEXT NOT NULL,
  snapshot  TEXT NOT NULL CHECK (json_valid(snapshot)),
  decision  INTEGER,
  verdict   TEXT,
  source    TEXT NOT NULL
);
CREATE TRIGGER periods_immutable_u BEFORE UPDATE ON periods BEGIN SELECT RAISE(ABORT, 'closed months are immutable'); END;
CREATE TRIGGER periods_immutable_d BEFORE DELETE ON periods BEGIN SELECT RAISE(ABORT, 'closed months are immutable'); END;
CREATE TABLE period_reports (
  id          INTEGER PRIMARY KEY,
  month       TEXT NOT NULL REFERENCES periods(month),
  reported_at TEXT NOT NULL,
  decision    INTEGER NOT NULL,
  verdict     TEXT NOT NULL,
  note        TEXT NOT NULL DEFAULT ''
);

CREATE TABLE checks (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL,
  started_at  TEXT NOT NULL,
  finished_at TEXT NOT NULL,
  ok          INTEGER NOT NULL CHECK (ok IN (0,1)),
  severity    TEXT NOT NULL DEFAULT 'info' CHECK (severity IN ('info','warn','urgent')),
  summary     TEXT NOT NULL,
  details     TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(details))
);
CREATE INDEX checks_name ON checks(name, started_at);

CREATE TABLE keys (
  id           INTEGER PRIMARY KEY,
  name         TEXT NOT NULL UNIQUE,
  prefix       TEXT NOT NULL,
  hash         TEXT NOT NULL,
  scopes       TEXT NOT NULL,
  created_at   TEXT NOT NULL,
  revoked_at   TEXT,
  last_used_at TEXT
);
CREATE TABLE query_log (
  id          INTEGER PRIMARY KEY,
  key_name    TEXT NOT NULL,
  at          TEXT NOT NULL,
  sql         TEXT NOT NULL,
  duration_ms INTEGER NOT NULL,
  rows        INTEGER NOT NULL DEFAULT 0,
  error       TEXT
);

-- Курсоры сборщиков (водяные знаки, последние прогоны).
CREATE TABLE cursors (
  name       TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE VIEW v_balances AS
  SELECT a.code AS account, a.name, a.kind, a.grp, a.subject, a.liquid,
         COALESCE(SUM(l.amount), 0) AS balance_micros,
         ROUND(CASE WHEN a.kind IN ('asset','expense') THEN 1 ELSE -1 END
               * COALESCE(SUM(l.amount), 0) / 1e6, 2) AS balance,
         MAX(e.effective_at) AS last_effective_at
  FROM accounts a
  LEFT JOIN lines l ON l.account = a.code
  LEFT JOIN entries e ON e.id = l.entry_id
  GROUP BY a.code;

CREATE VIEW v_unbalanced AS
  SELECT entry_id, SUM(amount) AS sum_micros, COUNT(*) AS lines FROM lines GROUP BY entry_id HAVING SUM(amount) <> 0;

-- Базовый план счетов. Счета с именем страницы, человека или карты заводятся при первой проводке.
INSERT INTO accounts(code, name, kind, grp, liquid, opened_at) VALUES
 ('bybit',                 'Bybit, основной счёт агентства',              'asset',     'money',    1, '2026-01-01'),
 ('cincin_wallet',         'Кошелёк CinCin',                              'asset',     'money',    1, '2026-01-01'),
 ('cash:unknown',          'Деньги без указанного метода (до 2026-08)',    'asset',     'money',    0, '2026-01-01'),
 ('clearing:offset',       'Зачёты: деньги фана в счёт выплаты человеку',  'asset',     'clearing', 0, '2026-01-01'),
 ('suspense:accrual',      'Ждёт начисления (переводы и выплаты с 2026-09)','liability', 'suspense', 0, '2026-01-01'),
 ('opening',               'Стартовые остатки и выравнивания',            'equity',    'owner',    0, '2026-01-01'),
 ('owner:draw',            'Изъятия владельца',                            'equity',    'owner',    0, '2026-01-01'),
 ('owner:injection',       'Вливания владельца',                           'equity',    'owner',    0, '2026-01-01'),
 ('income:custom',         'Доход мимо платформ: кастомы и крипта фанов',  'income',    'income',   0, '2026-01-01'),
 ('income:referrals',      'Реферальные выплаты платформ',                 'income',    'income',   0, '2026-01-01'),
 ('income:other',          'Прочий доход',                                 'income',    'income',   0, '2026-01-01'),
 ('expense:chatter_salary','Зарплата чаттеров',                            'expense',   'expense',  0, '2026-01-01'),
 ('expense:km_salary',     'Зарплата контент-менеджеров',                  'expense',   'expense',  0, '2026-01-01'),
 ('expense:owner_salary',  'Зарплата владельца (CEO)',                     'expense',   'owner',    0, '2026-01-01'),
 ('expense:model_share',   'Доля модели сверх её доли',                    'expense',   'expense',  0, '2026-01-01'),
 ('expense:contractor',    'Подрядчики',                                   'expense',   'expense',  0, '2026-01-01'),
 ('expense:traffic',       'Трафик',                                       'expense',   'expense',  0, '2026-01-01'),
 ('expense:sfs',           'SFS-рассылки',                                 'expense',   'expense',  0, '2026-01-01'),
 ('expense:youtube',       'YouTube-продвижение',                          'expense',   'expense',  0, '2026-01-01'),
 ('expense:tools',         'Инструменты',                                  'expense',   'expense',  0, '2026-01-01'),
 ('expense:ai_dev',        'AI и разработка',                              'expense',   'expense',  0, '2026-01-01'),
 ('expense:fees',          'Комиссии платёжных маршрутов',                 'expense',   'expense',  0, '2026-01-01'),
 ('expense:other',         'Прочие расходы',                               'expense',   'expense',  0, '2026-01-01'),
 ('expense_refund',        'Возвраты расходов (уменьшают расходы месяца)', 'expense',   'expense',  0, '2026-01-01');
