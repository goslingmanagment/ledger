-- Выпуск 2, L1: правила дележа продаж хаба. Логика — пакет internal/split; строки — какие правила действуют, с какого
-- момента, по какому решению владельца (docs/plans/2026-11-09-release-2.md, «Правила дележа»). params повторяют
-- split.Default: правка правила — новая строка с valid_from и закрытие старой valid_to, вместе с кодом.
INSERT INTO split_rules(kind, participants, params, valid_from, decision_ref, created_at) VALUES
 ('ppv_sender', '[]', '{"roles":["chatter"],"wait_days":7}', '2026-11-02T00:00:00+03:00',
  'Владелец 2026-11-08, «Правила дележа» п. 1: купленный PPV — отправителю (долёт)',
  strftime('%Y-%m-%dT%H:%M:%S+03:00', 'now', '+3 hours')),
 ('pot', '[]', '{"pot":"obshak","sources":["post_tip","campaign"]}', '2026-11-02T00:00:00+03:00',
  'Владелец 2026-11-08, «Правила дележа» п. 2: чаевые под постом и Campaign OnlyFans — в общак',
  strftime('%Y-%m-%dT%H:%M:%S+03:00', 'now', '+3 hours')),
 ('holder', '[]', '{"several":"last_writer","last_writer_hours":48,"none_last":"decide"}', '2026-11-02T00:00:00+03:00',
  'Владелец 2026-11-08, «Правила дележа» п. 3: остальное — смене, державшей страницу',
  strftime('%Y-%m-%dT%H:%M:%S+03:00', 'now', '+3 hours')),
 ('decide', '[]', '{"by":["owner_split","marks"]}', '2026-11-02T00:00:00+03:00',
  'Владелец 2026-11-09, вопрос 1: две смены, последним писал никто — решение владельца или согласные метки чаттеров',
  strftime('%Y-%m-%dT%H:%M:%S+03:00', 'now', '+3 hours'));
UPDATE schema_docs SET description = 'Правила дележа продаж хаба между сменами, общаком и «ничей» (пакет internal/split), с датами действия и ссылкой на решение владельца. Продажи раньше первого valid_from и смены дня до него — по-старому, их платят таблицы выплат'
 WHERE tbl = 'split_rules' AND col = '';
INSERT INTO schema_docs(tbl, col, description) VALUES
 ('split_rules', 'kind', 'ppv_sender — купленный PPV отправителю; pot — в общак; holder — смене, державшей страницу; decide — спорная продажа по решению владельца или меткам'),
 ('split_rules', 'participants', 'Кого касается, JSON-список; [] — всех'),
 ('split_rules', 'params', 'Параметры правила JSON (wait_days — сколько PPV ждёт смены отправителя; pot — какой общак)'),
 ('split_rules', 'valid_from', 'С какого момента действует, МСК'),
 ('split_rules', 'valid_to', 'До какого момента, МСК; NULL — действует'),
 ('split_rules', 'decision_ref', 'Решение владельца, на котором правило стоит'),
 ('split_rules', 'created_at', 'Когда строка записана, МСК');
