-- Выпуск 2, L2: факты смен и продаж хаба (операция агентов agentTeamSales, право read:team + read:money) для правил
-- дележа. Читаются окнами по неделе МСК (понедельник 00:00 — следующий понедельник, текущая — до момента чтения):
-- каждый час недели, задевающие последние 14 дней, раз в неделю все с начала правила (split.Default.Since).
-- Окно — согласованный снимок хаба: работы окна (от суток до начала до 7 дней после конца) и все его продажи.
-- Ответ не полностью (отказ, 403 без права, не тот срез, держатель не из работ, нет страницы гранта) — окно unknown,
-- его данные остаются, но начисления по ним не считаются. Время — МСК '+03:00' с миллисекундами, деньги — mills.

CREATE TABLE hub_team_sync (
  window_from TEXT PRIMARY KEY,
  window_to   TEXT NOT NULL,
  status      TEXT NOT NULL CHECK (status IN ('ok','unknown')),
  reasons     TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(reasons)),
  works       INTEGER,
  sales       INTEGER,
  net_mills   INTEGER,
  work_from   TEXT,
  work_to     TEXT,
  server_now  TEXT,
  requests    INTEGER NOT NULL DEFAULT 0,
  read_at     TEXT NOT NULL,
  ok_at       TEXT
);

CREATE TABLE hub_works (
  window_from    TEXT NOT NULL,
  ref            TEXT NOT NULL,
  user_id        INTEGER NOT NULL,
  username       TEXT NOT NULL,
  role           TEXT NOT NULL,
  kind           TEXT NOT NULL CHECK (kind IN ('shift','visit')),
  started_at     TEXT NOT NULL,
  ended_at       TEXT NOT NULL,
  ongoing        INTEGER NOT NULL CHECK (ongoing IN (0,1)),
  worked_minutes INTEGER NOT NULL,
  day            TEXT NOT NULL,
  payload        TEXT NOT NULL CHECK (json_valid(payload)),
  read_at        TEXT NOT NULL,
  PRIMARY KEY (window_from, ref)
);
CREATE INDEX hub_works_user ON hub_works(user_id, started_at);

CREATE TABLE hub_sales (
  transaction_ref     TEXT PRIMARY KEY,
  window_from         TEXT NOT NULL,
  page_label          TEXT NOT NULL,
  platform            TEXT NOT NULL,
  kind                TEXT NOT NULL,
  net_mills           INTEGER NOT NULL,
  occurred_at         TEXT NOT NULL,
  ppv_sender_user_id  INTEGER,
  last_writer_user_id INTEGER,
  post_tip            INTEGER NOT NULL CHECK (post_tip IN (0,1)),
  campaign            INTEGER NOT NULL CHECK (campaign IN (0,1)),
  payload             TEXT NOT NULL CHECK (json_valid(payload)),
  first_seen_at       TEXT NOT NULL,
  read_at             TEXT NOT NULL
);
CREATE INDEX hub_sales_window ON hub_sales(window_from, occurred_at);
CREATE INDEX hub_sales_time ON hub_sales(occurred_at);

INSERT INTO schema_docs(tbl, col, description) VALUES
 ('hub_team_sync','', 'Что хаб доказал по каждому недельному окну фактов команды (agentTeamSales). status unknown — ответ не принят (нет права read:team, отказ, срез не тот, держатель продажи не из работ, нет страницы гранта): начисления из фактов этой недели «неизвестно», не ноль'),
 ('hub_team_sync','window_from','Начало окна: понедельник 00:00 МСК'),
 ('hub_team_sync','window_to','Конец окна последней попытки, МСК: следующий понедельник или момент чтения у текущей недели'),
 ('hub_team_sync','status','ok — последняя попытка доказала окно; unknown — не доказала'),
 ('hub_team_sync','reasons','Почему ответ не принят (JSON-массив строк)'),
 ('hub_team_sync','works','Работ (смен и заходов) в последнем принятом ответе'),
 ('hub_team_sync','sales','Продаж окна в последнем принятом ответе'),
 ('hub_team_sync','net_mills','Сумма netMills продаж окна в последнем принятом ответе, тысячные доллара'),
 ('hub_team_sync','work_from','С какого момента хаб прочитал работы (workFrom), МСК'),
 ('hub_team_sync','work_to','До какого момента хаб прочитал работы (workTo: 7 дней после окна или его «сейчас»), МСК; PPV, ждущий смены отправителя позже, ещё ждёт'),
 ('hub_team_sync','server_now','«Сейчас» хаба в принятом ответе, МСК'),
 ('hub_team_sync','requests','Запросов к хабу в последней попытке'),
 ('hub_team_sync','read_at','Когда окно читалось последний раз, МСК'),
 ('hub_team_sync','ok_at','Когда окно последний раз доказано, МСК'),
 ('hub_works','', 'Работы команды по снимку каждого недельного окна (смены и заходы хаба): одна и та же работа может быть в двух окнах; у края окна хаб видит её обрезанной, полная версия — в окне, где она началась'),
 ('hub_works','window_from','Окно (hub_team_sync), из ответа которого строка'),
 ('hub_works','ref','Ссылка хаба на работу «<userId>:<startedAt>»: по ней продажа называет держателей'),
 ('hub_works','user_id','Пользователь хаба'),
 ('hub_works','username','Имя пользователя хаба; человек ledger — по people.aliases (имя или hub:<id>)'),
 ('hub_works','role','Роль в хабе сейчас: chatter, team_lead, owner'),
 ('hub_works','kind','shift — смена; visit — заход короче смены'),
 ('hub_works','started_at','Начало работы, МСК с миллисекундами'),
 ('hub_works','ended_at','Конец работы (у идущей — «сейчас» хаба), МСК с миллисекундами'),
 ('hub_works','ongoing','1 — работа идёт'),
 ('hub_works','worked_minutes','Отработано минут'),
 ('hub_works','day','День работы МСК: день, на который приходится середина работы (dayOfWork хаба), — как его даёт таблица выплат'),
 ('hub_works','payload','Работа как в ответе хаба (JSON): страницы с моментом, с которого она держит их продажи'),
 ('hub_works','read_at','Когда прочитана, МСК'),
 ('hub_sales','', 'Продажи хаба с фактами для правил дележа (internal/split): кто держал страницу, отправитель PPV, чаевые под постом, Campaign, последний писавший, метки чаттеров и решение владельца. Окно — hub_team_sync'),
 ('hub_sales','transaction_ref','Ссылка хаба на транзакцию'),
 ('hub_sales','window_from','Недельное окно продажи'),
 ('hub_sales','page_label','Страница хаба'),
 ('hub_sales','platform','fansly или onlyfans'),
 ('hub_sales','kind','ppv, tip, subscription, post'),
 ('hub_sales','net_mills','Net модели, тысячные доллара'),
 ('hub_sales','occurred_at','Когда куплено, МСК с миллисекундами'),
 ('hub_sales','ppv_sender_user_id','Кто отправил купленный PPV; NULL — не PPV или без автора'),
 ('hub_sales','last_writer_user_id','Кто последним писал фану на странице за 48 часов до продажи; NULL — никто с автором'),
 ('hub_sales','post_tip','1 — чаевые под постом на стене (общак)'),
 ('hub_sales','campaign','1 — взнос в Campaign OnlyFans (общак)'),
 ('hub_sales','payload','Продажа как в ответе хаба (JSON): держатели, писавшие, метки, решение владельца'),
 ('hub_sales','first_seen_at','Когда продажа появилась в ledger, МСК'),
 ('hub_sales','read_at','Когда получена текущая версия, МСК');
