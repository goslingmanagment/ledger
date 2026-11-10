-- Отчёты смен, этап R2 (план docs/plans/2026-11-10-shift-reports.md): начисления по постам «Отчётности» в тени и
-- переключение начислений людям на них с даты settings.payroll_reports_from. Пустая настройка — только таблицы, как
-- прежде: ни одной проводки эта миграция не пишет. С даты день человека начисляет report_accruals (выручка поста × ставка
-- вкладки модели плюс фикс за день), таблица — зеркало. Выплаченный день сам не меняется: изменение ждёт решения владельца
-- в payroll_pending_changes. Деньги — целые микродоллары, дни — МСК.

INSERT INTO settings(key, value, note, updated_at) VALUES ('payroll_reports_from', '',
  'С какого дня (МСК) начисления людям — из отчётов смен (report_accruals), а не из таблиц выплат; пусто — только таблицы. Ставит и снимает ledger payroll reports-from; откат — --off и пересборка. Вместе с payroll_facts_from не ставится',
  '2026-11-11T00:00:00+03:00')
ON CONFLICT(key) DO NOTHING;

CREATE TABLE report_accruals (
  person        TEXT NOT NULL,
  model         TEXT NOT NULL,
  day           TEXT NOT NULL,
  week          TEXT NOT NULL,
  pages         TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(pages)),
  posts         TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(posts)),
  net_micros    INTEGER NOT NULL,
  tab_model     TEXT,
  tab_name      TEXT,
  rate          REAL,
  fix_micros    INTEGER NOT NULL DEFAULT 0,
  amount_micros INTEGER,
  reason        TEXT,
  computed_at   TEXT NOT NULL,
  PRIMARY KEY (person, model, day)
);
CREATE INDEX report_accruals_week ON report_accruals(week, person);

-- What a reports accrual was posted from: a later change of the same day is told apart as an edited post or new terms.
CREATE TABLE report_accrual_entries (
  entry_id    INTEGER PRIMARY KEY REFERENCES entries(id),
  net_micros  INTEGER NOT NULL,
  rate        REAL,
  fix_micros  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE payroll_pending_changes (
  id             INTEGER PRIMARY KEY,
  person         TEXT NOT NULL,
  model          TEXT NOT NULL,
  name           TEXT NOT NULL,
  date           TEXT NOT NULL,
  posted_micros  INTEGER NOT NULL,
  new_micros     INTEGER NOT NULL,
  reason         TEXT NOT NULL CHECK (reason IN ('post_edited','new_post','terms_changed')),
  noticed_at     TEXT NOT NULL,
  state          TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','applied','dismissed')),
  decided_by     TEXT,
  decided_at     TEXT,
  note           TEXT
);
CREATE UNIQUE INDEX payroll_pending_changes_open ON payroll_pending_changes(model, name, date) WHERE state = 'open';
CREATE INDEX payroll_pending_changes_person ON payroll_pending_changes(person, state);

INSERT INTO schema_docs(tbl, col, description) VALUES
 ('report_accruals','', 'Начисления по отчётам смен: человек × модель × дата отчёта → выручка его постов «Отчётности» (v_report_pages, posted_micros страниц модели) × «Ставка %» его вкладки модели плюс «Фикс/см» один раз за день. Пересчитываются после каждого разбора постов, сохранения черновика и пересборки долгов. До settings.payroll_reports_from — тень (ничего не проводится); с неё суммы проводятся (accrual:payroll, source_key с «:reports»)'),
 ('report_accruals','person','Человек (people.slug)'),
 ('report_accruals','model','Модель страниц (как в v_report_pages); пусто — модель страницы неизвестна, сумма не считается'),
 ('report_accruals','day','Дата отчёта ГГГГ-ММ-ДД (день начала смены, как столбец таблицы выплат)'),
 ('report_accruals','week','Понедельник недели дня'),
 ('report_accruals','pages','Страницы дня: JSON [{"page": "<страница хаба>", "micros": <сумма поста>}]'),
 ('report_accruals','posts','Посты, из которых страницы: JSON-массив shift_report_posts.id'),
 ('report_accruals','net_micros','Выручка дня по постам: сумма страниц модели, микродоллары'),
 ('report_accruals','tab_model','Модель вкладки человека, на которую идёт день (payroll_tabs.model); NULL — вкладки нет'),
 ('report_accruals','tab_name','Имя вкладки человека (payroll_tabs.name); NULL — вкладки нет'),
 ('report_accruals','rate','«Ставка %» вкладки, доля (0.25)'),
 ('report_accruals','fix_micros','«Фикс/см» вкладки за день, микродоллары; 0 — фикса нет'),
 ('report_accruals','amount_micros','Начислено бы: выручка × ставка + фикс, микродоллары; NULL — «без ставки» (reason)'),
 ('report_accruals','reason','Почему сумма не посчитана: нет вкладки модели, нет «Ставка %», модель страницы неизвестна'),
 ('report_accruals','computed_at','Когда строка последний раз изменилась, МСК'),
 ('report_accrual_entries','', 'Из чего проведено начисление по отчёту (accrual:payroll с «:reports»): по нему поздняя правка выплаченного дня различается как правка поста или новые условия'),
 ('report_accrual_entries','entry_id','Проводка начисления (entries.id)'),
 ('report_accrual_entries','net_micros','Выручка дня по постам на момент проводки, микродоллары'),
 ('report_accrual_entries','rate','Ставка вкладки на момент проводки'),
 ('report_accrual_entries','fix_micros','Фикс за день на момент проводки, микродоллары'),
 ('payroll_pending_changes','', 'Изменения за выплаченные дни, которые ждут решения владельца: с settings.payroll_reports_from день, по которому уже прошла выплата (payable_days.paid > 0), сам не меняется — проведённое стоит, новое ждёт здесь. Применяет или отклоняет владелец: ledger payroll change apply|dismiss <id> (право decide). Новое изменение того же дня заменяет сумму открытого, а не добавляет второе'),
 ('payroll_pending_changes','id','Номер изменения'),
 ('payroll_pending_changes','person','Человек (people.slug)'),
 ('payroll_pending_changes','model','Модель вкладки'),
 ('payroll_pending_changes','name','Имя вкладки'),
 ('payroll_pending_changes','date','День ГГГГ-ММ-ДД'),
 ('payroll_pending_changes','posted_micros','Проведено за день сейчас, микродоллары'),
 ('payroll_pending_changes','new_micros','Сколько выходит по отчёту, микродоллары; 0 — день больше не начисляется'),
 ('payroll_pending_changes','reason','post_edited — пост изменён после проводки; new_post — отчёт пришёл, когда день стоял по таблице; terms_changed — та же выручка, другие ставка или фикс'),
 ('payroll_pending_changes','noticed_at','Когда изменение замечено последний раз, МСК'),
 ('payroll_pending_changes','state','open — ждёт решения; applied — применено (день сторнирован и проведён заново); dismissed — отклонено, книги как были'),
 ('payroll_pending_changes','decided_by','Кто решил: имя ключа, ledger — само (источник выключен, отчёт вернулся к проведённой сумме)'),
 ('payroll_pending_changes','decided_at','Когда решено, МСК'),
 ('payroll_pending_changes','note','Заметка решения');

UPDATE schema_docs
   SET description = 'Откуда день: sheet — таблица выплат, facts — факты хаба (с settings.payroll_facts_from), reports — отчёты смен (с settings.payroll_reports_from); у не-дней пусто'
 WHERE tbl = 'payable_days' AND col = 'source';

UPDATE schema_docs
   SET description = 'Дни вкладок: сумма дня в float точно как в таблице (процент по выручке дня плюс фикс поровну на дни с записями). Заменяются при каждом новом удачном разборе вкладки. С settings.payroll_facts_from или settings.payroll_reports_from дни таблицы — зеркало: начисляют факты хаба (fact_accruals) или отчёты смен (report_accruals), таблица только сравнивается'
 WHERE tbl = 'payroll_days' AND col = '';

UPDATE schema_docs
   SET description = description || '. С settings.payroll_reports_from пустая ячейка таблицы — не пробел: строки этих дней только unknown — reason «по отчёту не начислено: …» (отчёт не разобран, отчёта нет, нет ставки) или «смена без отчёта: …» (у смены хаба нет опубликованного отчёта)'
 WHERE tbl = 'payroll_unentered' AND col = '';

UPDATE schema_docs
   SET description = 'Права через запятую: read — SQL и схема, propose — предложения и споры, decide — решения владельца: бриф, правила, споры, заявки на ничьи продажи, изменения за выплаченные дни (только openclaw-main), collect — сборщики, import — разовые импорты, payouts — только GET /v1/payouts, GET /v1/payouts/prepare (выплаты чаттерам для хаба), GET /v1/reports (отчёты смен) и GET /v1/report-days (база оплаты по отчётам), больше ничего, reports — только POST /v1/reports (агент команды сохраняет отчёты смен)'
 WHERE tbl = 'keys' AND col = 'scopes';
