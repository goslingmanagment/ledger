-- Сбор Bybit (выпуск 1, PR4). Движения Bybit (депозиты, внутренние депозиты, выводы, P2P) и строки
-- журнала Funding — события в events (source 'bybit'), баланс — observations (kind 'bybit_balance'),
-- водяные знаки и статус прогона — cursors (bybit:*), разбор — proposals. Здесь: связи событий со
-- строками журнала и решения по ним, зависшие выводы и представление «несвязанные движения».
-- Время — МСК '+03:00'.

CREATE TABLE event_links (
  id           INTEGER PRIMARY KEY,
  event_id     INTEGER NOT NULL REFERENCES events(id),
  part         TEXT NOT NULL DEFAULT '',
  document_num INTEGER CHECK (document_num IS NULL OR document_num > 0),
  how          TEXT NOT NULL CHECK (how IN ('mark','note_key','manual','seen','rule')),
  decision     TEXT NOT NULL,
  brief_ref    TEXT NOT NULL DEFAULT '',
  note         TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL
);
CREATE UNIQUE INDEX event_links_once ON event_links(event_id, part, IFNULL(document_num, 0), how, brief_ref);
CREATE INDEX event_links_document ON event_links(document_num);

CREATE TABLE bybit_pending (
  key           TEXT PRIMARY KEY,
  first_seen_at TEXT NOT NULL,
  last_status   TEXT NOT NULL,
  checked_at    TEXT NOT NULL,
  movement      TEXT NOT NULL CHECK (json_valid(movement)),
  error         TEXT NOT NULL DEFAULT '',
  resolved_at   TEXT,
  resolution    TEXT
);

-- Движение закрыто, если у него есть строка журнала или явное решение «личное» / «игнор».
CREATE VIEW v_bybit_unlinked AS
  SELECT e.id AS event_id, e.source_id AS key, e.kind, e.occurred_at,
         json_extract(e.payload, '$.direction') AS direction,
         json_extract(e.payload, '$.amount') AS amount,
         json_extract(e.payload, '$.coin') AS coin,
         json_extract(e.payload, '$.counterparty_kind') AS counterparty_kind,
         (SELECT group_concat(l.how || ':' || l.decision, ', ') FROM event_links l WHERE l.event_id = e.id) AS links,
         (SELECT max(p.id) FROM proposals p WHERE p.key = e.source_id AND p.status = 'open') AS open_proposal
  FROM events e
  WHERE e.source = 'bybit' AND e.kind IN ('deposit','internal_deposit','withdrawal','p2p')
    AND NOT EXISTS (SELECT 1 FROM event_links l WHERE l.event_id = e.id AND l.part = ''
                    AND (l.document_num IS NOT NULL OR l.decision IN ('personal','ignore')));

INSERT OR IGNORE INTO schema_docs(tbl, col, description) VALUES
 ('event_links','', 'Связи событий (движения Bybit, события бота CinCin) со строками журнала и решения по ним. Одно движение может дать несколько строк: вывод и комиссию сети'),
 ('event_links','id','Номер связи'),
 ('event_links','event_id','Событие (events.id)'),
 ('event_links','part','Часть события: пусто — само движение; delta — скрытое списание в цепочке остатка карты (card:N:delta)'),
 ('event_links','document_num','Номер строки журнала (documents.num, действующая версия в v_documents); NULL — строки нет по решению'),
 ('event_links','how','Откуда связь: mark — отметка брифа ledger-sync, note_key — ключ движения в примечании строки, manual — вручную (ledger bybit link), seen — закрыто в ledger-sync без отметки, rule — правило игнора'),
 ('event_links','decision','Решение: confirm, recorded (записано владельцем), custom, personal (личное), ignore, closed (закрыто в ledger-sync, строка неизвестна)'),
 ('event_links','brief_ref','Где решено: ledger-sync:<бриф>/<пункт> или бриф ledger'),
 ('event_links','note','Текст решения или пояснение'),
 ('event_links','created_at','Когда связь записана, МСК'),
 ('bybit_pending','', 'Выводы Bybit в незавершённом статусе (SecurityCheck, Pending…). Событием вывод становится только при success; CancelByUser, Reject и Fail закрываются без движения денег'),
 ('bybit_pending','key','withdrawal:<withdrawId>'),
 ('bybit_pending','first_seen_at','Когда вывод замечен впервые, МСК'),
 ('bybit_pending','last_status','Последний статус Bybit'),
 ('bybit_pending','checked_at','Последняя проверка, МСК'),
 ('bybit_pending','movement','Движение в разобранном виде с сырой записью Bybit (JSON)'),
 ('bybit_pending','error','Ошибка последней точечной проверки по withdrawID'),
 ('bybit_pending','resolved_at','Когда вывод завершился, МСК; NULL — ещё висит'),
 ('bybit_pending','resolution','success — стал событием; CancelByUser, Reject, Fail — денег не было; closed — закрыт в ledger-sync'),
 ('v_bybit_unlinked','', 'Движения Bybit без строки журнала и без решения personal/ignore. Проверка «каждое событие с 2026-08-01 связано»: SELECT * FROM v_bybit_unlinked WHERE occurred_at >= ''2026-08-01'''),
 ('v_bybit_unlinked','event_id','Событие (events.id)'),
 ('v_bybit_unlinked','key','Ключ движения: deposit:N, internal_deposit:N, withdrawal:N, p2p:N'),
 ('v_bybit_unlinked','kind','Вид движения'),
 ('v_bybit_unlinked','occurred_at','Когда случилось, МСК'),
 ('v_bybit_unlinked','direction','income или expense'),
 ('v_bybit_unlinked','amount','Сумма в монете, как прислал Bybit'),
 ('v_bybit_unlinked','coin','Монета'),
 ('v_bybit_unlinked','counterparty_kind','Чем опознан контрагент: address, uid, p2p'),
 ('v_bybit_unlinked','links','Какие связи всё же есть (how:decision), например seen:closed'),
 ('v_bybit_unlinked','open_proposal','Открытое предложение по движению (proposals.id)');
