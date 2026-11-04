-- Деньги в пути: результат покрытия платежей заявками и переводами на последний расчёт. Это производные
-- таблицы: каждый расчёт заменяет их целиком; проводки по ним — обычные (kind 'transit:*'), с reversal.
CREATE TABLE coverage_runs (
  id          INTEGER PRIMARY KEY,
  as_of       TEXT NOT NULL,
  started_at  TEXT NOT NULL,
  finished_at TEXT NOT NULL,
  status      TEXT NOT NULL CHECK (status IN ('ok','unknown')),
  reason      TEXT NOT NULL DEFAULT '',
  notes       TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(notes))
);
CREATE TABLE coverage_pages (
  page_label  TEXT PRIMARY KEY,
  model       TEXT NOT NULL,
  channel     TEXT NOT NULL,
  as_of       TEXT NOT NULL,
  hold_net    REAL NOT NULL DEFAULT 0,
  hold        REAL NOT NULL DEFAULT 0,
  avail_net   REAL NOT NULL DEFAULT 0,
  available   REAL NOT NULL DEFAULT 0,
  stuck       REAL NOT NULL DEFAULT 0,
  riding_net  REAL NOT NULL DEFAULT 0,
  riding      REAL NOT NULL DEFAULT 0,
  min_payout  REAL NOT NULL DEFAULT 0
);
CREATE TABLE coverage_parts (
  page_label   TEXT NOT NULL,
  payment_key  TEXT NOT NULL,
  paid_at      TEXT NOT NULL,
  avail_at     TEXT NOT NULL,
  net          REAL NOT NULL,
  part         REAL NOT NULL,
  state        TEXT NOT NULL CHECK (state IN ('hold','page','stuck','riding','arrived','anchor','pre')),
  request_key  TEXT,
  transfer_num INTEGER,
  rate         REAL NOT NULL,
  est          REAL NOT NULL
);
CREATE INDEX coverage_parts_page ON coverage_parts(page_label, paid_at);
CREATE INDEX coverage_parts_transfer ON coverage_parts(transfer_num);
CREATE TABLE coverage_requests (
  page_label   TEXT NOT NULL,
  key          TEXT NOT NULL,
  requested_at TEXT NOT NULL,
  amount       REAL NOT NULL,
  method       TEXT NOT NULL,
  status       TEXT NOT NULL,
  avail        REAL NOT NULL,
  excess       REAL NOT NULL,
  transfer_num INTEGER,
  pre          INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (page_label, key)
);
CREATE TABLE coverage_transfers (
  num      INTEGER PRIMARY KEY,
  model    TEXT NOT NULL,
  date     TEXT NOT NULL,
  cut      TEXT NOT NULL,
  amount   REAL NOT NULL,
  est      REAL NOT NULL,
  k        REAL,
  held     REAL NOT NULL DEFAULT 0,
  fee      REAL NOT NULL DEFAULT 0,
  deposits TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(deposits)),
  bad      TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(bad)),
  by_month TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(by_month))
);
-- Деньги в пути по страницам: из последнего расчёта; при недоказанных данных хаба — «неизвестно».
CREATE VIEW v_in_transit AS
  SELECT c.page_label AS page, c.model, c.channel,
         ROUND(c.hold_net, 2) AS hold_net, ROUND(c.hold, 2) AS hold,
         ROUND(c.avail_net, 2) AS available_net, ROUND(c.available, 2) AS available,
         ROUND(COALESCE((SELECT SUM(p.amount_mills) / 1000.0 FROM hub_payouts p
                          WHERE p.page_label = c.page_label AND p.status <> 'Processed'
                            AND p.requested_at >= (SELECT substr(as_of, 1, 10) FROM coverage_runs ORDER BY id DESC LIMIT 1)), 0), 2) AS requested_net,
         ROUND(c.riding_net, 2) AS at_model_net, ROUND(c.riding, 2) AS at_model,
         ROUND(c.stuck, 2) AS stuck, c.min_payout,
         ROUND(c.hold + c.available + c.riding + c.stuck, 2) AS total,
         (SELECT status FROM coverage_runs ORDER BY id DESC LIMIT 1) AS status,
         (SELECT reason FROM coverage_runs ORDER BY id DESC LIMIT 1) AS reason,
         c.as_of
  FROM coverage_pages c;
INSERT INTO schema_docs(tbl, col, description) VALUES
 ('coverage_runs','', 'Прогоны расчёта покрытия: срез, статус ok или unknown (хаб не доказал данные) и причина'),
 ('coverage_pages','', 'Страницы на последний срез: в холде, доступно, застряло (ниже минимума вывода), у модели до перевода; *_net — в net страницы, без суффикса — доля агентства по оценке'),
 ('coverage_parts','', 'Части платежей фанов: какая заявка и какой перевод покрыли; state: hold, page, stuck, riding, arrived, anchor, pre'),
 ('coverage_requests','', 'Заявки на вывод страниц: доступно на момент заявки, превышение над доступным net, какой перевод покрыл'),
 ('coverage_transfers','', 'Переводы моделей: оценка покрытых платежей, k = факт ÷ оценка, депозиты из пометки, удержано, комиссия маршрута, разбивка оценки по месяцам платежей'),
 ('v_in_transit','', 'Деньги в пути по страницам. hold / available / at_model / stuck / total — доля агентства по оценке, *_net — net страницы, requested_net — заявки в обработке; status unknown — хаб не доказал данные, цифрам верить нельзя');
