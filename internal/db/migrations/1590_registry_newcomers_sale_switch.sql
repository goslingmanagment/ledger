-- Ревью экосистемы, круг 1 (пакет R5).
--
-- Новичок из реестра людей хаба: с settings.payroll_terms_hub_from человек хаба с условиями оплаты начисляется по ним
-- без вкладки Google (вкладка «реестр <код>» в книгах), его строка people берётся из реестра (source hub-registry), а
-- автор поста узнаётся по Telegram id и тегу из реестра. Для этого hub_people хранит имя, логины и Telegram человека;
-- курсор ленты людей сбрасывается, чтобы следующий проход джоба hub-terms прочитал всех заново и заполнил новые колонки.
-- Пока настройка пуста, ничего из этого не действует.
--
-- Одно место для «чья продажа»: с settings.sale_decisions_hub_from решения, вопросы и слово владельца о ничьей продаже
-- пишет только хаб; запись в ledger отказывает, списки остаются историей. Пусто — пишет и ledger.
--
-- Миграция только добавляет: колонки (NULL), две строки настроек, описания; курсор ленты людей — пустой.

ALTER TABLE hub_people ADD COLUMN display_name TEXT;
ALTER TABLE hub_people ADD COLUMN logins TEXT;
ALTER TABLE hub_people ADD COLUMN telegram_user_id INTEGER;
ALTER TABLE hub_people ADD COLUMN report_tag TEXT;

UPDATE cursors SET value = '' WHERE name = 'hub:people';

INSERT INTO settings(key, value, note, updated_at) VALUES ('sale_decisions_hub_from', '',
  'С какого дня (МСК) решения «чья продажа», вопросы о ней и слово владельца о ничьей продаже пишет только хаб (hub sale decide|ask|answer); ledger decision add|applied|cancel, question add|answer|drop и accruals claim|unclaim отказывают, списки остаются историей. Пусто — пишет и ledger. Ставит ledger decision hub-from',
  '2026-11-13T00:00:00+03:00')
ON CONFLICT(key) DO NOTHING;

INSERT INTO schema_docs(tbl, col, description) VALUES
 ('hub_people','display_name','Имя человека в реестре хаба'),
 ('hub_people','logins','Логины хаба человека: JSON-массив id пользователей хаба (активных и нет)'),
 ('hub_people','telegram_user_id','Telegram id человека в реестре; с payroll_terms_hub_from по нему узнаётся автор поста «Отчётности»'),
 ('hub_people','report_tag','Тег отчёта человека в реестре (#…); с payroll_terms_hub_from по нему узнаётся автор поста');

UPDATE schema_docs
   SET description = 'Код человека — people.slug. С settings.payroll_terms_hub_from человек с условиями без строки people получает её из реестра (source hub-registry), а без вкладки модели — вкладку «реестр <код>» в книгах'
 WHERE tbl = 'hub_pay_terms' AND col = 'person';
