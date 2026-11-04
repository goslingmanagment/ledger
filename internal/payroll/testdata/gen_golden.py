#!/usr/bin/env python3
"""Regenerates golden.json: synthetic tabs through the original parser, to compare the Go port with.

    PYTHONDONTWRITEBYTECODE=1 python3 internal/payroll/testdata/gen_golden.py <dir with a copy of payroll.py> > internal/payroll/testdata/golden.json

Nothing here is real: names, pages and amounts are made up, and the only input is this file."""
import json, random, sys
from datetime import date, timedelta

sys.dont_write_bytecode = True
sys.path.insert(0, sys.argv[1])
import payroll as P  # noqa: E402

TODAY = date(2026, 10, 20)
EPOCH = date(1899, 12, 30)


def serial(d):
    return (d - EPOCH).days


def week(d0):
    s = serial(d0)
    return ["Работяга", "Страница", *[s + k for k in range(7)], "Итого за неделю", "З/П", "Выплачено", "Фикс"]


def head(kv=0, paid=0, rate=0.25, shift=None):
    h = ["К выплате:", "", "Заработано:", "", "Выплачено:", "", "Отраб. дней:", "", "Средн./смена:", "", "Лучший день:", ""]
    v = [kv, "", 0, "", paid, "", 0, "", 0, "", 0, ""]
    if rate is not None:
        h.append("Ставка %")
        v.append(rate)
    if shift is not None:
        if rate is None:
            h.append("")
            v.append("")
        h.append("Фикс/см $")
        v.append(shift)
    return [h, v, []]


def block(d0, pages, rate=0.25, pay=None, fix=None, paid="нет"):
    """pages: {page: [7 cells]}; the З/П column follows rate unless pay overrides the week's total."""
    rows = [week(d0)]
    total = 0
    for i, (page, cells) in enumerate(pages.items()):
        rev = sum(x for x in cells if isinstance(x, (int, float)))
        z = rev * rate
        total += z
        row = ["Работник", page, *cells, rev, z]
        if i == 0:
            row += [paid, fix if fix is not None else 0]
        rows.append(row)
    if fix:
        total += fix
    rows.append(["", "Сумма", *[0] * 7, 0, total if pay is None else pay])
    rows.append([])
    return rows


D = date(2026, 9, 7)
EMPTY = ["", "", "", "", "", "", ""]
CASES = {}


def case(name, rows, today=TODAY):
    CASES[name] = (rows, today)


# ---- tabs the parser accepts ----
w1 = {"Page A": [10, 20.5, "", 0, 33.33, 0.1, 0.2], "Page B": ["", 0.3, 40, "", 1e-3, "", 5]}
w2 = {"Page A": [12.75, "", "", 7.1, 0.7, "", ""], "Page B": ["", "", 100.01, 1.1, "", "", 0]}
paid1 = 25.0
rows = [*block(D, w1, paid=("да")), *block(D + timedelta(7), w2)]
tot = sum(r[10] for r in rows if len(r) > 10 and r[1] == "Сумма")
case("two_weeks", head(kv=tot - paid1, paid=paid1) + rows)

fix_rows = [*block(D, {"Page A": [100, 200, "", "", "", "", ""]}, fix=30),
            *block(D + timedelta(7), {"Page A": [50, "", "", 0, "", "", ""]}, fix=10)]
tot = sum(r[10] for r in fix_rows if len(r) > 10 and r[1] == "Сумма")
case("fix_note_and_zero_day", head(kv=tot, shift=10) + fix_rows)

over = [*block(D, {"Page A": [10, 10, 10, 10, 10, 10, 10]}), *block(D + timedelta(3), {"Page A": [1, 2, 3, 4, 5, 6, 7]})]
tot = sum(r[10] for r in over if len(r) > 10 and r[1] == "Сумма")
case("overlapping_weeks", head(kv=tot) + over)

norate = block(D, {"Page A": [1.5, 2.5, "", "", "", "", ""]}, rate=0.3)
tot = sum(r[10] for r in norate if len(r) > 10 and r[1] == "Сумма")
case("no_rate_note", head(kv=tot, rate=None) + norate)

empty = block(D, {"Page A": EMPTY}, pay=0) + block(D + timedelta(7), {"Page A": EMPTY}, pay=0)
case("empty_weeks_after_today", head() + block(date(2030, 1, 7), {"Page A": EMPTY}, pay=0))
case("empty_weeks", head() + empty)
case("negative_refund_day", head(kv=5) + block(D, {"Page A": [30, -10, "", "", "", "", ""]}))

# ---- refusals ----
ok = block(D, {"Page A": [100, "", "", "", "", "", ""]})
good = head(kv=25) + ok
case("bad_a1", [["Итого:"], [1]] + ok)
case("a2_not_number", [["К выплате:"], ["x"]] + ok)
case("no_rows", [])
case("no_paid_header", [["К выплате:", "", "x"], [25, "", 1], []] + ok)
case("paid_not_number", [["К выплате:", "Выплачено:"], [25, "x"], []] + ok)
nd = [r[:] for r in ok]
nd[0][4] = "07.09"
case("date_not_number", head(kv=25) + nd)
ns = [r[:] for r in ok]
ns[0][5] = ns[0][5] + 1
case("dates_not_consecutive", head(kv=25) + ns)
gap = [*block(D, {"Page A": [4, "", "", "", "", "", ""]}), *block(D + timedelta(14), {"Page A": [8, "", "", "", "", "", ""]})]
case("weeks_gap", head(kv=3) + gap)
back = [*block(D, {"Page A": [4, "", "", "", "", "", ""]}), *block(D - timedelta(7), {"Page A": [8, "", "", "", "", "", ""]})]
case("weeks_back", head(kv=3) + back)
ns2 = [r[:] for r in ok]
ns2 = ns2[:-2]
case("no_sum_row", head(kv=25) + ns2)
nb = [r[:] for r in ok]
nb[1] = nb[1][:2] + ["x", "y", "", "z", "", "", ""] + nb[1][9:]
case("cells_not_numbers", head(kv=25) + nb)
nb2 = [r[:] for r in ok]
nb2[1][10] = "много"
nb2[1].extend([""] * (13 - len(nb2[1])))
nb2[1][12] = True
case("zp_and_fix_not_numbers", head(kv=25) + nb2)
case("future_days", head(kv=25) + block(date(2026, 10, 19), {"Page A": [100, 50, 7, "", "", "", ""]}, pay=25 + 12.5 + 1.75))
copy1 = {"Page A": [1, 2, 3, "", "", "", ""]}
cp = [*block(D, copy1), *block(D + timedelta(7), copy1)]
case("copied_week", head(kv=3) + cp)
cp2 = [*block(D, {"Page A": [1, 2.0, 3, "", "", "", ""]}), *block(D + timedelta(7), {"Page A": [1.0, 2, 3.0, "", "", "", ""]})]
case("copied_week_int_vs_float", head(kv=3) + cp2)
zp = [r[:] for r in ok]
zp[2][10] = 26.5
case("week_pay_differs_from_rows", head(kv=26.5) + zp)
zps = [r[:] for r in ok]
zps[2][10] = "итого"
case("week_pay_is_text", head(kv=25) + zps)
zpe = [r[:] for r in ok]
zpe[2] = zpe[2][:10]
case("week_pay_is_empty", head(kv=25) + zpe)
rt = block(D, {"Page A": [100, "", "", "", "", "", ""]}, rate=0.5)
case("rate_differs", head(kv=50, rate=0.25) + rt)
rt2 = block(D, {"Page A": [1234567.5, "", "", "", "", "", ""]}, rate=0.5, fix=1000.25)
case("rate_differs_large_numbers", head(kv=rt2[-2][10], rate=0.25) + rt2)
nd2 = [["Работяга", "Страница", *[serial(D) + k for k in range(7)]], ["Работник", "Page A", *EMPTY, 0, 5], ["", "Сумма", *[0] * 7, 0, 5], []]
case("paid_without_days", head(kv=5, rate=None) + nd2)
fx = [["Работяга", "Страница", *[serial(D) + k for k in range(7)]], ["Работник", "Page A", *EMPTY, 0, 0, "", 5], ["", "Сумма", *[0] * 7, 0, 5], []]
case("fix_without_days", head(kv=5) + fx)
st = head(kv=25) + ok + [["заметка на полях"], ["", 7]]
case("stray_rows", st)
st2 = head(kv=25) + ok + [[f"строка {i}"] for i in range(8)]
case("stray_rows_many", st2)
st3 = [*head(kv=25), ["до блока"], *ok, [0, " ", ""]]
case("stray_before_block_blank_after", st3)
case("header_differs", head(kv=99, paid=0.125) + ok)
case("many_errors_together", head(kv=1000) + nd + [["мусор"]] + block(date(2026, 10, 26), {"Page A": [8, "", "", "", "", "", ""]}))
nullcell = [r[:] for r in ok]
nullcell[1][3] = None
case("null_cell", head(kv=25) + nullcell)
boolcell = [r[:] for r in ok]
boolcell[1][4] = False
case("bool_cell", head(kv=25) + boolcell)

# ---- random tabs: the float paths of sum(), round() and the day split ----
rng = random.Random(20261104)


def cell():
    x = rng.random()
    if x < .35:
        return ""
    if x < .55:
        return rng.randint(0, 900)
    if x < .9:
        return round(rng.uniform(0, 500), rng.choice([1, 2, 3]))
    return rng.uniform(0, 500)


for n in range(40):
    rate = rng.choice([.2, .22, .25, .3, 0.275])
    pages = [f"Page {c}" for c in "ABCDE"[:rng.randint(1, 5)]]
    d0, rows, tot = D - timedelta(7 * 6), [], 0.0
    for w in range(rng.randint(1, 6)):
        fix = rng.choice([0, 0, 10, 12.5, 7.77])
        b = block(d0, {p: [cell() for _ in range(7)] for p in pages}, rate=rate, fix=fix)
        if not any(isinstance(x, (int, float)) and x for r in b[1:-2] for x in r[2:9]):
            b = block(d0, {p: EMPTY for p in pages}, rate=rate, pay=0)
        if fix and not any(isinstance(x, (int, float)) for r in b[1:-2] for x in r[2:9]):
            b = block(d0, {p: EMPTY for p in pages}, rate=rate, pay=0)
        rows += b
        tot += b[-2][10]
        d0 += timedelta(7)
    paid = rng.choice([0, 10.5, tot / 3])
    case(f"random_{n:02d}", head(kv=tot - paid, paid=paid, rate=rate, shift=rng.choice([None, 10])) + rows)


def result(rows, today):
    where = "Model/Tab"
    try:
        r = P.parse_tab(rows, where, today)
    except P.Fail as e:
        return dict(error=str(e))
    return dict(days={str(d): v for d, v in sorted(r["days"].items())}, worked=sorted(str(d) for d in r["worked"]), notes=r["notes"])


NAMES = {
    "ok": [["Шапка"], ["Чаттер"], ["Первый"], [""], [], ["Второй", 5], ["ИТОГО"], ["после"]],
    "number_name": [["Чаттер"], [12.5], [7], ["ИТОГО"]],
    "no_block": [["Чаттер"], ["Первый"]],
    "reversed": [["ИТОГО"], ["Первый"], ["Чаттер"]],
    "empty": [],
}


def names(rows):
    try:
        return dict(names=P.names(rows, "Model/Итого"))
    except P.Fail as e:
        return dict(error=str(e))


out = dict(today=str(TODAY), tabs={k: dict(rows=r, today=str(t), **result(r, t)) for k, (r, t) in CASES.items()},
           names={k: dict(rows=r, **names(r)) for k, r in NAMES.items()})
print(json.dumps(out, ensure_ascii=False, separators=(",", ":")))
