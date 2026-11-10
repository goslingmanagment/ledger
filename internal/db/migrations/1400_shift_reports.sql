-- Отчёты смен, этап R1 (план docs/plans/2026-11-10-shift-reports.md): агент команды после каждого прогона своего
-- скрипта сохраняет расклад отчёта (POST /v1/reports, право reports): страницы и суммы, каждая продажа — в итоге
-- или нет и почему, добавленные руками суммы. ledger хранит версии и отдаёт их хабу (GET /v1/reports). Ничего здесь
-- не проводится и не начисляется: долги, payable_days и v_payables не меняются. Деньги — целые микродоллары
-- (mills агента × 1000), времена — МСК.

CREATE TABLE shift_reports (
  id           INTEGER PRIMARY KEY,
  person       TEXT,
  hub_username TEXT NOT NULL CHECK (trim(hub_username) <> ''),
  hub_user_id  INTEGER,
  chatter      TEXT NOT NULL DEFAULT '',
  report_date  TEXT NOT NULL,
  window_from  TEXT NOT NULL,
  window_to    TEXT NOT NULL,
  cut_at       TEXT NOT NULL,
  version      INTEGER NOT NULL CHECK (version >= 1),
  total_micros INTEGER NOT NULL,
  source       TEXT NOT NULL CHECK (source IN ('agent')),
  status       TEXT NOT NULL CHECK (status IN ('draft','posted','superseded')),
  content_hash TEXT NOT NULL,
  payload      TEXT NOT NULL CHECK (json_valid(payload)),
  created_at   TEXT NOT NULL,
  key_name     TEXT NOT NULL
);
CREATE UNIQUE INDEX shift_reports_version ON shift_reports(lower(hub_username), report_date, version);
CREATE INDEX shift_reports_date ON shift_reports(report_date);
CREATE INDEX shift_reports_person ON shift_reports(person, report_date);

CREATE TABLE shift_report_pages (
  report_id     INTEGER NOT NULL REFERENCES shift_reports(id),
  seq           INTEGER NOT NULL,
  page_label    TEXT NOT NULL,
  amount_micros INTEGER NOT NULL,
  PRIMARY KEY (report_id, page_label)
);

CREATE TABLE shift_report_lines (
  report_id       INTEGER NOT NULL REFERENCES shift_reports(id),
  seq             INTEGER NOT NULL,
  n               INTEGER,
  transaction_ref TEXT,
  page_label      TEXT NOT NULL,
  fan_ref         TEXT NOT NULL DEFAULT '',
  fan_name        TEXT NOT NULL DEFAULT '',
  kind            TEXT NOT NULL DEFAULT '',
  occurred_at     TEXT,
  net_micros      INTEGER NOT NULL,
  gross_micros    INTEGER,
  counted         INTEGER NOT NULL CHECK (counted IN (0,1)),
  decision        TEXT NOT NULL DEFAULT '' CHECK (decision IN ('','auto','include','exclude')),
  added           INTEGER NOT NULL CHECK (added IN (0,1)),
  what            TEXT NOT NULL DEFAULT '',
  flags           TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(flags)),
  notes           TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(notes)),
  ppv_author      TEXT NOT NULL DEFAULT '',
  ppv_sent_at     TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (report_id, seq)
);
CREATE INDEX shift_report_lines_ref ON shift_report_lines(transaction_ref);

CREATE VIEW v_shift_reports AS
  SELECT r.id AS id,
         r.person AS person,
         r.hub_username AS hub_username,
         r.hub_user_id AS hub_user_id,
         r.chatter AS chatter,
         r.report_date AS report_date,
         r.window_from AS window_from,
         r.window_to AS window_to,
         r.cut_at AS cut_at,
         r.version AS version,
         (SELECT COUNT(*) FROM shift_reports x
           WHERE lower(x.hub_username) = lower(r.hub_username) AND x.report_date = r.report_date) AS versions,
         r.status AS status,
         r.source AS source,
         r.total_micros AS total_micros,
         (SELECT COUNT(*) FROM shift_report_pages p WHERE p.report_id = r.id) AS pages,
         (SELECT COUNT(*) FROM shift_report_lines l WHERE l.report_id = r.id AND l.added = 0) AS lines,
         (SELECT COUNT(*) FROM shift_report_lines l WHERE l.report_id = r.id AND l.added = 1) AS adds,
         r.created_at AS created_at,
         r.key_name AS key_name
  FROM shift_reports r
  WHERE r.version = (SELECT MAX(x.version) FROM shift_reports x
                      WHERE lower(x.hub_username) = lower(r.hub_username) AND x.report_date = r.report_date);

INSERT INTO schema_docs(tbl, col, description) VALUES
 ('shift_reports','', 'Отчёты смен чаттеров (R1): расклад, который агент команды собрал скриптом и сохранил (POST /v1/reports). Отчёт — пользователь хаба × дата; у него версии 1..N, действующая — наибольшая. Ничего не проводит и не начисляет'),
 ('shift_reports','id','Номер версии отчёта'),
 ('shift_reports','person','Человек (people.slug) по aliases карточки (имя хаба или hub:<id>); NULL — пользователь хаба не сопоставлен с карточкой'),
 ('shift_reports','hub_username','Имя пользователя хаба, как прислал агент; отчёт узнаётся по нему без регистра и дате'),
 ('shift_reports','hub_user_id','id пользователя хаба; если агент не прислал — из фактов хаба (hub_works) по имени, иначе NULL'),
 ('shift_reports','chatter','Ключ чаттера в списке агента, свободный текст'),
 ('shift_reports','report_date','Дата отчёта ГГГГ-ММ-ДД: день МСК, когда началась смена'),
 ('shift_reports','window_from','Начало окна смены в скрипте агента, МСК'),
 ('shift_reports','window_to','Конец окна смены, МСК'),
 ('shift_reports','cut_at','Срез: «по хабу на …» — до какого времени скрипт видел продажи, МСК'),
 ('shift_reports','version','Номер версии отчёта: 1, 2, …; новая — когда содержание изменилось'),
 ('shift_reports','total_micros','Итог отчёта, микродоллары: сумма страниц'),
 ('shift_reports','source','Откуда: agent — скрипт агента (позже post — пост в «Отчётности»)'),
 ('shift_reports','status','draft — черновик агента; posted — подтверждён постом чаттера (R2); superseded — заменён новой версией'),
 ('shift_reports','content_hash','SHA-256 содержания без времени приёма: тот же расклад повторно новой версии не создаёт'),
 ('shift_reports','payload','JSON запроса, как пришёл'),
 ('shift_reports','created_at','Когда записано, МСК'),
 ('shift_reports','key_name','Ключ API, которым записано'),
 ('shift_report_pages','', 'Строки аккаунтов отчёта смены: страница и её сумма'),
 ('shift_report_pages','report_id','Версия отчёта (shift_reports.id)'),
 ('shift_report_pages','seq','Порядок строки в отчёте'),
 ('shift_report_pages','page_label','Страница хаба (pageLabel)'),
 ('shift_report_pages','amount_micros','Сумма страницы в отчёте, микродоллары: включённые продажи плюс добавленные суммы'),
 ('shift_report_lines','', 'Продажи расклада отчёта смены: каждая продажа окна — в итоге или нет и почему; added = 1 — сумма, добавленная руками'),
 ('shift_report_lines','report_id','Версия отчёта (shift_reports.id)'),
 ('shift_report_lines','seq','Порядок строки в отчёте'),
 ('shift_report_lines','n','Номер строки, как напечатал скрипт агента; NULL у добавленной суммы'),
 ('shift_report_lines','transaction_ref','Продажа хаба (hub_sales.transaction_ref); NULL у добавленной суммы'),
 ('shift_report_lines','page_label','Страница хаба'),
 ('shift_report_lines','fan_ref','Фан: id хаба'),
 ('shift_report_lines','fan_name','Фан: ник'),
 ('shift_report_lines','kind','Вид продажи хаба (message_purchase, tip, …)'),
 ('shift_report_lines','occurred_at','Когда продажа, МСК'),
 ('shift_report_lines','net_micros','Нетто продажи (у добавленной — сумма), микродоллары'),
 ('shift_report_lines','gross_micros','Брутто продажи, микродоллары; NULL — неизвестно'),
 ('shift_report_lines','counted','1 — в итоге страницы, 0 — нет'),
 ('shift_report_lines','decision','Как решено: auto — по правилам скрипта, include — включена по решению, exclude — исключена по решению; пусто у добавленной'),
 ('shift_report_lines','added','1 — сумма добавлена руками (вне окна или мимо хаба), объяснение в what'),
 ('shift_report_lines','what','Объяснение добавленной суммы'),
 ('shift_report_lines','flags','JSON-массив строк: почему продажа не в итоге'),
 ('shift_report_lines','notes','JSON-массив строк: заметки скрипта'),
 ('shift_report_lines','ppv_author','Кто отправил PPV, как видит скрипт'),
 ('shift_report_lines','ppv_sent_at','Когда отправлен PPV, как напечатал скрипт'),
 ('v_shift_reports','', 'Действующая версия каждого отчёта смены (наибольшая) с состоянием и числом версий'),
 ('v_shift_reports','id','Версия отчёта (shift_reports.id)'),
 ('v_shift_reports','person','Человек (people.slug); NULL — не сопоставлен'),
 ('v_shift_reports','hub_username','Имя пользователя хаба'),
 ('v_shift_reports','hub_user_id','id пользователя хаба'),
 ('v_shift_reports','chatter','Ключ чаттера у агента'),
 ('v_shift_reports','report_date','Дата отчёта, день начала смены МСК'),
 ('v_shift_reports','window_from','Начало окна смены, МСК'),
 ('v_shift_reports','window_to','Конец окна смены, МСК'),
 ('v_shift_reports','cut_at','Срез «по хабу на …», МСК'),
 ('v_shift_reports','version','Номер действующей версии'),
 ('v_shift_reports','versions','Сколько версий у отчёта'),
 ('v_shift_reports','status','draft или posted'),
 ('v_shift_reports','source','Откуда: agent'),
 ('v_shift_reports','total_micros','Итог, микродоллары'),
 ('v_shift_reports','pages','Сколько страниц'),
 ('v_shift_reports','lines','Сколько продаж в раскладе'),
 ('v_shift_reports','adds','Сколько сумм добавлено руками'),
 ('v_shift_reports','created_at','Когда записана версия, МСК'),
 ('v_shift_reports','key_name','Ключ API, которым записана');

UPDATE schema_docs
   SET description = 'Права через запятую: read — SQL и схема, propose — предложения и споры, decide — решения владельца: бриф, правила, споры, заявки на ничьи продажи (только openclaw-main), collect — сборщики, import — разовые импорты, payouts — только GET /v1/payouts, GET /v1/payouts/prepare (выплаты чаттерам для хаба) и GET /v1/reports (отчёты смен), больше ничего, reports — только POST /v1/reports (агент команды сохраняет отчёты смен)'
 WHERE tbl = 'keys' AND col = 'scopes';
