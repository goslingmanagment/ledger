-- Открытые вопросы «чья продажа» (план docs/plans/2026-11-10-shift-reports.md, R3): пока владелец или чаттеры не
-- ответили, вопрос агента команды — запись в ledger, а не строка в файле памяти. Ответ закрывает вопрос решением
-- (sale_decisions) или словами. Ничего не проводит и не начисляет.

CREATE TABLE sale_questions (
  id              INTEGER PRIMARY KEY,
  page_label      TEXT NOT NULL,
  fan_ref         TEXT NOT NULL DEFAULT '',
  fan_name        TEXT NOT NULL DEFAULT '',
  transaction_ref TEXT,
  net_micros      INTEGER,
  occurred_at     TEXT,
  question        TEXT NOT NULL,
  asked_of        TEXT NOT NULL CHECK (asked_of IN ('owner','chatters')),
  who             TEXT NOT NULL DEFAULT '',
  asked_at        TEXT NOT NULL,
  source_link     TEXT NOT NULL DEFAULT '',
  note            TEXT NOT NULL DEFAULT '',
  status          TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','answered','dropped')),
  decision_id     INTEGER REFERENCES sale_decisions(id),
  answer          TEXT,
  closed_at       TEXT,
  closed_note     TEXT,
  created_at      TEXT NOT NULL,
  key_name        TEXT NOT NULL
);
CREATE INDEX sale_questions_status ON sale_questions(status, id);
CREATE INDEX sale_questions_decision ON sale_questions(decision_id);

INSERT INTO schema_docs(tbl, col, description) VALUES
 ('sale_questions','', 'Открытые вопросы «чья продажа» (R3): агент команды не может решить, чья продажа, и спрашивает владельца или самих чаттеров; пока не ответили — вопрос ждёт здесь. Ответ — решение (sale_decisions, decision_id) или слова (answer). Пишет агент (право reports) или владелец (decide); ledger question list --md — открытые списком. Ничего не проводит'),
 ('sale_questions','id','Номер вопроса'),
 ('sale_questions','page_label','Страница хаба'),
 ('sale_questions','fan_ref','Фан: id хаба'),
 ('sale_questions','fan_name','Фан: ник'),
 ('sale_questions','transaction_ref','Продажа хаба (hub_sales.transaction_ref); NULL — продажи ещё нет или она неизвестна'),
 ('sale_questions','net_micros','Нетто продажи, микродоллары; NULL — неизвестно'),
 ('sale_questions','occurred_at','Когда продажа, МСК; NULL — неизвестно'),
 ('sale_questions','question','Вопрос словами (до 2000 знаков)'),
 ('sale_questions','asked_of','Кого спросили: owner — владельца; chatters — чаттеры решают между собой'),
 ('sale_questions','who','Какие чаттеры должны договориться, свободный текст; пусто — не указано'),
 ('sale_questions','asked_at','Когда спросили: ГГГГ-ММ-ДД или время МСК'),
 ('sale_questions','source_link','Ссылка на сообщение с вопросом'),
 ('sale_questions','note','Факты, как их записал агент (до 4000 знаков)'),
 ('sale_questions','status','open — ждёт ответа; answered — ответили (decision_id или answer); dropped — вопрос отпал (closed_note)'),
 ('sale_questions','decision_id','Решение, которым ответили (sale_decisions.id); NULL — ответ словами или ответа нет'),
 ('sale_questions','answer','Что ответили словами, когда записи решения не нужно'),
 ('sale_questions','closed_at','Когда ответили или вопрос отпал, МСК'),
 ('sale_questions','closed_note','Почему вопрос отпал'),
 ('sale_questions','created_at','Когда записано, МСК'),
 ('sale_questions','key_name','Ключ API, которым записано');

UPDATE schema_docs
   SET description = 'Права через запятую: read — SQL и схема, propose — предложения и споры, decide — решения владельца: бриф, правила, споры, заявки на ничьи продажи, изменения за выплаченные дни, общаки (только openclaw-main), collect — сборщики, import — разовые импорты, payouts — только GET /v1/payouts, GET /v1/payouts/prepare (выплаты чаттерам для хаба), GET /v1/reports (отчёты смен), GET /v1/report-days (база оплаты по отчётам), GET /v1/report-posts без текста постов, GET /v1/decisions, GET /v1/questions и GET /v1/pots, больше ничего, reports — POST /v1/reports (агент команды сохраняет отчёты смен), договорённости и вопросы «чья продажа» (POST /v1/decisions…, POST /v1/questions…), покупки общаков (POST /v1/pots/{slug}/sales, POST /v1/pot-sales/{id}/remove) и их чтение'
 WHERE tbl = 'keys' AND col = 'scopes';
