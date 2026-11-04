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
```

Откат на VPS: `cd /opt/ledger && set -a && . /etc/ledger/ledger.env && set +a && bash deploy/rollback.sh`
(`--restore-backup` — ещё и база из копии перед деплоем; записанное после неё уйдёт в `/data/quarantine/`).

## Копии и восстановление

- `bash deploy/install-backup-launchd.sh` — поставить копирование на мак (раз в 6 часов и при входе).
- `bash deploy/restore-drill.sh` — восстановить свежую копию локально и поднять на ней сервис.
- `bash deploy/replica-drill.sh` на VPS — восстановить базу из реплики Litestream и проверить её.

## Ключи агентов

На VPS, `cd /opt/ledger`; ключ печатается один раз, в базе остаётся только SHA-256:

```
bash deploy/compose.sh exec -T app ledger keys create bobr --scope read      # decide — только openclaw-main
bash deploy/compose.sh exec -T app ledger keys list
bash deploy/compose.sh exec -T app ledger keys revoke bobr                   # действует сразу
bash deploy/compose.sh exec -T app ledger keys create bobr --scope read --replace   # новый вместо прежнего
```

Агенту — `~/.config/ledger/credentials` (chmod 600): `LEDGER_URL=https://ledger.gosling-agency.ru` и
`LEDGER_KEY=<ключ>`. Проверка: `ledger sql "SELECT 1"` отвечает строкой, `ledger sql "DELETE FROM meta"` — 403.
Файл по умолчанию держит ключ только на чтение. Задание с большими правами (сборщики мака, `decide`) называет свой файл через
`LEDGER_CREDENTIALS=<путь>`.

## Сайт

```
cp deploy/nginx/ledger.conf /etc/nginx/sites-available/ledger
ln -s /etc/nginx/sites-available/ledger /etc/nginx/sites-enabled/ledger
certbot certonly --webroot -w /var/www/letsencrypt -d ledger.gosling-agency.ru
nginx -t && systemctl reload nginx
```

Пока сертификата нет, блок 443 закомментирован в sites-available (DNS ещё не указывает на сервер).
