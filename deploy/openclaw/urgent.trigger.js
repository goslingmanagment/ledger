// Срочные сбои: новый приходит сразу, повтор того же — раз в 6 часов (учёт на сервере, таблица alerts).
// Вызов отмечает сигнал отправленным, поэтому нужен ключ с правом decide: чтение чужими ключами его не гасит.
// Пока новых строк нет, модель не запускается.
//
// У проверки условия 30 секунд, и ровно столько же ledger ждёт соединения с сервером, до которого не достучаться:
// шлюз убивал условие по таймауту, а владелец получал сырое «failed … timeout». Поэтому у команды свои 20 секунд
// (сторож на perl снимает её группу процессов SIGKILL: Go не реагирует на SIGALRM), а её сбой — исход проверки,
// не падение условия.
// Разовый сбой молчит: сеть моргнула, через 15 минут следующая проверка. Три подряд — срочное не проверяется
// полчаса: одно сообщение «ПРОВЕРКА НЕ РАБОТАЕТ», агент отправит его под шапкой «⚠️ Поломка · ledger»; пока не
// починится, повтор раз в 6 часов. Счёт сбоев — в состоянии условия (trigger.state).
const LIMIT_S = 20;
const TELL_AFTER = 3;
const REPEAT_MS = 6 * 3600 * 1000;
const guard = `/usr/bin/perl -e '$l = shift; $p = fork; unless ($p) { setpgrp(0, 0); exec @ARGV } $SIG{ALRM} = sub { kill "KILL", -$p }; alarm $l; waitpid $p, 0; exit($? & 127 ? 124 : $? >> 8)'`;
const command = `LEDGER_CREDENTIALS=$HOME/.config/ledger/credentials.decide ${guard} ${LIMIT_S} $HOME/.config/ledger/bin/ledger health --urgent 2>&1; echo "ledger-exit=$?"`;

const prev = trigger.state ?? {};
const started = Date.now();
let out = "";
try {
  const res = await exec({ command });
  out = String(res?.aggregated ?? "");
} catch (e) {
  out = `exec: ${String(e?.message ?? e)}`;
}
const ms = Date.now() - started;
const now = new Date().toISOString();
const code = out.match(/^ledger-exit=(\d+)$/m)?.[1];

if (code === "0") {
  const lines = out.split("\n").filter((l) => l.startsWith("СРОЧНО"));
  json({ fire: lines.length > 0, message: lines.join("\n").slice(0, 3000), state: { fails: 0, ms, at: now } });
} else {
  const fails = (prev.fails ?? 0) + 1;
  const since = prev.fails ? prev.since ?? now : now;
  const why =
    code === "124"
      ? `ledger не ответил за ${LIMIT_S} с`
      : out.split("\n").filter((l) => l.trim() && !l.startsWith("ledger-exit=")).slice(-1)[0]?.trim().slice(0, 300) ||
        `команда завершилась с кодом ${code ?? "?"}`;
  const tell = fails >= TELL_AFTER && (!prev.told || Date.parse(now) - Date.parse(prev.told) >= REPEAT_MS);
  const msk = new Date(Date.parse(since) + 3 * 3600 * 1000); // МСК — UTC+3 без перевода часов
  const two = (n) => String(n).padStart(2, "0");
  const hhmm = `${two(msk.getUTCDate())}.${two(msk.getUTCMonth() + 1)} ${two(msk.getUTCHours())}:${two(msk.getUTCMinutes())}`;
  json({
    fire: tell,
    message: tell
      ? `ПРОВЕРКА НЕ РАБОТАЕТ: срочные сбои ledger не проверяются с ${hhmm} МСК, проверок подряд без ответа: ${fails}.\nПричина: ${why}`
      : "",
    state: { fails, since, told: tell ? now : prev.fails ? prev.told ?? null : null, ms, at: now, why },
  });
}
