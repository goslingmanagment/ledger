-- Долги людям (выпуск 1, PR6): карточки людей, состояние таблиц выплат, дни, FIFO «за какие дни»,
-- «не занесено» по хабу и замороженные стартовые долги. Проводки живут в entries/lines, сырые вкладки —
-- в events (source 'sheets': kind tab, table и run — след каждого прогона, по нему видно, что сборщик жив);
-- всё здесь, кроме people и payroll_openings, выводится из них заново.
-- Суммы payroll_days и шапки вкладок — float, как в таблице: из них карточка месяца воспроизводит
-- расчёт по таблицам до цента. Суммы payable_days — целые микродоллары, как в lines.

CREATE TABLE people (
  slug        TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  aliases     TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(aliases)),
  status      TEXT NOT NULL DEFAULT '',
  position    TEXT NOT NULL DEFAULT '',
  pages       TEXT NOT NULL DEFAULT '',
  schedule    TEXT,
  workdays    TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(workdays)),
  rate        TEXT NOT NULL DEFAULT '',
  models      TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(models)),
  source      TEXT NOT NULL,
  imported_at TEXT NOT NULL
);

CREATE TABLE payroll_tables (
  model      TEXT PRIMARY KEY,
  status     TEXT NOT NULL CHECK (status IN ('ok','error')),
  error      TEXT,
  tabs       TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(tabs)),
  fetched_at TEXT NOT NULL,
  event_id   INTEGER NOT NULL REFERENCES events(id)
);

CREATE TABLE payroll_tabs (
  model           TEXT NOT NULL,
  name            TEXT NOT NULL,
  person          TEXT,
  status          TEXT NOT NULL CHECK (status IN ('ok','error')),
  error           TEXT,
  fetched_at      TEXT NOT NULL,
  kv              REAL,
  paid            REAL,
  total           REAL,
  rate            REAL,
  notes           TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(notes)),
  event_id        INTEGER NOT NULL REFERENCES events(id),
  ok_event_id     INTEGER REFERENCES events(id),
  ok_at           TEXT,
  posted_event_id INTEGER REFERENCES events(id),
  listed          INTEGER NOT NULL DEFAULT 1 CHECK (listed IN (0,1)),
  PRIMARY KEY (model, name)
);

CREATE TABLE payroll_days (
  model    TEXT NOT NULL,
  name     TEXT NOT NULL,
  person   TEXT,
  date     TEXT NOT NULL,
  amount   REAL NOT NULL,
  worked   INTEGER NOT NULL CHECK (worked IN (0,1)),
  week     TEXT NOT NULL,
  pages    TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(pages)),
  event_id INTEGER NOT NULL REFERENCES events(id),
  PRIMARY KEY (model, name, date)
);
CREATE INDEX payroll_days_person ON payroll_days(person, date);

CREATE TABLE payable_days (
  person  TEXT NOT NULL,
  seq     INTEGER NOT NULL,
  kind    TEXT NOT NULL CHECK (kind IN ('opening','day','other','advance')),
  date    TEXT NOT NULL,
  model   TEXT NOT NULL DEFAULT '',
  name    TEXT NOT NULL DEFAULT '',
  amount  INTEGER NOT NULL,
  paid    INTEGER NOT NULL,
  unpaid  INTEGER NOT NULL,
  paid_by TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(paid_by)),
  PRIMARY KEY (person, seq)
);

CREATE TABLE payroll_unentered (
  model      TEXT NOT NULL,
  name       TEXT NOT NULL,
  person     TEXT NOT NULL,
  date       TEXT NOT NULL,
  page       TEXT NOT NULL,
  window_from TEXT,
  window_to  TEXT,
  status     TEXT NOT NULL CHECK (status IN ('ok','unknown')),
  reason     TEXT,
  net_mills  INTEGER,
  share      REAL,
  checked_at TEXT NOT NULL,
  PRIMARY KEY (model, name, date, page)
);
CREATE INDEX payroll_unentered_person ON payroll_unentered(person, date);

CREATE TABLE payroll_openings (
  person   TEXT PRIMARY KEY,
  amount   INTEGER NOT NULL,
  kv       REAL NOT NULL,
  accrued  INTEGER NOT NULL,
  paid     INTEGER NOT NULL,
  flags    TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(flags)),
  entry_id INTEGER REFERENCES entries(id),
  at       TEXT NOT NULL
);
CREATE TRIGGER payroll_openings_u BEFORE UPDATE ON payroll_openings BEGIN SELECT RAISE(ABORT, 'openings are frozen'); END;
CREATE TRIGGER payroll_openings_d BEFORE DELETE ON payroll_openings BEGIN SELECT RAISE(ABORT, 'openings are frozen'); END;

CREATE VIEW v_payables AS
  WITH who AS (
    SELECT subject AS person FROM accounts WHERE grp = 'payable' AND subject IS NOT NULL
    UNION SELECT person FROM payroll_tabs WHERE person IS NOT NULL
  ),
  bal AS (
    SELECT substr(account, 9) AS person, -SUM(amount) AS micros FROM lines WHERE account LIKE 'payable:%' GROUP BY account
  ),
  tabs AS (
    SELECT person, COUNT(*) AS tabs, SUM(kv) AS kv, SUM(status <> 'ok') AS bad, MIN(fetched_at) AS fetched_at
    FROM payroll_tabs WHERE person IS NOT NULL GROUP BY person
  ),
  unpaid AS (
    SELECT person, MIN(date) AS oldest, group_concat(item, ', ') AS days
    FROM (SELECT person, date,
                 CASE kind WHEN 'opening' THEN 'старт ' ELSE date || ' ' END || printf('%.2f', unpaid / 1e6) AS item
          FROM payable_days WHERE unpaid > 0 ORDER BY person, seq)
    GROUP BY person
  ),
  blank AS (
    SELECT person,
           COUNT(DISTINCT CASE WHEN status = 'ok' THEN model || '/' || name || '/' || date END) AS days,
           SUM(CASE WHEN status = 'ok' THEN net_mills END) / 1000.0 AS net,
           SUM(CASE WHEN status = 'ok' THEN share END) AS share,
           SUM(status = 'unknown') AS unknown
    FROM payroll_unentered GROUP BY person
  )
  SELECT who.person AS person,
         COALESCE(p.name, who.person) AS name,
         COALESCE(bal.micros, 0) AS balance_micros,
         ROUND(COALESCE(bal.micros, 0) / 1e6, 2) AS balance,
         tabs.kv AS to_pay,
         CASE WHEN tabs.kv IS NOT NULL THEN ROUND(COALESCE(bal.micros, 0) / 1e6 - tabs.kv, 2) END AS difference,
         unpaid.oldest AS oldest_unpaid,
         unpaid.days AS unpaid_days,
         COALESCE(blank.days, 0) AS unentered_days,
         ROUND(COALESCE(blank.net, 0), 2) AS unentered_net,
         ROUND(COALESCE(blank.share, 0), 2) AS unentered_share,
         COALESCE(blank.unknown, 0) AS unentered_unknown,
         tabs.tabs AS tabs,
         CASE WHEN tabs.tabs IS NULL THEN NULL WHEN tabs.bad > 0 THEN 'error' ELSE 'ok' END AS tabs_status,
         tabs.fetched_at AS fetched_at
  FROM who
  LEFT JOIN people p ON p.slug = who.person
  LEFT JOIN bal ON bal.person = who.person
  LEFT JOIN tabs ON tabs.person = who.person
  LEFT JOIN unpaid ON unpaid.person = who.person
  LEFT JOIN blank ON blank.person = who.person;

INSERT OR IGNORE INTO schema_docs(tbl, col, description) VALUES
 ('people','', 'Карточки людей из /OS people/*.md (ledger import people): кто есть кто, график, страницы, ставка текстом. По имени и псевдонимам вкладка и строка журнала находят человека'),
 ('people','slug','Код человека: имя файла карточки; счёт долга — payable:<slug>'),
 ('people','name','Имя (title карточки)'),
 ('people','aliases','Другие имена человека, JSON-массив'),
 ('people','status','active, testing, departed'),
 ('people','position','Должность'),
 ('people','pages','Страницы текстом (account карточки)'),
 ('people','schedule','Слот смены по МСК, например 16:00-00:00; NULL — нет графика'),
 ('people','workdays','Рабочие дни, JSON-массив (mon…sun). Ночной слот датируется вечером входа: mon при слоте 00:00–08:00 — это ячейка вторника'),
 ('people','rate','Ставка текстом из карточки'),
 ('people','models','Модели человека, JSON-массив; решает, чья вкладка, когда имя одно на двоих'),
 ('people','source','Откуда импортировано'),
 ('people','imported_at','Когда импортировано, МСК'),
 ('payroll_tables','', 'Таблицы выплат моделей: последний прочитанный список вкладок из «Итого» и «Неактивные»'),
 ('payroll_tables','model','Модель таблицы'),
 ('payroll_tables','status','ok — список прочитан; error — таблица целиком не прочитана'),
 ('payroll_tables','error','Почему не прочитана'),
 ('payroll_tables','tabs','Имена вкладок из списков, JSON-массив'),
 ('payroll_tables','fetched_at','Когда сборщик читал таблицу, МСК'),
 ('payroll_tables','event_id','Событие последнего чтения (events, source sheets)'),
 ('payroll_tabs','', 'Последнее состояние каждой вкладки выплат; вся история — в events (source sheets, kind tab). При status error суммы остаются от последнего удачного разбора'),
 ('payroll_tabs','model','Модель таблицы'),
 ('payroll_tabs','name','Имя вкладки'),
 ('payroll_tabs','person','Человек (people.slug); NULL — имя не сопоставлено, начислений нет, это в ledger payables'),
 ('payroll_tabs','status','ok — вкладка прошла строгий разбор; error — нет'),
 ('payroll_tabs','error','Текст отказа разбора или чтения'),
 ('payroll_tabs','fetched_at','Когда сборщик читал вкладку последний раз, МСК'),
 ('payroll_tabs','kv','«К выплате» из шапки вкладки'),
 ('payroll_tabs','paid','«Выплачено» из шапки вкладки'),
 ('payroll_tabs','total','Сумма «З/П» всех недельных блоков'),
 ('payroll_tabs','rate','«Ставка %» из шапки: доля от выручки, например 0.25'),
 ('payroll_tabs','notes','Замечания разбора, JSON-массив'),
 ('payroll_tabs','event_id','Событие последнего чтения'),
 ('payroll_tabs','ok_event_id','Событие последнего удачного разбора: из него дни и суммы'),
 ('payroll_tabs','ok_at','Когда прочитан этот удачный разбор, МСК'),
 ('payroll_tabs','posted_event_id','Какой удачный разбор уже проведён начислениями'),
 ('payroll_tabs','listed','1 — вкладка есть в списках «Итого»/«Неактивные»; 0 — пропала, её начисления остаются, пока не решит человек'),
 ('payroll_days','', 'Дни вкладок: сумма дня в float точно как в таблице (процент по выручке дня плюс фикс поровну на дни с записями). Заменяются при каждом новом удачном разборе вкладки'),
 ('payroll_days','model','Модель таблицы'),
 ('payroll_days','name','Имя вкладки'),
 ('payroll_days','person','Человек (people.slug) или NULL'),
 ('payroll_days','date','День, ГГГГ-ММ-ДД'),
 ('payroll_days','amount','Начислено за день, доллары, float'),
 ('payroll_days','worked','1 — в дне есть запись (хотя бы 0); 0 — ячейки пустые'),
 ('payroll_days','week','Первый день недельного блока'),
 ('payroll_days','pages','Значения дня по страницам блока, JSON: страница → число или null'),
 ('payroll_days','event_id','Событие удачного разбора'),
 ('payable_days','', 'FIFO «за какие дни»: выплата гасит самые старые неоплаченные дни (сначала стартовый долг). Строится заново при каждом изменении, учётом не является: учёт — проводки на payable:<slug>'),
 ('payable_days','person','Человек'),
 ('payable_days','seq','Порядок гашения внутри человека'),
 ('payable_days','kind','opening — стартовый долг, day — день вкладки, other — ручная проводка на счёт долга, advance — выплачено больше, чем начислено'),
 ('payable_days','date','Дата дня (у стартового долга — дата проводки)'),
 ('payable_days','model','Модель вкладки дня'),
 ('payable_days','name','Вкладка дня'),
 ('payable_days','amount','Начислено, микродоллары'),
 ('payable_days','paid','Погашено выплатами, микродоллары'),
 ('payable_days','unpaid','Осталось, микродоллары (у advance — минус)'),
 ('payable_days','paid_by','Номера строк журнала, которые погасили этот день, JSON-массив'),
 ('payroll_unentered','', '«Не занесено»: дни вкладок без значения, по которым хаб показывает деньги на страницах недели в окне смены человека, за последние 45 дней. Только сведения: в долг не идут'),
 ('payroll_unentered','model','Модель таблицы'),
 ('payroll_unentered','name','Имя вкладки'),
 ('payroll_unentered','person','Человек'),
 ('payroll_unentered','date','День вкладки (календарная дата начала смены)'),
 ('payroll_unentered','page','Страница хаба или, если не сопоставлена, строка вкладки'),
 ('payroll_unentered','window_from','Начало окна смены, МСК'),
 ('payroll_unentered','window_to','Конец окна смены, МСК'),
 ('payroll_unentered','status','ok — хаб доказал данные; unknown — «неизвестно», это не ноль'),
 ('payroll_unentered','reason','Почему неизвестно'),
 ('payroll_unentered','net_mills','Net хаба за окно, тысячные доллара; при unknown — NULL'),
 ('payroll_unentered','share','Доля человека по ставке вкладки, доллары; NULL, если неизвестно'),
 ('payroll_unentered','checked_at','Когда посчитано, МСК'),
 ('payroll_openings','', 'Стартовый долг каждого человека (ledger payroll open): неоплаченные дни до даты начисления. Посчитан один раз и заморожен, нулевой тоже'),
 ('payroll_openings','person','Человек'),
 ('payroll_openings','amount','Стартовый долг, микродоллары'),
 ('payroll_openings','kv','«К выплате» всех его вкладок в момент расчёта'),
 ('payroll_openings','accrued','Начислено с даты начисления на момент расчёта, микродоллары'),
 ('payroll_openings','paid','Выплачено по таблице с даты начисления на момент расчёта, микродоллары'),
 ('payroll_openings','flags','Что насторожило при расчёте, JSON-массив'),
 ('payroll_openings','entry_id','Проводка; NULL при нулевом долге'),
 ('payroll_openings','at','Когда заморожен, МСК'),
 ('v_payables','', 'Кому сколько должны и за какие дни: долг по проводкам против «К выплате» вкладок, неоплаченные дни, «не занесено» отдельно'),
 ('v_payables','person','Человек: people.slug или субъект счёта payable (доля модели по сделкам)'),
 ('v_payables','name','Имя'),
 ('v_payables','balance_micros','Долг по проводкам, микродоллары, плюс — должны мы'),
 ('v_payables','balance','Долг по проводкам, доллары'),
 ('v_payables','to_pay','«К выплате» всех его вкладок; NULL — вкладок нет'),
 ('v_payables','difference','Долг минус «К выплате»; норма ±1'),
 ('v_payables','oldest_unpaid','Самый старый неоплаченный день'),
 ('v_payables','unpaid_days','Неоплаченные дни текстом: дата и сумма'),
 ('v_payables','unentered_days','«Не занесено»: сколько дней по хабу с деньгами. В долг не идёт'),
 ('v_payables','unentered_net','«Не занесено»: net хаба за эти дни, доллары'),
 ('v_payables','unentered_share','«Не занесено»: доля человека по ставке вкладки, доллары'),
 ('v_payables','unentered_unknown','Сколько строк «неизвестно»: хаб не доказал данные, это не ноль'),
 ('v_payables','tabs','Сколько у него вкладок'),
 ('v_payables','tabs_status','ok или error, если хоть одна вкладка в ошибке'),
 ('v_payables','fetched_at','Самое старое чтение его вкладок, МСК');
