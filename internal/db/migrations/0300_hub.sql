-- Импорт хаба (выпуск 1, PR5): грант ключа, зеркала транзакций, выплат Fansly и заявок OnlyFans и
-- статус честности каждой выгрузки. Зеркало — текущее состояние хаба за проверенное окно; каждая
-- новая или изменённая версия строки ещё и событие в events (source 'hub', kind = датасет).
-- Время — МСК '+03:00' (строки зеркал — с миллисекундами, как в хабе), суммы хаба — целые mills
-- (1 mill = $0.001), как в ответах хаба.

CREATE TABLE hub_pages (
  page_label TEXT PRIMARY KEY,
  platform   TEXT NOT NULL,
  model_slug TEXT NOT NULL,
  model_name TEXT NOT NULL,
  seen_at    TEXT NOT NULL
);

CREATE TABLE hub_transactions (
  page_label    TEXT NOT NULL,
  key           TEXT NOT NULL,
  occurred_at   TEXT NOT NULL,
  net_mills     INTEGER,
  currency      TEXT,
  type          TEXT,
  ref           TEXT,
  fields        TEXT NOT NULL CHECK (json_valid(fields)),
  hash          TEXT NOT NULL,
  event_id      INTEGER NOT NULL REFERENCES events(id),
  first_seen_at TEXT NOT NULL,
  updated_at    TEXT NOT NULL,
  PRIMARY KEY (page_label, key)
);
CREATE INDEX hub_transactions_time ON hub_transactions(page_label, occurred_at);

CREATE TABLE hub_payouts (
  page_label          TEXT NOT NULL,
  key                 TEXT NOT NULL,
  requested_at        TEXT NOT NULL,
  amount_mills        INTEGER,
  status              TEXT,
  method              TEXT,
  updated_at_platform TEXT,
  payout_ref          TEXT,
  fields              TEXT NOT NULL CHECK (json_valid(fields)),
  hash                TEXT NOT NULL,
  event_id            INTEGER NOT NULL REFERENCES events(id),
  first_seen_at       TEXT NOT NULL,
  updated_at          TEXT NOT NULL,
  PRIMARY KEY (page_label, key)
);
CREATE INDEX hub_payouts_time ON hub_payouts(page_label, requested_at);

CREATE TABLE hub_of_payouts (
  page_label    TEXT NOT NULL,
  key           TEXT NOT NULL,
  requested_at  TEXT NOT NULL,
  amount_mills  INTEGER,
  currency      TEXT,
  state         TEXT,
  payout_ref    TEXT,
  fields        TEXT NOT NULL CHECK (json_valid(fields)),
  hash          TEXT NOT NULL,
  event_id      INTEGER NOT NULL REFERENCES events(id),
  first_seen_at TEXT NOT NULL,
  updated_at    TEXT NOT NULL,
  PRIMARY KEY (page_label, key)
);
CREATE INDEX hub_of_payouts_time ON hub_of_payouts(page_label, requested_at);

CREATE TABLE hub_sync (
  page_label    TEXT NOT NULL,
  dataset       TEXT NOT NULL,
  status        TEXT NOT NULL CHECK (status IN ('ok','unknown')),
  reasons       TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(reasons)),
  blockers      TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(blockers)),
  window_from   TEXT,
  window_to     TEXT,
  rows          INTEGER,
  net_mills     INTEGER,
  capture_floor TEXT,
  mirror_from   TEXT,
  full_at       TEXT,
  requests      INTEGER NOT NULL DEFAULT 0,
  checked_at    TEXT NOT NULL,
  ok_at         TEXT,
  PRIMARY KEY (page_label, dataset)
);

-- Описания для `ledger schema`. INSERT OR IGNORE: описание из другой ветки с тем же (tbl, col) остаётся.
INSERT OR IGNORE INTO schema_docs(tbl, col, description) VALUES
 ('hub_pages','', 'Страницы гранта ключа хаба (hub capabilities): платформа и модель. Заменяется каждой выгрузкой'),
 ('hub_pages','page_label','Страница хаба (pageLabel)'),
 ('hub_pages','platform','fansly или onlyfans'),
 ('hub_pages','model_slug','modelSlug хаба; в модель ledger переводит настройка hub_model_slugs'),
 ('hub_pages','model_name','modelName хаба'),
 ('hub_pages','seen_at','Когда грант прочитан последний раз, МСК'),
 ('hub_transactions','', 'Зеркало транзакций хаба (без payout_reversal) за доказанное окно: строка = строка хаба, сверено со сводкой хаба того же окна. Читать вместе с hub_sync: при status unknown деньги страницы «неизвестно»'),
 ('hub_transactions','page_label','Страница хаба'),
 ('hub_transactions','key','Ключ строки в хабе (стабильный внутри датасета)'),
 ('hub_transactions','occurred_at','occurredAt хаба, МСК с миллисекундами'),
 ('hub_transactions','net_mills','netMills хаба: net после комиссии платформы, тысячные доллара'),
 ('hub_transactions','currency','Валюта строки; в расчёты идёт только USD'),
 ('hub_transactions','type','transactionType хаба (subscription, tip, message_purchase, post_purchase, other, refund…)'),
 ('hub_transactions','ref','transactionRef хаба; по нему строка сверяется с hub_exceptions'),
 ('hub_transactions','fields','Хранимые поля строки как в ответе хаба (JSON)'),
 ('hub_transactions','hash','Короткий хеш хранимых полей; новая версия строки — новый хеш и новое событие'),
 ('hub_transactions','event_id','Событие текущей версии строки (events, source hub)'),
 ('hub_transactions','first_seen_at','Когда ключ появился в зеркале, МСК'),
 ('hub_transactions','updated_at','Когда получена текущая версия, МСК'),
 ('hub_payouts','', 'Зеркало заявок на вывод Fansly (датасет payouts) за доказанное окно. Список доказан полным: одна страница ответа, точный счёт, без обрезки'),
 ('hub_payouts','page_label','Страница хаба'),
 ('hub_payouts','key','Ключ заявки в хабе'),
 ('hub_payouts','requested_at','requestedAt хаба, МСК с миллисекундами'),
 ('hub_payouts','amount_mills','amountMills: сумма заявки, тысячные доллара'),
 ('hub_payouts','status','statusLabel хаба (Processed — выведено)'),
 ('hub_payouts','method','methodProviderLabel хаба; допустимые — настройка fansly_payout_methods'),
 ('hub_payouts','updated_at_platform','updatedAtPlatform хаба, МСК'),
 ('hub_payouts','payout_ref','payoutRef: номер заявки на платформе'),
 ('hub_payouts','fields','Хранимые поля заявки как в ответе хаба (JSON)'),
 ('hub_payouts','hash','Короткий хеш хранимых полей; новая версия — новый хеш и новое событие'),
 ('hub_payouts','event_id','Событие текущей версии заявки (events, source hub)'),
 ('hub_payouts','first_seen_at','Когда заявка появилась в зеркале, МСК'),
 ('hub_payouts','updated_at','Когда получена текущая версия, МСК'),
 ('hub_of_payouts','', 'Зеркало заявок на вывод OnlyFans (датасет ofapi_payout_requests) за доказанное окно: строка на инвойс, последнее наблюдение'),
 ('hub_of_payouts','page_label','Страница хаба'),
 ('hub_of_payouts','key','Ключ заявки в хабе'),
 ('hub_of_payouts','requested_at','requestedAt хаба, МСК с миллисекундами'),
 ('hub_of_payouts','amount_mills','amountMills: сумма заявки, тысячные доллара'),
 ('hub_of_payouts','currency','Валюта заявки; в расчёты идёт только USD'),
 ('hub_of_payouts','state','state хаба: approved — выведено, new — ещё нет'),
 ('hub_of_payouts','payout_ref','payoutRef: номер инвойса OnlyFans'),
 ('hub_of_payouts','fields','Хранимые поля заявки как в ответе хаба (JSON)'),
 ('hub_of_payouts','hash','Короткий хеш хранимых полей; новая версия — новый хеш и новое событие'),
 ('hub_of_payouts','event_id','Событие текущей версии заявки (events, source hub)'),
 ('hub_of_payouts','first_seen_at','Когда заявка появилась в зеркале, МСК'),
 ('hub_of_payouts','updated_at','Когда получена текущая версия, МСК'),
 ('hub_sync','', 'Что хаб доказал по каждой странице и датасету. status unknown — ответ не принят (блокеры, дыры, ошибки источника, неприменённый фильтр, список не сошёлся со сводкой); тогда деньги этой страницы «неизвестно», а не ноль'),
 ('hub_sync','page_label','Страница хаба; * — грант ключа'),
 ('hub_sync','dataset','transactions, payouts, ofapi_payout_requests, page_profile_daily, pending_balance; capabilities — грант ключа'),
 ('hub_sync','status','ok — последняя попытка доказала данные окна; unknown — не доказала'),
 ('hub_sync','reasons','Почему ответ не принят (JSON-массив строк)'),
 ('hub_sync','blockers','Блокеры хаба, которые пришли в принятом ответе и допускаются правилами чтения (delivery_not_exhausted и т. п.)'),
 ('hub_sync','window_from','Начало окна последней попытки, МСК'),
 ('hub_sync','window_to','Конец окна (срез) последней попытки, МСК; при ok данные доказаны за [window_from, window_to)'),
 ('hub_sync','rows','Строк за окно в последнем принятом ответе'),
 ('hub_sync','net_mills','Сумма netMills (transactions) или amountMills (заявки) за окно; у балансов — последнее значение'),
 ('hub_sync','capture_floor','Пол захвата плоскости: с какого момента хаб хранит строки (у транзакций — из сводки), МСК'),
 ('hub_sync','mirror_from','С какого момента зеркало полно (нижняя граница копии), МСК'),
 ('hub_sync','full_at','Последняя полная перекачка окна, МСК; между ними докачивается свежий хвост (hub_tail_days)'),
 ('hub_sync','requests','Запросов к хабу в последней попытке'),
 ('hub_sync','checked_at','Последняя попытка, МСК'),
 ('hub_sync','ok_at','Последняя принятая выгрузка, МСК');

-- Колонки таблиц, которые заполняют `ledger import business` и `ledger import hub-exceptions`.
INSERT OR IGNORE INTO schema_docs(tbl, col, description) VALUES
 ('routes','page_label','Страница хаба'),
 ('routes','model','Модель ledger, чьи деньги идут через страницу'),
 ('routes','channel','Канал вывода: Fansly, OF'),
 ('routes','rate','Ставка маршрута: какая доля net доходит до перевода (десятичная дробь текстом)'),
 ('routes','min_payout','Минимум вывода маршрута, микродоллары (ниже — «застряло»)'),
 ('routes','payee','Кому приходит вывод: model — модели, она переводит агентству; agency — агентству целиком'),
 ('routes','lag_days','Сколько дней от среза перевода до строки журнала'),
 ('route_rates','method','Метод заявки (methodProviderLabel хаба); * — любой метод страницы'),
 ('route_rates','valid_from','С какой заявки (requestedAt, МСК) действует ставка'),
 ('hub_exceptions','kind','row — решение по строке хаба, start — начало страницы (первая строка), floor — пол журнала заявок'),
 ('hub_exceptions','transaction_ref','transactionRef строки хаба (kind row)'),
 ('hub_exceptions','at','row и start — момент строки до секунды (МСК); floor — дата первой заявки, ГГГГ-ММ-ДД как в файле'),
 ('hub_exceptions','net','Net строки, как в файле (0.00); сверяется с netMills ±0.005'),
 ('hub_exceptions','decision','приз — доступно сразу, в оборот; исключить — не оборот; возврат — минус без холда'),
 ('hub_exceptions','basis','Основание решения'),
 ('hub_exceptions','source','Из какого файла импортировано; повторный импорт заменяет строки того же источника'),
 ('hub_exceptions','id','Номер строки'),
 ('hub_exceptions','page_label','Страница хаба'),
 ('hub_exceptions','created_at','Когда импортировано, МСК'),
 ('routes','valid_from','С какой даты маршрут действует; NULL — всегда'),
 ('routes','valid_to','До какой даты маршрут действует; NULL — действует'),
 ('routes','note','Откуда маршрут (источник импорта) или заметка'),
 ('route_rates','id','Номер строки'),
 ('route_rates','page_label','Страница (routes.page_label)'),
 ('route_rates','rate','Ставка маршрута для этого метода с valid_from (десятичная дробь текстом)'),
 ('route_rates','note','Заметка к ставке');
