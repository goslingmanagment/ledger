# Деплой ledger

Образец — `deploy/` TaskIndex, упрощённый: у ledger нет режима «записи закрыты», поэтому сбой нового
релиза до здорового `/health` возвращает прежний релиз, а если миграции уже прошли — и базу из копии,
снятой перед деплоем. Сырые факты после копии сборщики выгружают заново.

## Где что

| Что | Где |
|---|---|
| Checkout и bare-репозиторий | `/opt/ledger`, `/opt/ledger.git` на `agency-de` |
| Настройки и ключи | `/etc/ledger/ledger.env` (root, 600), пример — `deploy/.env.example` |
| База и реплика | том `ledger_data`: `/data/ledger.db`, реплика Litestream `/data/replica/ledger` |
| Состояние релизов | `/opt/ledger/deploy/.runtime/current.json`, снимки `releases/<deployment>/` |
| Порт | `127.0.0.1:3300` → контейнер `:8080`, nginx `deploy/nginx/ledger.conf` |
| Копии на маке | `~/Backups/ledger/<UTC>/ledger.db`, launchd `ru.gosling-agency.ledger-backup`, каждые 6 часов |

## Выпуск

```
bash deploy/release.sh            # origin/main: CI зелёный → образ на VPS → копия → deploy.sh → копия
bash deploy/release.sh --plan     # только план на сервере
bash deploy/release.sh --bootstrap   # первая установка
bash deploy/release.sh --build-here  # пока CI на маке (CI_POOL=pc): зелёный CI, образ собирается на VPS
```

Откат на VPS: `cd /opt/ledger && set -a && . /etc/ledger/ledger.env && set +a && bash deploy/rollback.sh`
(`--restore-backup` — ещё и база из копии перед деплоем; записанное после неё уйдёт в `/data/quarantine/`).

## Копии и восстановление

- `bash deploy/install-backup-launchd.sh` — поставить копирование на мак (раз в 6 часов и при входе).
- `bash deploy/restore-drill.sh` — восстановить свежую копию локально и поднять на ней сервис.
- `bash deploy/replica-drill.sh` на VPS — восстановить базу из реплики Litestream и проверить её.

## Если сервер потерян

Копии на маке — полная база (`~/Backups/ledger/<UTC>/ledger.db`). На новом VPS: перенести `/etc/ledger/ledger.env`; `LEDGER_HOST=root@<адрес> bash deploy/release.sh --bootstrap --build-here`;
остановить сервис: `bash deploy/compose.sh stop app litestream`; положить копию в том:
`docker run --rm -i -v ledger_data:/data alpine sh -c 'rm -f /data/ledger.db* && cat > /data/ledger.db && chown 1000:1000 /data/ledger.db' < ledger.db`; запустить: `bash deploy/compose.sh up -d`;
проверить: `ledger verify /data/ledger.db`, `ledger health --now`; перевести DNS и получить сертификат. Сбор Bybit и хаба догонит пропущенное сам, сборщики мака досылают очередь.
Проверено учением `deploy/restore-drill.sh` на маке; на живом VPS эта последовательность не прогонялась.

## Ключи агентов

На VPS, `cd /opt/ledger`; ключ печатается один раз, в базе остаётся только SHA-256:

```
bash deploy/compose.sh exec -T app ledger keys create bobr --scope read,propose   # агенты: read,propose; decide — только openclaw-main (read,propose,decide)
bash deploy/compose.sh exec -T app ledger keys list
bash deploy/compose.sh exec -T app ledger keys revoke bobr                   # действует сразу
bash deploy/compose.sh exec -T app ledger keys create bobr --scope read,propose --replace   # новый вместо прежнего
```

Агенту — `~/.config/ledger/credentials` (chmod 600): `LEDGER_URL=https://ledger.gosling-agency.ru` и
`LEDGER_KEY=<ключ>`. Проверка: `ledger sql "SELECT 1"` отвечает строкой, `ledger sql "DELETE FROM meta"` — 403.
Файл по умолчанию держит ключ только на чтение. Задание с большими правами (сборщики мака, `decide`) называет свой файл через
`LEDGER_CREDENTIALS=<путь>`.

## Задания на маке

`ledger collect sheets` и `ledger collect cards` живут на маке: `gog` и сессия телеграма есть только там. Таблицы читаются раз в час, бот
CinCin раз в 30 минут, только на чтение (`gog --readonly`, `uv run tg history`). Пока API недоступен, события лежат в очереди
(`~/.config/ledger/spool` и `spool-cards`) и досылаются без дублей. Журнал раз в час: в тени `ledger journal sync` толкает новые коммиты
файла из /OS на сервер, после переключения `ledger mirror` пишет файл из базы, коммит делает ближайшая сессия /OS.

1. На VPS: `bash deploy/compose.sh exec -T app ledger keys create mac-collector --scope read,collect,import`.
2. На маке в `~/.config/ledger/` (всё chmod 600): `credentials.collector` с `LEDGER_URL` и `LEDGER_KEY`;
   `sheets.json` — `{"account": "...", "tables": {"<модель>": "<id таблицы>"}}`, пример — `internal/payroll/testdata/sheets.example.json`;
   `cards.json` — `{"peer": <id бота>, "account": "...", "tg_dir": "<каталог tg-inbox>"}`, пример — `internal/cards/testdata/cards.example.json`.
3. `bash deploy/install-mac-jobs.sh shadow` (после переключения `live`): бинарник в `~/.config/ledger/bin`, launchd-агенты
   `ru.gosling-agency.ledger-cards`, `-sheets`, `-journal` или `-mirror` (шаблон один, `deploy/launchd/ru.gosling-agency.ledger-job.plist`),
   журналы в `~/Library/Logs/ledger`.

Карточки людей переносятся один раз: `ledger import people <папка people>` внутри контейнера, папку — через `docker cp`. Стартовые
долги — `ledger payroll open --dry-run`, потом без `--dry-run`: это делает человек, посмотрев таблицу.

## Сайт

```
cp deploy/nginx/ledger.conf /etc/nginx/sites-available/ledger
ln -s /etc/nginx/sites-available/ledger /etc/nginx/sites-enabled/ledger
certbot certonly --webroot -w /var/www/letsencrypt -d ledger.gosling-agency.ru
nginx -t && systemctl reload nginx
```

Пока сертификата нет, блок 443 закомментирован в sites-available (DNS ещё не указывает на сервер).

## Рутины OpenClaw

`deploy/openclaw/`: промпты, условия и `install.sh`, идемпотентно по `declarationKey`. Ночной бриф 00:30, health 11:00, срочное каждые 15 минут
(модель запускается, только когда есть что сказать), цифры месяца по понедельникам, закрытие месяца 10–18-го. Получатель — `LEDGER_TG_CHAT_ID`,
в репозитории его нет. Закрытие требует ключа с правом decide: `~/.config/ledger/credentials.decide`; основная сессия OpenClaw применяет ответы
владельца тем же ключом: `ledger decide`, затем `ledger rule add` и `ledger note add`, чтобы отвеченное больше не спрашивать.

## Ночь переключения

Только после «да» владельца. `bash deploy/switch.sh check` до ночи: режим, ключи сбора, API, журнал равен экспорту, проверки зелёные, кроме кассы и
карт, их выравнивает финальный импорт. `LEDGER_TG_CHAT_ID=<чат> bash deploy/switch.sh go --aligns <файл вне репозитория>`: финальный импорт, `ledger mode live`,
задания мака и рутины OpenClaw. Откат: `bash deploy/switch.sh back`. `ledger-sync` неделю остаётся запасным путём: его джобы только выключены.
