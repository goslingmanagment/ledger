-- Условия оплаты людей из реестра людей хаба (экосистема, пакет H3). Доля и фикс за смену человека на модели живут в
-- реестре людей хаба («Люди → карточка → График и условия»), каждое условие действует с дня до следующего условия того
-- же человека и модели. Джоб hub-terms раз в 15 минут читает ленту хаба GET /api/v1/agent/people по курсору
-- (cursors 'hub:people' — наибольший updatedAt, с перекрытием) ключом ledger в хабе; условия приходят, только если у
-- ключа есть read:pay_terms, без него ничего не хранится. Человек из ответа хранится целиком (hub_people), его условия
-- тоже (hub_pay_terms); условие, которое хаб больше не отдаёт, помечается gone_at, а не удаляется.
-- Начисления берут условия из реестра только с settings.payroll_terms_hub_from, она пуста: до неё условия считает шапка
-- вкладки, как прежде, а реестр стоит рядом в `ledger payroll terms diff` и проверке. Миграция только добавляет: две
-- новые таблицы и настройку. Деньги — целые микродоллары (mills хаба × 1000), доля — в сотых процента, как в хабе.

CREATE TABLE hub_people (
  hub_id     INTEGER PRIMARY KEY,
  code       TEXT NOT NULL,
  kind       TEXT NOT NULL,
  role       TEXT NOT NULL,
  state      TEXT NOT NULL,
  left_on    TEXT,
  updated_at TEXT NOT NULL,
  stored_at  TEXT NOT NULL
);
CREATE INDEX hub_people_code ON hub_people(code);

CREATE TABLE hub_pay_terms (
  hub_id        INTEGER PRIMARY KEY,
  hub_person_id INTEGER NOT NULL,
  person        TEXT NOT NULL,
  model_slug    TEXT NOT NULL,
  model_name    TEXT NOT NULL DEFAULT '',
  valid_from    TEXT NOT NULL,
  valid_to      TEXT,
  share_bp      INTEGER NOT NULL CHECK (share_bp BETWEEN 0 AND 10000),
  fix_micros    INTEGER NOT NULL CHECK (fix_micros >= 0),
  note          TEXT,
  updated_at    TEXT NOT NULL,
  stored_at     TEXT NOT NULL,
  gone_at       TEXT
);
CREATE INDEX hub_pay_terms_person ON hub_pay_terms(person, model_slug, valid_from);
CREATE INDEX hub_pay_terms_hub_person ON hub_pay_terms(hub_person_id);

INSERT INTO settings(key, value, note, updated_at) VALUES ('payroll_terms_hub_from', '',
  'С какого дня (МСК) начисление дня берёт условия человека (доля, фикс за смену) из реестра людей хаба (hub_pay_terms), действующие в этот день; нет условия на день — шапка вкладки, как прежде, и проверка падает. Пусто — только шапки вкладок. Ставит и снимает ledger payroll terms-from; откат — --off и пересборка',
  '2026-11-12T00:00:00+03:00')
ON CONFLICT(key) DO NOTHING;

INSERT INTO schema_docs(tbl, col, description) VALUES
 ('hub_people','', 'Люди реестра хаба, как их отдаёт лента GET /api/v1/agent/people (H3): код, вид, роль, состояние. Пишет джоб hub-terms; человек из ответа пишется целиком'),
 ('hub_people','hub_id','id человека в реестре хаба'),
 ('hub_people','code','Код человека в хабе — people.slug ledger'),
 ('hub_people','kind','human, agent или service'),
 ('hub_people','role','Роль в реестре: owner, team_lead, chatter, content_manager, contractor, agent, service'),
 ('hub_people','state','invited, set_up, working (работает) или left (ушёл)'),
 ('hub_people','left_on','День ухода (МСК); NULL — работает или день не известен'),
 ('hub_people','updated_at','updatedAt хаба: последнее изменение человека или того, что к нему относится, условия тоже'),
 ('hub_people','stored_at','Когда ledger записал эту версию'),
 ('hub_pay_terms','', 'Условия оплаты людей из реестра хаба (H3): доля и фикс за смену на модели с дня до следующего условия той же модели. Пишет джоб hub-terms, только если у ключа ledger в хабе есть read:pay_terms. Начисления берут их с settings.payroll_terms_hub_from (условие, действующее в день); до неё — шапки вкладок, реестр только в сравнении'),
 ('hub_pay_terms','hub_id','id условия в хабе'),
 ('hub_pay_terms','hub_person_id','id человека в реестре хаба (hub_people.hub_id)'),
 ('hub_pay_terms','person','Код человека — people.slug'),
 ('hub_pay_terms','model_slug','modelSlug хаба; в модель ledger переводит настройка hub_model_slugs'),
 ('hub_pay_terms','model_name','Имя модели в хабе'),
 ('hub_pay_terms','valid_from','Первый день (МСК), с которого условие действует'),
 ('hub_pay_terms','valid_to','Последний день: день до следующего условия той же модели; NULL — действует сейчас'),
 ('hub_pay_terms','share_bp','Доля от денег модели в сотых процента: 2500 — 25 %'),
 ('hub_pay_terms','fix_micros','Фикс за день со сменой, микродоллары; 0 — нет'),
 ('hub_pay_terms','note','Строка владельца к условию'),
 ('hub_pay_terms','updated_at','updatedAt условия в хабе'),
 ('hub_pay_terms','stored_at','Когда ledger записал эту версию'),
 ('hub_pay_terms','gone_at','Когда хаб перестал отдавать условие у человека, которого отдаёт; NULL — отдаёт. Такое условие не действует');
