-- Бриф, решения владельца, улики и сборщик карт (выпуск 1, PR8). Нумерация брифа заморожена: пункт
-- хранит предложение и его версию, решение действует только на показанную версию. Время — МСК '+03:00'.

CREATE TABLE briefs (
  id         INTEGER PRIMARY KEY,
  created_at TEXT NOT NULL,
  created_by TEXT NOT NULL,
  mode       TEXT NOT NULL,
  status     TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','closed')),
  sealed     INTEGER NOT NULL DEFAULT 0 CHECK (sealed IN (0,1)),
  sent_at    TEXT,
  closed_at  TEXT
);

CREATE TABLE brief_items (
  brief_id         INTEGER NOT NULL REFERENCES briefs(id),
  num              INTEGER NOT NULL CHECK (num > 0),
  proposal_id      INTEGER NOT NULL REFERENCES proposals(id),
  proposal_version INTEGER NOT NULL,
  key              TEXT NOT NULL,
  block            TEXT NOT NULL,
  summary          TEXT NOT NULL,
  asked_count      INTEGER NOT NULL,
  first_asked_at   TEXT NOT NULL,
  ledger_match     TEXT NOT NULL DEFAULT '',
  decision         TEXT,
  decision_text    TEXT,
  decided_at       TEXT,
  decided_by       TEXT,
  document_nums    TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (brief_id, num)
);
CREATE INDEX brief_items_key ON brief_items(key);
CREATE INDEX brief_items_proposal ON brief_items(proposal_id);
CREATE TRIGGER brief_items_sealed_i BEFORE INSERT ON brief_items
  WHEN (SELECT sealed FROM briefs WHERE id = NEW.brief_id) = 1
  BEGIN SELECT RAISE(ABORT, 'brief numbering is frozen'); END;
CREATE TRIGGER brief_items_frozen_u BEFORE UPDATE OF brief_id, num, proposal_id, proposal_version, key, block, summary,
  asked_count, first_asked_at, ledger_match ON brief_items
  BEGIN SELECT RAISE(ABORT, 'brief numbering is frozen'); END;
CREATE TRIGGER brief_items_frozen_d BEFORE DELETE ON brief_items
  BEGIN SELECT RAISE(ABORT, 'brief numbering is frozen'); END;

CREATE TABLE evidence (
  id         INTEGER PRIMARY KEY,
  key        TEXT NOT NULL,
  source     TEXT NOT NULL,
  text       TEXT NOT NULL,
  created_at TEXT NOT NULL,
  author     TEXT NOT NULL,
  UNIQUE (key, source, text)
);
CREATE INDEX evidence_key ON evidence(key);

CREATE TABLE deposit_links (
  deposit_event_id    INTEGER PRIMARY KEY REFERENCES events(id),
  withdrawal_event_id INTEGER NOT NULL UNIQUE REFERENCES events(id),
  how                 TEXT NOT NULL CHECK (how IN ('auto','import','manual')),
  created_at          TEXT NOT NULL
);

CREATE VIEW v_open AS
  SELECT p.id, p.key, p.version, p.kind, p.block, p.summary, p.created_at, p.decision, p.decision_text,
         (SELECT max(i.brief_id) FROM brief_items i WHERE i.proposal_id = p.id) AS last_brief,
         (SELECT count(DISTINCT substr(b.sent_at, 1, 10)) FROM brief_items i JOIN briefs b ON b.id = i.brief_id
           WHERE i.key = p.key AND b.sent_at IS NOT NULL) AS asked_days
  FROM proposals p
  WHERE p.status = 'open';

UPDATE schema_docs SET description = 'Откуда связь: mark — решение брифа (ledger-sync или ledger, см. brief_ref), note_key — ключ движения в примечании строки, manual — вручную (ledger bybit link), seen — закрыто в ledger-sync без отметки, rule — правило (игнор, автозапись, служебное событие бота)'
  WHERE tbl = 'event_links' AND col = 'how';
UPDATE schema_docs SET description = 'Решение: confirm, recorded (записано владельцем), custom, personal (личное), ignore, closed (закрыто в ledger-sync, строка неизвестна), auto (записано по точному правилу)'
  WHERE tbl = 'event_links' AND col = 'decision';
UPDATE schema_docs SET description = 'Где решено: ledger-sync:<бриф>/<пункт> или ledger:<бриф>/<пункт>'
  WHERE tbl = 'event_links' AND col = 'brief_ref';

INSERT OR IGNORE INTO schema_docs(tbl, col, description) VALUES
 ('briefs','', 'Брифы владельцу: состав и нумерация заморожены при создании'),
 ('briefs','id','Номер брифа'),
 ('briefs','created_at','Когда создан, МСК'),
 ('briefs','created_by','Кто создал: имя ключа или local'),
 ('briefs','mode','Режим учёта при создании: shadow — журнал ведётся в /OS, live — ledger пишет строки сам'),
 ('briefs','status','open — есть что решать; closed — живых пунктов не осталось'),
 ('briefs','sealed','1 — нумерация заморожена, новых пунктов не будет'),
 ('briefs','sent_at','Когда отправлен владельцу, МСК; NULL — ещё не отправлен, такой бриф не считается вопросом'),
 ('briefs','closed_at','Когда закрыт, МСК'),
 ('brief_items','', 'Пункты брифа с замороженными номерами. Номер, предложение, версия и текст не меняются; меняется только решение'),
 ('brief_items','brief_id','Бриф (briefs.id)'),
 ('brief_items','num','Номер пункта в брифе: единственный допустимый в сообщении владельцу и в ledger decide'),
 ('brief_items','proposal_id','Предложение (proposals.id)'),
 ('brief_items','proposal_version','Версия предложения, показанная в брифе; если вышла новая, решение отклоняется'),
 ('brief_items','key','Ключ движения или события: withdrawal:N, card:N, card:N:delta, agent:…'),
 ('brief_items','block','Блок брифа: auto, likely, unknown, p2p, non_usdt, cards'),
 ('brief_items','summary','Строка пункта'),
 ('brief_items','asked_count','На сколько разных дней МСК пункт спрашивался, считая день создания брифа; от 3 — висит'),
 ('brief_items','first_asked_at','Первая отправка владельцу, МСК'),
 ('brief_items','ledger_match','Строки журнала с той же суммой и датой ±1 день: вопрос владельцу, не автозакрытие'),
 ('brief_items','decision','Решение: confirm, personal, custom, defer, recorded'),
 ('brief_items','decision_text','Текст решения: JSON-правка custom или номера строк recorded'),
 ('brief_items','decided_at','Когда решено, МСК'),
 ('brief_items','decided_by','Кем решено: имя ключа'),
 ('brief_items','document_nums','Какие строки журнала дал пункт, через запятую'),
 ('evidence','', 'Улики по движению: уже найденное расследованием, показывается в брифе как prior_evidence'),
 ('evidence','key','Ключ движения или события, как в brief_items.key'),
 ('evidence','source','Откуда улика: tg, gmail, ledger, agent…'),
 ('evidence','text','Факт без цитат'),
 ('evidence','created_at','Когда записана, МСК'),
 ('evidence','author','Кто записал: имя ключа или import:ledger-sync'),
 ('deposit_links','', 'Депозит в кошелёк CinCin ↔ вывод Bybit: один депозит на один вывод'),
 ('deposit_links','deposit_event_id','Событие бота (events.id, source cincin_bot, kind deposit)'),
 ('deposit_links','withdrawal_event_id','Вывод Bybit (events.id, source bybit, kind withdrawal)'),
 ('deposit_links','how','auto — ровно один кандидат по сумме, адресу CinCin и ±3 ч; import — из ledger-sync; manual — ledger cards link'),
 ('deposit_links','created_at','Когда связано, МСК'),
 ('v_open','', 'Что ждёт решения владельца: открытые предложения, последний бриф и число разных дней, когда пункт уже спрашивался'),
 ('v_open','id','Предложение (proposals.id)'),
 ('v_open','key','Ключ движения или события'),
 ('v_open','version','Действующая версия предложения'),
 ('v_open','kind','Вид: bybit, bybit_p2p, bybit_non_usdt, card_purchase, card_load, card_delta, card_deposit, agent'),
 ('v_open','block','auto, likely, unknown'),
 ('v_open','summary','Строка для брифа'),
 ('v_open','created_at','Когда предложено, МСК'),
 ('v_open','decision','Записанное решение, если журнал ещё не подтвердил его (режим shadow) или пункт отложен'),
 ('v_open','decision_text','Текст решения'),
 ('v_open','last_brief','Последний бриф с этой версией'),
 ('v_open','asked_days','Сколько разных дней МСК пункт уходил владельцу'),
 ('proposals','id','Номер предложения'),
 ('proposals','key','Ключ движения или события; версии одного ключа — строки с разными version'),
 ('proposals','version','Версия: новая строка, если предложение изменилось; старая становится superseded'),
 ('proposals','kind','Вид: bybit, bybit_p2p, bybit_non_usdt, card_purchase, card_load, card_delta, card_deposit, agent'),
 ('proposals','summary','Строка для брифа'),
 ('proposals','content','JSON: движение, предложенные строки журнала (documents), подсказки'),
 ('proposals','status','open — ждёт решения; approved — решено владельцем; auto — записано по правилу; superseded — закрыто журналом или новой версией; rejected — отклонено'),
 ('proposals','created_at','Когда предложено, МСК'),
 ('proposals','author','Кто предложил: сборщик или имя ключа агента'),
 ('proposals','decided_at','Когда решено, МСК'),
 ('proposals','decided_by','Кем решено'),
 ('proposals','decision','Решение: confirm, personal, custom, defer, recorded, auto, new_version; у закрытых журналом — recorded или решение связи'),
 ('proposals','decision_text','Текст решения или связи, которые закрыли предложение'),
 ('proposals','brief_id','Бриф, по которому решено'),
 ('proposals','brief_item','Пункт брифа, по которому решено'),
 ('disputes','id','Номер спора'),
 ('disputes','opened_at','Когда открыт, МСК'),
 ('disputes','opened_by','Кто открыл: имя ключа'),
 ('disputes','subject','Что оспаривается'),
 ('disputes','account','Счёт, если оспаривается остаток'),
 ('disputes','amount','Сумма в микродолларах'),
 ('disputes','evidence','Улики'),
 ('disputes','status','open, resolved (исправление проведено), rejected'),
 ('disputes','resolution','Решение: JSON со строками и проводками исправления или текст отказа'),
 ('disputes','resolved_at','Когда решён, МСК'),
 ('disputes','resolved_by','Кем решён: имя ключа');
