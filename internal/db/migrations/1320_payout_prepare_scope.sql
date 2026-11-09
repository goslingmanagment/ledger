-- Выпуск 2, L5: ключ payouts открывает ещё GET /v1/payouts/prepare (бриф выплаты одному человеку). Права хранятся
-- текстом, проверка — в коде; здесь только описание для агентов.

UPDATE schema_docs
   SET description = 'Права через запятую: read — SQL и схема, propose — предложения и споры, decide — решения по брифу (только openclaw-main), collect — сборщики, import — разовые импорты, payouts — только GET /v1/payouts и GET /v1/payouts/prepare (выплаты чаттерам для хаба), больше ничего'
 WHERE tbl = 'keys' AND col = 'scopes';
