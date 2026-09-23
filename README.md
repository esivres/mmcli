# mmcli

Простой консольный клиент Mattermost. Stateless: каждый запуск — отдельный
процесс, который ходит к серверу по REST. Секреты (пароль, кешированный
session-токен) хранятся в системном keyring (Secret Service на Linux, Keychain на macOS).

## Установка

```sh
brew tap esivres/mmcli https://github.com/esivres/mmcli
brew install --cask esivres/mmcli/mmcli
```

На Linux имя `mmcli` занято ModemManager (`/usr/bin/mmcli`): в скриптах
вызывайте `$(brew --prefix)/bin/mmcli`.

## Сборка

```sh
go build -o mmcli ./cmd/mmcli
```

Релиз: пуш тега `v*` — GoReleaser собирает бинарники и обновляет
`Casks/mmcli.rb`.

## Аутентификация

Логин по login_id + паролю; полученный session-токен кешируется в keyring и
переиспользуется между запусками. При истечении токена (401) клиент
автоматически перелогинивается один раз.

```sh
# пароль из stdin (предпочтительно)
printf '%s' "$PASSWORD" | mmcli login \
  --context work \
  --url https://mm.example.com \
  --login-id me@example.com \
  --team myteam \
  --password-stdin
```

Источник пароля при login (по приоритету): `--password` → `--password-stdin`
→ `$MMCLI_PASSWORD` → строка из stdin.

## Контексты

Несколько серверов хранятся как именованные контексты в
`$XDG_CONFIG_HOME/mmcli/config.json` (только не-секретные поля).

```sh
mmcli context list
mmcli context current
mmcli context use work
mmcli logout --context work   # стирает токен и пароль из keyring
```

## Чтение

```sh
mmcli get <permalink|post_id>            # один пост
mmcli get <permalink|post_id> --thread   # весь тред
mmcli thread <permalink|post_id>         # то же, что get --thread
```

Принимаются permalink (`https://host/<team>/pl/<id>`) и голые 26-символьные ID.

## Поиск

Флаги собираются в search-модификаторы Mattermost (`in:`, `from:`, `after:`,
`before:`), к ним добавляется свободный запрос.

```sh
mmcli search "релиз" --channel ops --from alice \
  --after 2026-01-01 --before 2026-02-01 --limit 50
```

Команда автоматически читается на уровне той team, что задана `--team` или
дефолтной для контекста.

## Ответ и публикация

```sh
mmcli reply <permalink|post_id> "текст ответа"   # тред: отвечает в корень нити
mmcli post  <channel|channel_link> "текст"       # новое сообщение в канал
```

## Вывод

По умолчанию — компактный JSON (потребитель — автоматизация). `--pretty` —
форматированный JSON. Посты разворачиваются в плоский вид с ISO-временем и
разрешённым username вместо сырых ID; тред/поиск отдаются в хронологическом
порядке (старые сверху).

## Замечание про архитектуру

Это намеренно чистый CLI без демона: все операции — request/response, держать
постоянный коннект незачем. Постоянное соединение (WebSocket) имело бы смысл
только для live-событий; если такое понадобится, его можно добавить отдельной
подкомандой-демоном, не ломая текущий CLI.
