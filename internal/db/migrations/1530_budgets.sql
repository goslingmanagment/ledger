-- Бюджеты для консоли хаба («Деньги → Бюджеты»): планы статей по месяцам. Факт — журнал (v_documents), план оборота
-- страниц — month_plans, лимиты статей — settings.category_caps. Право ключа budgets — GET /v1/budgets и запись планов.
CREATE TABLE budget_plans (
  id          INTEGER PRIMARY KEY,
  month       TEXT NOT NULL CHECK (month GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]'),
  item        TEXT NOT NULL CHECK (length(trim(item)) BETWEEN 1 AND 64),
  plan_micros INTEGER CHECK (plan_micros IS NULL OR plan_micros >= 0),
  note        TEXT NOT NULL DEFAULT '',
  source      TEXT NOT NULL,
  author      TEXT NOT NULL,
  created_at  TEXT NOT NULL
);
CREATE INDEX budget_plans_month ON budget_plans(month, item, id);
CREATE TRIGGER budget_plans_immutable_u BEFORE UPDATE ON budget_plans BEGIN SELECT RAISE(ABORT, 'budget_plans are append-only: add a row'); END;
CREATE TRIGGER budget_plans_immutable_d BEFORE DELETE ON budget_plans BEGIN SELECT RAISE(ABORT, 'budget_plans are append-only: add a row'); END;
CREATE VIEW v_budget_plans AS
  SELECT p.month, p.item, p.plan_micros, p.note, p.source, p.author, p.created_at
  FROM budget_plans p
  WHERE p.id = (SELECT max(x.id) FROM budget_plans x WHERE x.month = p.month AND x.item = p.item)
    AND p.plan_micros IS NOT NULL;

INSERT INTO schema_docs(tbl, col, description) VALUES
 ('budget_plans','', 'План статьи бюджета на месяц, только дописывается: новая строка той же статьи заменяет план, plan_micros NULL снимает его; действующие — v_budget_plans'),
 ('budget_plans','month', 'Месяц ГГГГ-ММ'),
 ('budget_plans','item', 'Статья: категория журнала («Чаттер зарплата», «Трафик», …) или «Доход» — план дохода агентства'),
 ('budget_plans','plan_micros', 'План, микродоллары; NULL — план снят'),
 ('budget_plans','source', 'Откуда план: closes.md (перенос), console (консоль хаба), cli'),
 ('budget_plans','note', 'Заметка к плану: на что и почему'),
 ('budget_plans','author', 'Имя ключа, которым записан план'),
 ('budget_plans','created_at', 'Когда записан план, МСК'),
 ('v_budget_plans','', 'Действующие планы статей по месяцам (последняя строка статьи, снятые не видны)');

UPDATE schema_docs
   SET description = 'Права через запятую: read — SQL и схема, propose — предложения и споры, decide — решения владельца: бриф, правила, споры, заявки на ничьи продажи, изменения за выплаченные дни, общаки, планы бюджета (только openclaw-main), collect — сборщики, import — разовые импорты, payouts — только GET /v1/payouts, GET /v1/payouts/prepare (выплаты чаттерам для хаба), GET /v1/reports (отчёты смен), GET /v1/report-days (база оплаты по отчётам), GET /v1/report-posts без текста постов, GET /v1/decisions и GET /v1/pots, больше ничего, reports — POST /v1/reports (агент команды сохраняет отчёты смен), договорённости (POST /v1/decisions…), покупки общаков (POST /v1/pots/{slug}/sales, POST /v1/pot-sales/{id}/remove) и их чтение, budgets — только GET /v1/budgets (бюджеты по месяцам для консоли хаба: журнал, планы, оборот) и POST /v1/budgets/plans (план статьи по слову владельца из консоли)'
 WHERE tbl = 'keys' AND col = 'scopes';
