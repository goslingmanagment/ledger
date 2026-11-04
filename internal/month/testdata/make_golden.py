#!/usr/bin/env python3
"""Generates golden.json: a synthetic month of pages, requests and transfers, plus the numbers the original
monthly card script computes for it. Run once against a copy of that script:
    PYTHONDONTWRITEBYTECODE=1 OS_MONTH_CACHE=/tmp/x python3 make_golden.py /path/to/scripts golden.json
Everything here is made up with a fixed seed; the script's own business constants are patched to these."""
import json, math, os, random, sys
from datetime import datetime, timedelta, timezone, date

scripts, out = sys.argv[1], sys.argv[2]
sys.path.insert(0, scripts)
import month as M

MSK = M.MSK
rng = random.Random(7)
ROUTES = {"alfa-1": ("Alfa", "Fansly", .962, 0, "model"), "alfa-2": ("Alfa", "Fansly", .962, 0, "model"),
          "alfa-of": ("Alfa", "OF", .958, 0, "model"), "beta-1": ("Beta", "Fansly", .962, 0, "model"),
          "gamma-1": ("Gamma", "Fansly", .908, 500, "agency")}
M.ROUTES = ROUTES
M.MODELS = ["Alfa", "Beta", "Gamma"]
M.MODEL_ROUTE = {v[0]: v[4] for v in ROUTES.values()}
M.NAME = {"Alfa": "Альфа", "Beta": "Бета", "Gamma": "Гамма"}
M.TAB_ALIAS = {"Вася": "Василий"}
START_BETA = datetime(2026, 8, 22, 20, 45, 28, tzinfo=timezone.utc)
AS_OF = datetime(2026, 11, 2, 12, 0, 0, tzinfo=MSK)
HOLD = M.HOLD
M._EXC = dict(rows={}, starts={"beta-1": START_BETA}, floor={})

payments, payouts, of_payouts = {p: [] for p in ROUTES}, {p: [] for p in ROUTES}, {p: [] for p in ROUTES}
key = 0
def k():
    global key
    key += 1
    return f"k{key:06d}"

def gen_day(page, d0):
    n = rng.randint(0, 4)
    for _ in range(n):
        t = d0 + timedelta(seconds=rng.randint(0, 86399))
        if page == "beta-1" and t < START_BETA:
            continue
        net = rng.randint(1000, 90000) / 1000
        row = dict(page=page, t=t, net=net, key=k(), prize=False, avail=t + HOLD)
        payments[page].append(row)
    if page == "alfa-1" and rng.random() < .01:
        t = d0 + timedelta(seconds=rng.randint(0, 86399))
        payments[page].append(dict(page=page, t=t, net=100.0, key=k(), prize=True, avail=t))
    if page == "alfa-2" and rng.random() < .01:
        t = d0 + timedelta(seconds=rng.randint(0, 86399))
        payments[page].append(dict(page=page, t=t, net=-rng.randint(5000, 20000) / 1000, key=k(), prize=False, avail=t))

d = datetime(2026, 5, 1, tzinfo=MSK)
while d < AS_OF:
    for p in ROUTES:
        gen_day(p, d)
    d += timedelta(days=1)
for p in ROUTES:
    payments[p] = [r for r in payments[p] if r["t"] < AS_OF]
    payments[p].sort(key=lambda r: (r["t"], r["key"]))
payments["beta-1"].insert(0, dict(page="beta-1", t=START_BETA, net=11.111, key=k(), prize=False, avail=START_BETA + HOLD))

def available(page, t, taken):
    return sum(r["net"] for r in payments[page] if (r["avail"] < t)) - taken

sessions = {}
mon = datetime(2026, 6, 1, tzinfo=MSK)
taken = {p: 0.0 for p in ROUTES}
while mon < AS_OF:
    base = mon + timedelta(hours=rng.randint(9, 20), minutes=rng.randint(0, 59))
    for i, p in enumerate(ROUTES):
        t = base + timedelta(minutes=i * 3, seconds=rng.randint(0, 59))
        if t > AS_OF or (p == "beta-1" and t < START_BETA + timedelta(days=8)):
            continue
        av = available(p, t, taken[p])
        minimum = ROUTES[p][3]
        if av < max(minimum, 50):
            continue
        amount = float(int(av)) if rng.random() > .08 else float(int(av * .6))
        taken[p] += amount
        if ROUTES[p][1] == "OF":
            st = "approved" if t < AS_OF - timedelta(days=2) else "new"
            of_payouts[p].append(dict(page=p, t=t, amount=amount, status=st, method="of"))
        else:
            method = "unmapped:5" if p == "gamma-1" else ("paxum" if p == "alfa-2" and rng.random() < .12 else "usdt")
            upd = t + timedelta(days=1)
            of = dict(page=p, t=t, amount=amount, status="Processed", method=method, updated=upd)
            payouts[p].append(of)
        sessions.setdefault(mon, []).append((p, t, amount))
    mon += timedelta(days=7)

ledger = []
def row(dt, typ, cat, who, model, amount, method, note, status="paid", cur="USDT"):
    ledger.append([len(ledger) + 1, dt, typ, cat, who, model, f"{amount:.2f}", cur, method, status, note])

def rate(p, q_t, method):
    return M.rate_of(p, dict(method=method, t=q_t))

for mon in sorted(sessions):
    rows = sessions[mon]
    for model in M.MODELS:
        mine = [(p, t, a) for p, t, a in rows if ROUTES[p][0] == model]
        if not mine:
            continue
        last = max(t for _, t, _ in mine)
        if last > AS_OF - timedelta(days=3):
            continue  # no transfer yet: stays «едет»
        if model == "Gamma" and last.day % 2:
            continue
        dep = []
        for p, t, a in mine:
            method = next((q["method"] for q in payouts[p] if q["t"] == t), "of")
            dep.append(round(a * rate(p, t, method), 2))
        total = sum(dep)
        noise = rng.uniform(.994, 1.006)
        cut = last.astimezone(MSK).strftime("%Y-%m-%d %H:%M")
        doc = last.astimezone(MSK).date() + timedelta(days=rng.randint(0, 1))
        if last < datetime(2026, 8, 1, tzinfo=MSK) and last < datetime(2026, 7, 27, tzinfo=MSK):
            continue  # untagged old era
        if model == "Gamma":
            gross = round(total * noise, 2)
            fee = round(gross * .09, 2)
            note = f"выплата обменника {{перевод: срез {cut}; депозиты: {gross:.2f}; комиссия: {fee:.2f}}}"
            row(str(doc), "income", "Платформы", "Обменник", model, gross, "Internal Transfer (Bybit)", note)
            row(str(doc), "expense", "Комиссии", "Обменник", model, fee, "Удержано из вывода", "комиссия обменника")
        else:
            amount = float(round(total * .7 * noise))
            held = float(rng.randint(10, 60)) if rng.random() < .3 else 0
            tag = f"срез {cut}; депозиты: {'+'.join(f'{x:.2f}' for x in dep)}" + (f"; удержано: {held:.2f}" if held else "")
            row(str(doc), "income", "Платформы", "Модель " + model, model, amount, "Internal Transfer (Bybit)", f"перевод {{перевод: {tag}}}")
            if held:
                row(str(doc), "expense", "Прочее", "Модель " + model, model, held, "Удержано из вывода", "покупка для съёмки")
ledger.sort(key=lambda r: r[1])
for i, r in enumerate(ledger, 1):
    r[0] = i

# Expenses of every kind the card knows.
def ex(dt, cat, who, model, amount, method, note, typ="expense"):
    ledger.append([len(ledger) + 1, dt, typ, cat, who, model, f"{amount:.2f}", "USDT", method, "paid", note])
ex("2026-09-02", "Инструменты", "Сервис А", "—", 217.00, "USDT TRC-20", "на три месяца {предоплата 2026-09..2026-11}")
ex("2026-09-05", "Комиссии", "Bybit", "—", 7.00, "USDT TRC-20", "сеть за август {за 2026-08: 6} {за 2026-09: 1}")
ex("2026-09-07", "AI и разработка", "Сервис Б", "—", 110.25, "Карта CinCin 1234", "подписка")
ex("2026-09-10", "Подрядчик", "Подрядчик В", "Alfa", 150.00, "USDT TRC-20", "работа")
ex("2026-09-11", "Чаттер зарплата", "Пётр", "Alfa", 300.00, "Internal Transfer (Bybit)", "выплата")
ex("2026-09-12", "Чаттер зарплата", "Вася", "Alfa", 121.00, "Internal Transfer (Bybit)", "выплата и бонус {сверх таблицы: 21}")
ex("2026-09-13", "Чаттер зарплата", "Пётр", "Beta", 40.00, "Internal Transfer (Bybit)", "округление {сверх таблицы: 0.15; период 2026-08}")
ex("2026-09-15", "Доля модели", "Модель Гамма", "Gamma", 300.00, "Internal Transfer (Bybit)", "30% {внутри доли}")
ex("2026-09-16", "Доля модели", "Модель Гамма", "Gamma", 50.50, "Internal Transfer (Bybit)", "разовая доплата {сверх доли}")
ex("2026-09-17", "Прочее", "Модель Альфа", "Alfa", 135.00, "Удержано из вывода", "доля за кастом {внутри доли}")
ex("2026-09-18", "Owner зарплата", "Владелец", "—", 1000.00, "P2P (Bybit)", "зарплата")
ex("2026-09-19", "Owner draw", "Владелец", "—", 500.00, "P2P (Bybit)", "личное")
ex("2026-09-20", "Трафик", "Канал", "Alfa", 290.00, "USDT TRC-20", "закупка")
ex("2026-10-06", "Инструменты", "Сервис Г", "—", 95.95, "BTC (Bybit)", "за 3 месяца {предоплата 2026-10..2026-12}")
ex("2026-10-08", "КМ зарплата", "КМ", "Alfa", 579.64, "P2P (Bybit)", "выплата")
ex("2026-09-21", "Возврат расхода", "Сервис", "—", 49.50, "Карта CinCin 1234", "возврат", typ="income")
ex("2026-09-22", "Возврат расхода", "Подрядчик", "—", 94.00, "USDT TRC-20", "старый {до 01.08}", typ="income")
ex("2026-09-23", "Custom", "Фан", "Alfa", 250.00, "USDT TRC-20", "аванс {сделка: d1; цена 450; доля 135}", typ="income")
ex("2026-10-02", "Custom", "Фан", "Alfa", 200.00, "USDT TRC-20", "остаток {сделка: d1; цена 450; доля 135}", typ="income")
ex("2026-10-03", "Custom", "Другой фан", "Beta", 100.00, "USDT TRC-20", "крипта {доля 0}", typ="income")
ex("2026-10-04", "Рефералы", "Старая страница", "Beta", 37.13, "Internal Transfer (Bybit)", "реферал {доля 0}", typ="income")
ledger.sort(key=lambda r: (r[1], r[0]))

parsed = []
for r in ledger:
    parsed.append([r[0], r[1], r[1][:7], r[2], r[3], r[4], r[5], float(r[6]), r[7], r[8], r[9], r[10]])

tabs = {}
def tab(model, name, start, end, step):
    days, worked = {}, set()
    d = start
    while d <= end:
        if rng.random() > .2:
            v = round(rng.uniform(5, 90), 3) + (rng.random() < .3) * 8
            days[d] = v
            worked.add(d)
        d += timedelta(days=step)
    tabs[(model, name)] = dict(days=days, worked=worked, notes=[f"{model}/{name}: пометка"] if rng.random() < .5 else [])
tab("Alfa", "Пётр", date(2026, 8, 1), date(2026, 11, 1), 1)
tab("Alfa", "Василий", date(2026, 8, 15), date(2026, 10, 25), 2)
tab("Beta", "Пётр", date(2026, 8, 25), date(2026, 11, 1), 1)
tab("Gamma", "Глеб", date(2026, 8, 1), date(2026, 10, 30), 3)

class FakeHub:
    calls, notes, floors = 0, [], {}
    def pages(self):
        return [("alfa-1", "Alfa"), ("alfa-2", "Alfa"), ("alfa-of", "Alfa"), ("beta-1", "Beta"), ("gamma-1", "Gamma"), ("delta-1", "Delta")]
    def payouts(self, page, a, b):
        return [dict(q) for q in payouts[page] if a <= q["t"] < b]
    def of_payouts(self, page, a, b):
        return [dict(q) for q in of_payouts[page] if a <= q["t"] < b]
    def rows(self, page, a, b):
        return sorted((dict(r) for r in payments[page] if a <= r["t"] < b), key=lambda r: (r["t"], r["key"]))
    def summary(self, page, a, b):
        return 3, 123456

hub = FakeHub()
M.bybit = lambda sql, args=(): []
C = M.cover(AS_OF, parsed, hub)
expected = {}
for mo in ["2026-09", "2026-10"]:
    R = M.card(C, mo, tabs, hub)
    tot = M.totals(R)
    expected[mo] = dict(totals={k: round(v, 2) for k, v in tot.items()},
        lines=[dict(model=m, channel=c, **{k: round(x, 2) for k, x in v.items()}) for (m, c), v in R["lines"].items()],
        expenses=dict(chat={k: round(v, 6) for k, v in R["exp"]["chat"].items()}, extra=R["exp"]["extra"],
                      cats={k: round(v, 6) for k, v in R["exp"]["cats"].items()}, total=round(R["exp"]["total"], 2)),
        transfers=[dict(n=t["n"], model=m, cut=M.iso(t["cut"]), amount=t["amount"], est=round(t["est"], 2), k=round(t["k"], 6),
                        month=round(t["by_month"].get(mo, 0.0) * t["k"], 2), bad=t["bad"])
                   for m, chain in R["transfers"].items() for t in chain if "k" in t],
        requests=[dict(page=q["page"], t=M.iso(q["t"]), amount=q["amount"], avail=round(q["avail"], 3),
                       transfer=q["transfer"]["n"] if isinstance(q["transfer"], dict) else q["transfer"]) for q in R["reqs"]])

iso = lambda t: t.astimezone(timezone.utc).isoformat()
out_doc = dict(as_of=iso(AS_OF), start_beta=iso(START_BETA),
    routes=[dict(page=p, model=v[0], channel=v[1], rate=v[2], min=v[3], payee=v[4]) for p, v in ROUTES.items()],
    payments={p: [dict(key=r["key"], t=iso(r["t"]), net_mills=round(r["net"] * 1000), prize=r["prize"], avail=iso(r["avail"])) for r in rows]
              for p, rows in payments.items()},
    payouts={p: [dict(key=f"q{i}", t=iso(q["t"]), amount_mills=round(q["amount"] * 1000), status=q["status"], method=q["method"], updated=iso(q["updated"]))
                 for i, q in enumerate(rows)] for p, rows in payouts.items()},
    of_payouts={p: [dict(key=f"o{i}", t=iso(q["t"]), amount_mills=round(q["amount"] * 1000), status=q["status"]) for i, q in enumerate(rows)]
                for p, rows in of_payouts.items()},
    ledger=[[str(r[0]), r[1], r[2], r[3], r[4], r[5], r[6], r[7], r[8], r[9], r[10]] for r in ledger],
    tabs=[dict(model=m, name=n, days={d.isoformat(): v for d, v in t["days"].items()}, worked=sorted(d.isoformat() for d in t["worked"]), notes=t["notes"])
          for (m, n), t in tabs.items()],
    expected=expected)
json.dump(out_doc, open(out, "w"), ensure_ascii=False, indent=None)
print("rows", sum(len(v) for v in payments.values()), "requests", sum(len(v) for v in payouts.values()) + sum(len(v) for v in of_payouts.values()),
      "ledger", len(ledger), "transfers", sum(1 for r in ledger if r[3] == "Платформы"), {k: v["totals"].get("decision") for k, v in expected.items()})
