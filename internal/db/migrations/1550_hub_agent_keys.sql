-- Один ключ агента (план экосистемы, M2): агент приходит в ledger с ключом хаба вместо своего ключа ledger. ledger
-- спрашивает хаб, чей ключ (только sha256 ключа, своим ключом хаба с правом introspect:agent_keys), и даёт коду агента
-- права из своей таблицы hub_agents, которую правит владелец (`ledger keys hub-agent`). Кода нет в таблице — ничего.
-- Ключу хаба открыты только права чтения: read и payouts; decide и всё, что пишет или меняет деньги, — никогда
-- (CHECK ниже и проверка в коде). Каждый вызов ключом хаба пишется в hub_key_calls под кодом агента.

CREATE TABLE hub_agents (
  code        TEXT PRIMARY KEY CHECK (code <> '' AND length(code) <= 63 AND code NOT GLOB '*[^a-z0-9-]*' AND code NOT GLOB '-*'),
  scopes      TEXT NOT NULL CHECK (scopes IN ('read', 'payouts', 'read,payouts')),
  note        TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL,
  disabled_at TEXT
);

CREATE TABLE hub_key_calls (
  id         INTEGER PRIMARY KEY,
  at         TEXT NOT NULL,
  agent_code TEXT NOT NULL,
  method     TEXT NOT NULL,
  path       TEXT NOT NULL,
  status     INTEGER NOT NULL,
  held       INTEGER NOT NULL DEFAULT 0 CHECK (held IN (0, 1))
);
CREATE INDEX hub_key_calls_at ON hub_key_calls(at);
CREATE INDEX hub_key_calls_agent ON hub_key_calls(agent_code, at);

INSERT INTO schema_docs(tbl, col, description) VALUES
 ('hub_agents','', 'Агенты хаба, которым ledger открыт по ключу хаба (M2): код агента хаба → права ledger. Правит владелец на сервере: ledger keys hub-agent <код> --scope read[,payouts] | --off. Кода нет или строка выключена — ключ хаба получает отказ'),
 ('hub_agents','code', 'Код агента в хабе (люди хаба вида agent: gengar, os-machine)'),
 ('hub_agents','scopes', 'Права через запятую, только чтение: read — SQL и схема, payouts — маршруты выплат для хаба. decide, propose, reports, collect, import ключу хаба не даются никогда'),
 ('hub_agents','note', 'Заметка владельца: зачем агенту доступ'),
 ('hub_agents','created_at', 'Когда строка заведена, МСК'),
 ('hub_agents','updated_at', 'Когда права менялись последний раз, МСК'),
 ('hub_agents','disabled_at', 'Когда владелец выключил доступ (NULL — включён); строка остаётся для истории'),
 ('hub_key_calls','', 'Журнал вызовов ledger ключами хаба: каждый вызов с ответом хаба «ключ действует» — под кодом агента, с итогом. Ни ключа, ни его отпечатка здесь нет'),
 ('hub_key_calls','at', 'Когда пришёл вызов, МСК'),
 ('hub_key_calls','agent_code', 'Код агента в хабе'),
 ('hub_key_calls','method', 'HTTP-метод'),
 ('hub_key_calls','path', 'Путь запроса без параметров'),
 ('hub_key_calls','status', 'HTTP-статус ответа ledger (403 — у кода нет такого права или строки)'),
 ('hub_key_calls','held', '1 — хаб не отвечал, ledger держал последний ответ «ключ действует» (не дольше часа с его получения)');
