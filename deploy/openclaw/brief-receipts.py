#!/usr/bin/env python3
"""Отмечает ночной бриф отправленным по квитанции OpenClaw: только то, что точно дошло до владельца.
Сообщение уходит после хода агента, поэтому сам агент этого знать не может. Запускает условие утреннего health.
Отметка влияет на счёт «висит» и на то, какие строки «записал сам» уже показаны: без квитанции они придут снова."""
import json
import os
import re
import subprocess
import sys

DECLARATION = "ledger-nightly:v1"
TITLE = re.compile(r"(?:💰\s*)?Байбит\s*[·:—-]\s*бриф\s*#(\d+)", re.I)
LEDGER = os.path.expanduser("~/.config/ledger/bin/ledger")


def openclaw(*args):
    return json.loads(subprocess.check_output(["openclaw", "cron", *args, "--json"], text=True, timeout=60))


def main():
    jobs = openclaw("list", "--all").get("jobs", [])
    job = next((j for j in jobs if j.get("declarationKey") == DECLARATION), None)
    if job is None:
        return
    target = str((job.get("delivery") or {}).get("to") or "").removeprefix("telegram:")
    entries = openclaw("runs", "--id", job["id"], "--limit", "20").get("entries") or []
    for e in entries:
        summary = (e.get("summary") or "").strip()
        title = summary.splitlines()[0].strip("*# ") if summary else ""
        m = TITLE.fullmatch(title)
        resolved = (e.get("delivery") or {}).get("resolved") or {}
        if not (m and e.get("action") == "finished" and e.get("status") == "ok" and e.get("delivered") is True
                and e.get("deliveryStatus") == "delivered" and resolved.get("ok") is True
                and str(resolved.get("to", "")).removeprefix("telegram:") == target):
            continue
        subprocess.run([LEDGER, "brief-sent", "--brief", m[1]], check=True, stdout=subprocess.DEVNULL)


if __name__ == "__main__":
    try:
        main()
    except (subprocess.SubprocessError, OSError, ValueError) as err:
        sys.exit(f"квитанции брифа не прочитаны: {err}")
