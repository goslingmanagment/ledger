-- Запросы агентов: ключ ищется по хешу; описания ключей, журнала запросов и служебных таблиц
-- для `ledger schema`. INSERT OR IGNORE: описание из другой ветки с тем же (tbl, col) остаётся.
CREATE UNIQUE INDEX keys_hash ON keys(hash);

UPDATE schema_docs
   SET description = 'Ключи агентов: SHA-256 ключа и права. Сам ключ показывается один раз, при выпуске (ledger keys create на сервере)'
 WHERE tbl = 'keys' AND col = '';

INSERT OR IGNORE INTO schema_docs(tbl, col, description) VALUES
 ('keys','name','Имя агента: bobr, gengar, openclaw-main, claude-mac…; у заменённого ключа к имени добавлено ~<id>'),
 ('keys','prefix','Начало ключа (ldg_<имя>_xxxx), чтобы узнать его в списке; не секрет'),
 ('keys','hash','SHA-256 ключа, hex; по нему API находит ключ'),
 ('keys','scopes','Права через запятую: read — SQL и схема, propose — предложения и споры, decide — решения по брифу (только openclaw-main), collect — сборщики, import — разовые импорты'),
 ('keys','created_at','Когда выпущен, МСК'),
 ('keys','revoked_at','Когда отозван, МСК; NULL — действует'),
 ('keys','last_used_at','Последний запрос с этим ключом, МСК, с точностью до минуты'),
 ('query_log','key_name','Чей ключ (keys.name)'),
 ('query_log','at','Когда пришёл запрос, МСК'),
 ('query_log','sql','Текст запроса как прислан (до 64 КБ)'),
 ('query_log','duration_ms','Сколько выполнялся, мс'),
 ('query_log','rows','Сколько строк вернул'),
 ('query_log','error','Ошибка или отказ; NULL — успех'),
 ('meta','','Служебные значения сервиса: время первого запуска и подобное'),
 ('schema_docs','','Русские описания таблиц и колонок для ledger schema; col = '''' — описание самой таблицы'),
 ('schema_migrations','','Применённые миграции: имя файла и время применения');
