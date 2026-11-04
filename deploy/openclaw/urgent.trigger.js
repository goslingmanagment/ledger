// Срочные сбои: новый приходит сразу, повтор того же — раз в 6 часов (учёт на сервере, таблица alerts).
// Пока новых строк нет, модель не запускается. Команда сломалась — молчим: это покажет утренний health.
const res = await exec({ command: "LEDGER_CREDENTIALS=$HOME/.config/ledger/credentials.bobr $HOME/.config/ledger/bin/ledger health --urgent" });
const lines = String(res?.aggregated ?? "").split("\n").filter((l) => l.startsWith("СРОЧНО"));
json({ fire: lines.length > 0, message: lines.join("\n").slice(0, 3000) });
