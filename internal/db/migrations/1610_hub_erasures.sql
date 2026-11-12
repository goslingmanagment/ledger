-- Стирание фанов хаба в копиях ledger (ревью экосистемы, R5в; хаб — hub#1271). Джоб hub-erasures читает ленту хаба
-- GET /api/v1/agent/erasures (законченные стирания фанов: площадка и ссылка фана, без продаж) по курсору
-- (cursors 'hub:erasures') и стирает слова о фане, которые ledger держит у его продаж (их находит по ссылке фана в
-- hub_sales) и у строк с его ссылкой на страницах площадки; деньги, номера, состояния и кто из команды остаются.
-- hub_erasures — что применено: номер стирания хаба, площадка, когда, сколько строк по таблицам; ссылки фана здесь нет.
-- Миграция только добавляет таблицу.

CREATE TABLE hub_erasures (
  id         INTEGER PRIMARY KEY,
  platform   TEXT NOT NULL,
  erased_at  TEXT NOT NULL,
  applied_at TEXT NOT NULL,
  rows       TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(rows))
);

INSERT INTO schema_docs(tbl, col, description) VALUES
 ('hub_erasures','', 'Стирания фанов хаба, применённые к копиям ledger (джоб hub-erasures, hub#1271): слова о фане у его продаж и строк с его ссылкой стёрты, деньги остались. Ссылки фана здесь нет'),
 ('hub_erasures','id','Номер стирания в хабе'),
 ('hub_erasures','platform','Площадка фана'),
 ('hub_erasures','erased_at','Когда хаб закончил стирание'),
 ('hub_erasures','applied_at','Когда ledger применил его у себя'),
 ('hub_erasures','rows','Сколько строк изменено по таблицам при первом применении (JSON)');
