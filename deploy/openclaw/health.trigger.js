// Утренний health: агент (и модель) запускается, только если есть что сказать. Проверки идут на сервере каждый час.
// С --quiet здоровый ledger не печатает ничего; любой вывод — сбой или ответ сломанной команды, агент передаст его владельцу.
// Заодно: отметка ночного брифа отправленным по квитанции доставки (сломалось — это увидит следующий вывод).
const res = await exec({ command: "export LEDGER_CREDENTIALS=$HOME/.config/ledger/credentials.bobr; python3 $HOME/code/2-finance/ledger/deploy/openclaw/brief-receipts.py; $HOME/.config/ledger/bin/ledger health --quiet" });
const out = String(res?.aggregated ?? "").trim();
json({ fire: out.length > 0, message: `Вывод ledger health --quiet:\n${out.slice(0, 3000)}` });
