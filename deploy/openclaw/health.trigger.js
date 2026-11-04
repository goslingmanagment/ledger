// Утренний health: агент (и модель) запускается, только если есть что сказать. Проверки идут на сервере каждый час.
// С --quiet здоровый ledger не печатает ничего; любой вывод — сбой или ответ сломанной команды, агент передаст его владельцу.
const res = await exec({ command: "LEDGER_CREDENTIALS=$HOME/.config/ledger/credentials.bobr $HOME/.config/ledger/bin/ledger health --quiet" });
const out = String(res?.aggregated ?? "").trim();
json({ fire: out.length > 0, message: `Вывод ledger health --quiet:\n${out.slice(0, 3000)}` });
