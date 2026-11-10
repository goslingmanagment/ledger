-- Отчёты смен, этап R2a (план docs/plans/2026-11-10-shift-reports.md): сборщик на маке (ledger collect reports) читает
-- топики «Отчётность» через tg history и шлёт посты событиями tg_reports; сервер разбирает их сюда и сводит с черновиками
-- агента по страницам (v_report_pages). Пост чаттера — подтверждение отчёта. Ничего здесь не проводится и не начисляется.
-- Деньги — целые микродоллары, времена — МСК.

CREATE TABLE shift_report_posts (
  id           INTEGER PRIMARY KEY,
  event_id     INTEGER NOT NULL REFERENCES events(id),
  peer         TEXT NOT NULL,
  topic        INTEGER NOT NULL,
  msg_id       INTEGER NOT NULL,
  posted_at    TEXT NOT NULL,
  sender_id    TEXT NOT NULL DEFAULT '',
  sender       TEXT NOT NULL DEFAULT '',
  tag          TEXT NOT NULL DEFAULT '',
  person       TEXT,
  model        TEXT NOT NULL DEFAULT '',
  report_date  TEXT,
  total_micros INTEGER NOT NULL,
  pages_micros INTEGER NOT NULL,
  flags        TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(flags)),
  text         TEXT NOT NULL,
  current      INTEGER NOT NULL CHECK (current IN (0,1)),
  digest       TEXT NOT NULL,
  parsed_at    TEXT NOT NULL,
  UNIQUE (peer, topic, msg_id, event_id)
);
CREATE UNIQUE INDEX shift_report_posts_event ON shift_report_posts(event_id);
CREATE INDEX shift_report_posts_day ON shift_report_posts(person, report_date);

CREATE TABLE shift_report_post_pages (
  post_id       INTEGER NOT NULL REFERENCES shift_report_posts(id),
  seq           INTEGER NOT NULL,
  name          TEXT NOT NULL,
  page_label    TEXT,
  in_shift      INTEGER NOT NULL CHECK (in_shift IN (0,1)),
  amount_micros INTEGER NOT NULL,
  PRIMARY KEY (post_id, seq)
);
CREATE INDEX shift_report_post_pages_label ON shift_report_post_pages(page_label);

-- База оплаты: человек × дата отчёта × страница. Пост — действующая версия последнего (по posted_at) поста человека за
-- дату, где страница в смене и у поста нет блокирующей пометки; черновик — действующая версия агента.
CREATE VIEW v_report_pages AS
WITH posted AS (
  SELECT p.person, p.report_date, pp.page_label, p.id AS post_id, p.posted_at, p.model AS post_model,
         SUM(pp.amount_micros) AS amount
    FROM shift_report_posts p JOIN shift_report_post_pages pp ON pp.post_id = p.id
   WHERE p.current = 1 AND p.person IS NOT NULL AND p.report_date IS NOT NULL
     AND pp.page_label IS NOT NULL AND pp.in_shift = 1
     AND NOT EXISTS (SELECT 1 FROM json_each(p.flags) f
                      WHERE f.value IN ('unknown_author','no_date','sum_mismatch','date_mismatch'))
   GROUP BY p.id, pp.page_label
),
latest AS (
  SELECT posted.*, ROW_NUMBER() OVER (PARTITION BY person, report_date, page_label
                                      ORDER BY posted_at DESC, post_id DESC) AS rn
    FROM posted
),
report AS (
  SELECT r.id, r.person, r.report_date,
         ROW_NUMBER() OVER (PARTITION BY r.person, r.report_date ORDER BY r.created_at DESC, r.id DESC) AS rn
    FROM shift_reports r
   WHERE r.source = 'agent' AND r.status <> 'superseded' AND r.person IS NOT NULL
),
draft AS (
  SELECT d.person, d.report_date, sp.page_label, d.id AS report_id, sp.amount_micros AS amount
    FROM report d JOIN shift_report_pages sp ON sp.report_id = d.id
   WHERE d.rn = 1
),
day_keys AS (
  SELECT person, report_date, page_label FROM latest WHERE rn = 1
  UNION
  SELECT person, report_date, page_label FROM draft
)
SELECT k.person AS person,
       k.report_date AS report_date,
       k.page_label AS page,
       COALESCE((SELECT COALESCE((SELECT j.value FROM settings s, json_each(s.value) j
                                   WHERE s.key = 'hub_model_slugs' AND json_valid(s.value) AND j.key = hp.model_slug),
                                 hp.model_slug)
                   FROM hub_pages hp WHERE hp.page_label = k.page_label), l.post_model, '') AS model,
       l.amount AS posted_micros,
       d.amount AS draft_micros,
       CASE WHEN l.amount IS NOT NULL AND d.amount IS NOT NULL THEN l.amount - d.amount END AS diff_micros,
       CASE WHEN l.amount IS NULL THEN 'draft_only'
            WHEN d.amount IS NULL THEN 'post_only'
            WHEN l.amount = d.amount THEN 'posted_equal'
            ELSE 'posted_changed' END AS state,
       l.posted_at AS posted_at,
       d.report_id AS report_id,
       l.post_id AS post_id
  FROM day_keys k
  LEFT JOIN latest l ON l.rn = 1 AND l.person = k.person AND l.report_date = k.report_date AND l.page_label = k.page_label
  LEFT JOIN draft d ON d.person = k.person AND d.report_date = k.report_date AND d.page_label = k.page_label;

INSERT INTO schema_docs(tbl, col, description) VALUES
 ('shift_report_posts','', 'Посты чаттеров в топиках «Отчётность» (R2a): событие tg_reports сборщика на маке, разобранное сервером. Правка сообщения — новое событие и новая версия; действующая — current = 1. Разбор повторяется джобом, когда меняются настройки report_topics и report_authors. Ничего не проводит и не начисляет'),
 ('shift_report_posts','id','Номер разобранной версии поста'),
 ('shift_report_posts','event_id','Событие, из которого разобрано (events.id, source tg_reports)'),
 ('shift_report_posts','peer','Чат Telegram (id группы)'),
 ('shift_report_posts','topic','Топик группы (id раздела)'),
 ('shift_report_posts','msg_id','Номер сообщения в чате'),
 ('shift_report_posts','posted_at','Когда сообщение отправлено, МСК'),
 ('shift_report_posts','sender_id','id отправителя в Telegram'),
 ('shift_report_posts','sender','Имя отправителя в Telegram'),
 ('shift_report_posts','tag','Тег чаттера из строки «Чаттер», как написан (#тег); пусто — нет'),
 ('shift_report_posts','person','Человек (people.slug) по settings.report_authors: сначала tg:<id отправителя>, потом #тег; NULL — автор не узнан'),
 ('shift_report_posts','model','Модель топика (settings.report_topics); пусто — топика нет в настройке'),
 ('shift_report_posts','report_date','Дата отчёта ГГГГ-ММ-ДД из строки «Дата»; без года — ближайший к posted_at год; NULL — дату не прочитать'),
 ('shift_report_posts','total_micros','«Общий доход» поста, микродоллары'),
 ('shift_report_posts','pages_micros','Сумма строк аккаунтов в смене, микродоллары; не равна total_micros — пометка sum_mismatch'),
 ('shift_report_posts','flags','JSON-массив пометок: unknown_author — автор не узнан; tag_mismatch — тег и отправитель указывают на разных людей (верен отправитель); unknown_page — имя страницы не сопоставлено; sum_mismatch — строки не сходятся с итогом; date_mismatch — дата отчёта дальше суток от дня отправки; no_date — дату не прочитать; duplicate — тот же человек и дата с той же страницей в другом сообщении. Блокируют пост в базе оплаты: unknown_author, no_date, sum_mismatch, date_mismatch; unknown_page — только саму страницу'),
 ('shift_report_posts','text','Текст сообщения, как прочитан'),
 ('shift_report_posts','current','1 — последняя версия сообщения; 0 — заменена правкой'),
 ('shift_report_posts','digest','Отпечаток разбора: повторный разбор с тем же итогом ничего не пишет'),
 ('shift_report_posts','parsed_at','Когда разобрано последний раз, МСК'),
 ('shift_report_post_pages','', 'Строки аккаунтов поста: имя, как написано, и страница хаба по settings.report_topics или по имени страницы хаба'),
 ('shift_report_post_pages','post_id','Версия поста (shift_report_posts.id)'),
 ('shift_report_post_pages','seq','Порядок строки в посте'),
 ('shift_report_post_pages','name','Имя страницы, как написано в посте'),
 ('shift_report_post_pages','page_label','Страница хаба (pageLabel); NULL — имя не сопоставлено'),
 ('shift_report_post_pages','in_shift','0 — «не в смене», 1 — с суммой'),
 ('shift_report_post_pages','amount_micros','Сумма страницы в посте, микродоллары; 0 у «не в смене»'),
 ('v_report_pages','', 'База оплаты по отчётам: человек × дата отчёта × страница, где есть сумма поста или черновика агента. Пост — действующая версия последнего поста человека за дату со страницей в смене и без блокирующих пометок; черновик — действующая версия агента (source agent) с человеком. Ничего не начисляет'),
 ('v_report_pages','person','Человек (people.slug)'),
 ('v_report_pages','report_date','Дата отчёта ГГГГ-ММ-ДД'),
 ('v_report_pages','page','Страница хаба (pageLabel)'),
 ('v_report_pages','model','Модель страницы: hub_pages и hub_model_slugs, иначе модель топика поста'),
 ('v_report_pages','posted_micros','Сумма в посте чаттера, микродоллары; NULL — не опубликована'),
 ('v_report_pages','draft_micros','Сумма в черновике агента, микродоллары; NULL — черновика нет'),
 ('v_report_pages','diff_micros','Пост минус черновик, микродоллары, когда есть оба'),
 ('v_report_pages','state','posted_equal — пост равен черновику; posted_changed — отличается (diff_micros); post_only — пост без черновика; draft_only — черновик без поста'),
 ('v_report_pages','posted_at','Когда отправлен пост, МСК'),
 ('v_report_pages','report_id','Версия черновика (shift_reports.id)'),
 ('v_report_pages','post_id','Версия поста (shift_report_posts.id)');

UPDATE schema_docs
   SET description = 'Параметры учёта (доля агентства, холд, нормы k и т. п.). Отчёты смен: report_topics — JSON-список топиков «Отчётность» [{"peer": "<чат>", "topic": <id>, "model": "<модель>", "pages": {"<имя в посте строчными>": "<страница хаба>"}}]; report_authors — JSON-объект {"tg:<id отправителя>": "<человек>", "#<тег строчными>": "<человек>"}. Нет ключа — ничего не настроено'
 WHERE tbl = 'settings' AND col = '';

UPDATE schema_docs
   SET description = 'Права через запятую: read — SQL и схема, propose — предложения и споры, decide — решения владельца: бриф, правила, споры, заявки на ничьи продажи (только openclaw-main), collect — сборщики, import — разовые импорты, payouts — только GET /v1/payouts, GET /v1/payouts/prepare (выплаты чаттерам для хаба), GET /v1/reports (отчёты смен) и GET /v1/report-days (база оплаты по отчётам), больше ничего, reports — только POST /v1/reports (агент команды сохраняет отчёты смен)'
 WHERE tbl = 'keys' AND col = 'scopes';
