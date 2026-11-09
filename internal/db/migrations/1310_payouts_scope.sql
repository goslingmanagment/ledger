-- Выпуск 2, L6: право ключа payouts — только GET /v1/payouts (выплаты чаттерам для «Выплат» хаба). Права хранятся
-- текстом, проверка — в коде; здесь только описание для агентов.

UPDATE schema_docs
   SET description = 'Права через запятую: read — SQL и схема, propose — предложения и споры, decide — решения по брифу (только openclaw-main), collect — сборщики, import — разовые импорты, payouts — только GET /v1/payouts (выплаты чаттерам для хаба), больше ничего'
 WHERE tbl = 'keys' AND col = 'scopes';
