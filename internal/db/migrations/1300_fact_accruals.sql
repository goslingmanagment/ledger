-- Выпуск 2, L3: начисления из фактов хаба в тени. Каждая продажа доказанных окон hub_team_sync проходит правила
-- дележа (internal/split): доли смен и заходов — людям по их ставке (тот же разбор шапки вкладки, что у таблиц выплат:
-- «Ставка %» от выручки дня, «Фикс/см» за день со сменой), общак — суммой со списком, «ждёт», «решить», «ничей» —
-- списком. Ничего из этого не проводится: долги, payable_days и v_payables не меняются, пока владелец не скажет «да»
-- по таблице «было — стало» (L4). Всё здесь строится заново после каждого чтения фактов. Деньги — целые
-- микродоллары (net хаба в mills × 1000), дни — МСК.

CREATE TABLE fact_homes (
  transaction_ref TEXT NOT NULL,
  seq             INTEGER NOT NULL,
  window_from     TEXT NOT NULL,
  occurred_at     TEXT NOT NULL,
  page_label      TEXT NOT NULL,
  model           TEXT NOT NULL,
  sale_kind       TEXT NOT NULL,
  net_micros      INTEGER NOT NULL,
  home            TEXT NOT NULL CHECK (home IN ('shift','visit','pot','waiting','decide','nobody','before_rule','unknown')),
  reason          TEXT NOT NULL,
  pot             TEXT,
  waits_until     TEXT,
  hub_user_id     INTEGER,
  username        TEXT,
  person          TEXT,
  work_ref        TEXT,
  percent         INTEGER,
  share_micros    INTEGER NOT NULL,
  day             TEXT NOT NULL,
  week            TEXT NOT NULL,
  holders         TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(holders)),
  PRIMARY KEY (transaction_ref, seq)
);
CREATE INDEX fact_homes_week ON fact_homes(week, home);
CREATE INDEX fact_homes_person ON fact_homes(person, day);

CREATE TABLE fact_accruals (
  hub_user_id   INTEGER NOT NULL,
  username      TEXT NOT NULL,
  person        TEXT,
  model         TEXT NOT NULL,
  page_label    TEXT NOT NULL,
  day           TEXT NOT NULL,
  week          TEXT NOT NULL,
  kind          TEXT NOT NULL CHECK (kind IN ('sales','fix')),
  sales         INTEGER NOT NULL,
  net_micros    INTEGER NOT NULL,
  rate          REAL,
  amount_micros INTEGER,
  reason        TEXT,
  PRIMARY KEY (hub_user_id, model, page_label, day)
);
CREATE INDEX fact_accruals_person ON fact_accruals(person, week);

CREATE TABLE fact_weeks (
  week        TEXT PRIMARY KEY,
  status      TEXT NOT NULL CHECK (status IN ('ok','unknown')),
  reasons     TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(reasons)),
  read_at     TEXT,
  hash        TEXT NOT NULL DEFAULT '',
  computed_at TEXT NOT NULL
);

CREATE VIEW v_accrual_diff AS
  WITH sheet AS (
    SELECT person, date(date, '-6 days', 'weekday 1') AS week, SUM(amount) AS amount
    FROM payroll_days WHERE person IS NOT NULL AND amount <> 0 GROUP BY person, week
  ),
  facts AS (
    SELECT person, week, SUM(COALESCE(amount_micros, 0)) AS micros, SUM(net_micros) AS net,
           SUM(CASE WHEN amount_micros IS NULL THEN net_micros ELSE 0 END) AS unpriced
    FROM fact_accruals WHERE person IS NOT NULL GROUP BY person, week
  ),
  keys AS (SELECT person, week FROM sheet UNION SELECT person, week FROM facts)
  SELECT k.person AS person,
         COALESCE(p.name, k.person) AS name,
         k.week AS week,
         w.status AS facts_status,
         CAST(ROUND(COALESCE(s.amount, 0) * 1000000) AS INTEGER) AS sheet_micros,
         ROUND(COALESCE(s.amount, 0), 2) AS sheet,
         CASE WHEN w.status = 'ok' THEN COALESCE(f.micros, 0) END AS facts_micros,
         CASE WHEN w.status = 'ok' THEN ROUND(COALESCE(f.micros, 0) / 1e6, 2) END AS facts,
         CASE WHEN w.status = 'ok' THEN COALESCE(f.micros, 0) - CAST(ROUND(COALESCE(s.amount, 0) * 1000000) AS INTEGER) END
           AS difference_micros,
         CASE WHEN w.status = 'ok' THEN ROUND((COALESCE(f.micros, 0) - ROUND(COALESCE(s.amount, 0) * 1000000)) / 1e6, 2) END
           AS difference,
         CASE WHEN w.status = 'ok' THEN COALESCE(f.net, 0) END AS facts_net_micros,
         CASE WHEN w.status = 'ok' THEN COALESCE(f.unpriced, 0) END AS unpriced_net_micros
  FROM keys k
  JOIN fact_weeks w ON w.week = k.week
  LEFT JOIN sheet s ON s.person = k.person AND s.week = k.week
  LEFT JOIN facts f ON f.person = k.person AND f.week = k.week
  LEFT JOIN people p ON p.slug = k.person;

INSERT INTO schema_docs(tbl, col, description) VALUES
 ('fact_homes','', 'Чья каждая продажа доказанных окон хаба по правилам дележа (internal/split), в тени: строка — доля продажи (у продажи без долей — одна строка на всю сумму). Ничего не проводится'),
 ('fact_homes','transaction_ref','Продажа (hub_sales)'),
 ('fact_homes','seq','Номер доли внутри продажи, с 0'),
 ('fact_homes','window_from','Недельное окно продажи'),
 ('fact_homes','occurred_at','Когда куплено, МСК'),
 ('fact_homes','page_label','Страница хаба'),
 ('fact_homes','model','Модель страницы (hub_pages и hub_model_slugs); пусто — страница не в гранте'),
 ('fact_homes','sale_kind','ppv, tip, subscription, post'),
 ('fact_homes','net_micros','Net всей продажи, микродоллары'),
 ('fact_homes','home','shift/visit — смене или заходу человека; pot — в общак; waiting — PPV ждёт смены отправителя; decide — ждёт решения владельца или согласных меток; nobody — ничей; before_rule — по-старому, платит таблица; unknown — факты не полны'),
 ('fact_homes','reason','Почему так (split.Reason…: ppv_sender, last_writer, holder, post_tip, campaign, no_last_writer, marks, owner_split…)'),
 ('fact_homes','pot','Какой общак (obshak)'),
 ('fact_homes','waits_until','До какого момента PPV ждёт смены отправителя, МСК'),
 ('fact_homes','hub_user_id','Кому доля: пользователь хаба'),
 ('fact_homes','username','Имя пользователя хаба'),
 ('fact_homes','person','Человек ledger (people.slug) по aliases карточки (имя хаба или hub:<id>); NULL — не сопоставлен'),
 ('fact_homes','work_ref','Смена или заход, которому доля (hub_works.ref); NULL — доля без держащей работы'),
 ('fact_homes','percent','Процент доли от net'),
 ('fact_homes','share_micros','Сколько этой строке, микродоллары: доля, или вся сумма у продажи без долей'),
 ('fact_homes','day','День МСК, на который приходятся деньги: день работы, которой доля (середина смены, dayOfWork хаба); без работы — день продажи'),
 ('fact_homes','week','Понедельник недели дня'),
 ('fact_homes','holders','Работы, державшие продажу: JSON [{userId, person, day, kind}]; по ним видно, чьей смены продажа ушла не ему'),
 ('fact_accruals','', 'Начисления из фактов в тени: пользователь хаба × модель × страница × день → net его долей и сумма по ставке его вкладки модели. kind fix — фикс «Фикс/см» за день со сменой, страница пустая. Читать с fact_weeks: при unknown неделя «неизвестно», не ноль'),
 ('fact_accruals','hub_user_id','Пользователь хаба'),
 ('fact_accruals','username','Имя пользователя хаба'),
 ('fact_accruals','person','Человек (people.slug); NULL — не сопоставлен, сумма не считается'),
 ('fact_accruals','model','Модель страницы'),
 ('fact_accruals','page_label','Страница хаба; пусто у фикса'),
 ('fact_accruals','day','День МСК (день работы)'),
 ('fact_accruals','week','Понедельник недели дня'),
 ('fact_accruals','kind','sales — доли продаж; fix — фикс за день со сменой'),
 ('fact_accruals','sales','Сколько долей продаж'),
 ('fact_accruals','net_micros','Net долей, микродоллары'),
 ('fact_accruals','rate','«Ставка %» вкладки человека в таблице модели, доля (0.25)'),
 ('fact_accruals','amount_micros','Начислено бы: net × ставка или фикс, микродоллары; NULL — ставки нет (reason)'),
 ('fact_accruals','reason','Почему сумма не посчитана'),
 ('fact_weeks','', 'Доказаны ли факты недели: окна от двух недель до неё до следующей (PPV ждёт 7 дней, ночная смена держит продажи после полуночи) все ok'),
 ('fact_weeks','week','Понедельник недели'),
 ('fact_weeks','status','ok или unknown'),
 ('fact_weeks','reasons','Какие окна не доказаны и почему (JSON-массив)'),
 ('fact_weeks','read_at','Самое раннее чтение нужных окон, МСК'),
 ('fact_weeks','hash','Хеш строк недели: неделя, чьи строки не изменились, не переписывается'),
 ('fact_weeks','computed_at','Когда строки недели последний раз изменились, МСК'),
 ('v_accrual_diff','', 'Было — стало по человеку и неделе: дни таблиц выплат (payroll_days) против начислений из фактов хаба (fact_accruals), в тени. При facts_status unknown факты NULL — «неизвестно», не ноль'),
 ('v_accrual_diff','person','Человек'),
 ('v_accrual_diff','name','Имя'),
 ('v_accrual_diff','week','Понедельник недели'),
 ('v_accrual_diff','facts_status','ok или unknown (fact_weeks)'),
 ('v_accrual_diff','sheet_micros','Начислено по таблице за неделю, микродоллары'),
 ('v_accrual_diff','sheet','Начислено по таблице, доллары'),
 ('v_accrual_diff','facts_micros','Начислено бы по фактам, микродоллары'),
 ('v_accrual_diff','facts','Начислено бы по фактам, доллары'),
 ('v_accrual_diff','difference_micros','Факты минус таблица, микродоллары'),
 ('v_accrual_diff','difference','Факты минус таблица, доллары'),
 ('v_accrual_diff','facts_net_micros','Net долей продаж по фактам, микродоллары'),
 ('v_accrual_diff','unpriced_net_micros','Net долей, для которых ставки нет (сумма не посчитана), микродоллары');
