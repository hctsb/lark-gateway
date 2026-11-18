<div align="center">

<img src="assets/logo-256.png" alt="LarkGateway logo: a bird in flight carrying its song" width="120">

# LarkGateway

**Run your own scripts the moment something happens in Lark.**

[![License: MIT](https://img.shields.io/badge/license-MIT-2B9C9C.svg)](LICENSE) ![Go 1.27+](https://img.shields.io/badge/go-1.27%2B-2E4A7A.svg) ![Windows](https://img.shields.io/badge/runs%20on-Windows%20%7C%20Linux-1F2A44.svg) ![Lark and Feishu](https://img.shields.io/badge/works%20with-Lark%20%26%20Feishu-5CC8C8.svg)

[What it does](#what-it-does) · [Quick start](#quick-start) · [Your first route](#your-first-route-an-echo-bot) · [Configuration](#configuration-reference) · [Operations](#running-it-day-to-day) · [Troubleshooting](#troubleshooting) · [FAQ](#faq)

</div>

---

## What it does

Want a script or program on your own computer to run when something happens in Lark? Someone edits a field in a Base, sends your bot a message, drops a file in a chat. **This is the thing for it.**

```
In Lark                     On your computer

a message arrives ─┐
a field is edited ─┼──► LarkGateway ──► your script
a file is posted  ─┘    (rules file)    .py .ps1 .bat .exe
```

Some things people use it for:

- A colleague sends the bot a bank statement → your script imports it into your accounting system and replies "done, 42 lines imported".
- Someone ticks "Approve" on a row in a Lark Base → your program generates a PDF and sends it.
- A customer table in a Base is edited → your server refreshes its own copy straight away, instead of re-reading the whole table every few minutes.

You write a small rules file, *"when this happens, run that"*, and your script can be anything that runs on your computer: Python, PowerShell, Node, a `.bat` file, an `.exe`. You don't need a public web address, a cloud server, or to know any Go.

## Features

- 🔌 **No public address needed.** Uses Lark's persistent connection: your computer makes one outgoing connection, like a browser does, and Lark pushes events down it.
- 💾 **Nothing is lost.** Every event is written to disk *before* Lark is told it arrived. A crash, a reboot or a failing script never loses one.
- 🔁 **Retries, then tells you.** A failed run is tried again after 1 minute, then 5 (3 tries by default; with more, later tries wait 15 minutes). If it still fails, the event is set aside and an alert is posted to a Lark chat.
- 🕳️ **Catches up after outages.** Lark does not replay events you missed while offline. After every reconnect, your route's catch-up command runs so your script can look for them.
- 🧭 **Many bots, one process.** One connection per Lark app, as many apps and routes as you like, each route with its own command, settings and folder.
- ⏰ **Scheduled runs too.** Give a route a timer (`tick`); your script can even say when it next needs to run.
- 🔒 **Least privilege.** Secrets stay in `.env`. Each command sees only the settings its route lists.
- 🩺 **Easy to watch.** A health page and `health.json`, JSON logs, and alerts in Lark.
- 🪶 **Small.** One ~14 MB executable, about 40 MB of memory in service.

## Contents

- [How it works](#how-it-works)
- [Quick start](#quick-start)
- [Your first route: an echo bot](#your-first-route-an-echo-bot)
- [More examples](#more-examples)
- [What your command receives](#what-your-command-receives)
- [Base record changes: the scope Lark doesn't document](#base-record-changes-the-scope-lark-doesnt-document)
- [Configuration reference](#configuration-reference)
- [Running it day to day](#running-it-day-to-day)
- [Troubleshooting](#troubleshooting)
- [FAQ](#faq)
- [Limitations](#limitations)
- [Contributing](#contributing)
- [License](#license)

## How it works

```
Lark sends an event
      │
      ▼
1. Does a route match it?  ── no ──► ignored
      │ yes
      ▼
2. Save it to spool/<route>/pending/
      │
      ▼
3. Tell Lark "got it"
      │
      ▼
4. Run your command (event JSON on stdin),
   one event at a time, oldest first
      │
      ├── exit 0 ──► done/  (kept 14 days)
      │
      └── failed ──► try again after 1, then 5 min
                          │ still failing
                          ▼
                     failed/ + alert in Lark
```

- **Apps.** Each `[[app]]` is one Lark app (bot). The gateway holds exactly one connection for it. Lark's SDK reconnects by itself; if it gives up, the gateway starts it again a minute later.
- **Routes.** Each `[[route]]` says: for this app and event type, where these fields have these values, run this command. One event can match several routes; each gets its own copy.
- **Catch-up.** After every connect and reconnect, each route that has a `catchup` command runs it once, after any events already waiting.
- **Ticks.** A route with a `tick` also runs a command on a timer, alongside its events.
- **It decides nothing.** What an event *means* is your command's job. The gateway only delivers it reliably.

## Quick start

You need: a Lark (or Feishu) **custom app** you can configure, and a Windows or Linux machine that stays on. Ten minutes, start to finish.

### 1. Set up the app in Lark

In the [Lark developer console](https://open.larksuite.com/app) (Feishu: [open.feishu.cn/app](https://open.feishu.cn/app)), open your app, or create a custom app:

1. **Features → Bot:** turn the bot on.
2. **Permissions & Scopes:** add the scopes your events need. For the echo bot below: `im:message.p2p_msg:readonly` (receive direct messages sent to the bot) and `im:message:send_as_bot` (reply as the bot). Lark's console lists the scope each event needs.
3. **Events & Callbacks → Event configuration:** choose **"Receive events through persistent connection"**, then add the event `im.message.receive_v1` ("Message received").
   > The console only lets you save this mode while a client is connected. If it refuses, start the gateway (step 4) and save again.
4. **Version Management & Release:** create a version and publish it. Scope and event changes only take effect after a release.
5. Note the **App ID** (`cli_...`) and **App Secret** from **Credentials & Basic Info**.

### 2. Install LarkGateway

**Option A: download** `lark-gateway.exe` from the [Releases](../../releases) page.

**Option B: with Go installed** (1.27 or later):

```bash
go install github.com/hctsb/lark-gateway@latest
```

**Option C: build from source:**

```bash
git clone https://github.com/hctsb/lark-gateway.git
cd lark-gateway
go build -o lark-gateway.exe .      # Linux: go build -o lark-gateway .
```

Put the executable in its own folder, for example `C:\LarkGateway`. Its config, spool and logs live beside it.

### 3. Configure

Copy `gateway.example.toml` to `gateway.toml` in that folder, and create a `.env` file beside it holding the secret:

```ini
# .env  (never commit this file)
MYBOT_APP_SECRET=your-app-secret-here
```

A minimal `gateway.toml`:

```toml
[alert]
app = "mybot"
chat_id = "oc_xxxxxxxx"        # a chat the bot is in; see "Finding a chat_id" below

[[app]]
name = "mybot"
app_id = "cli_xxxxxxxx"
secret_env = "MYBOT_APP_SECRET"
domain = "lark"                # "feishu" for open.feishu.cn

[[route]]
name = "echo"
app = "mybot"
event = "im.message.receive_v1"
match = { "event.message.chat_type" = ["p2p"], "event.sender.sender_type" = ["user"] }
command = ["python", "echo.py"]
workdir = "C:\\LarkGateway\\jobs"
pass_app_credentials = true
```

### 4. Check and run

```bash
lark-gateway.exe -check         # validates gateway.toml, .env, commands and folders
lark-gateway.exe -test-alert    # posts a test message to the alert chat
lark-gateway.exe                # runs in the foreground; Ctrl+C stops it cleanly
```

Open <http://127.0.0.1:8791/>: your app should show `"connected": true`.

### 5. Run it as a service

**Windows** (uses [NSSM](https://nssm.cc/), `choco install nssm`). From an administrator PowerShell in the gateway's folder:

```powershell
powershell -ExecutionPolicy Bypass -File install.ps1
```

It refuses to change anything unless `-check` passes, locks `.env` down to SYSTEM and Administrators, installs the service `LarkGateway` (automatic start, restart on failure, clean stop), starts it and shows the health page.

**Linux** (systemd; untested by us, contributions welcome):

```ini
# /etc/systemd/system/lark-gateway.service
[Unit]
Description=LarkGateway
After=network-online.target
Wants=network-online.target

[Service]
WorkingDirectory=/opt/lark-gateway
ExecStart=/opt/lark-gateway/lark-gateway -config /opt/lark-gateway/gateway.toml
Restart=always
RestartSec=10
KillSignal=SIGINT

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now lark-gateway
```

## Your first route: an echo bot

Save as `C:\LarkGateway\jobs\echo.py`. It replies to every direct message with the same text. It uses only Python's standard library.

```python
import json, os, sys, urllib.request

event = json.load(sys.stdin)                     # the Lark event, exactly as Lark sent it
msg = event["event"]["message"]
text = json.loads(msg["content"]).get("text", "(not text)")

base = "https://open.feishu.cn" if os.environ["GATEWAY_LARK_DOMAIN"] == "feishu" else "https://open.larksuite.com"

def post(path, body, token=None):
    req = urllib.request.Request(base + path, json.dumps(body).encode(), {"Content-Type": "application/json"})
    if token:
        req.add_header("Authorization", "Bearer " + token)
    return json.load(urllib.request.urlopen(req, timeout=20))

auth = post("/open-apis/auth/v3/tenant_access_token/internal", {
    "app_id": os.environ["GATEWAY_LARK_APP_ID"],
    "app_secret": os.environ["GATEWAY_LARK_APP_SECRET"],
})
if "tenant_access_token" not in auth:
    sys.exit(f"Lark refused the app login: {auth}")
token = auth["tenant_access_token"]

reply = post(f"/open-apis/im/v1/messages/{msg['message_id']}/reply",
             {"msg_type": "text", "content": json.dumps({"text": "You said: " + text})}, token)
if reply.get("code") != 0:
    sys.exit(f"Lark refused the reply: {reply}")  # non-zero exit = retry later
print("replied to", msg["message_id"])          # printed lines go to the gateway log
```

Send your bot a direct message. Within a second or two it answers, and `logs/gateway.log` shows `command ok`.

Tip: before writing a real handler, point a route at `tools/probe.py`. It records exactly what Lark sends, one line per event, in `tools/probe-seen.ndjson`, and acts on nothing.

## More examples

**A file posted in one group chat:**

```toml
[[route]]
name = "statements"
app = "mybot"
event = "im.message.receive_v1"
match = { "event.message.chat_id" = ["oc_xxxxxxxx"], "event.message.message_type" = ["file"] }
command = ["C:\\Python313\\python.exe", "import_statement.py"]
catchup = ["C:\\Python313\\python.exe", "import_statement.py", "--catchup"]
workdir = "C:\\jobs\\statements"
timeout = "10m"
pass_app_credentials = true       # to download the file and reply
env = ["DB_PASSWORD"]             # only this .env key reaches the script
```

**A record changed in one table of a Base** (read [the next section](#base-record-changes-the-scope-lark-doesnt-document) first):

```toml
[[app]]
name = "mybot"
app_id = "cli_xxxxxxxx"
secret_env = "MYBOT_APP_SECRET"
subscribe_bitable = ["bascnXXXXXXXX"]    # the Base token, from its URL

[[route]]
name = "approvals"
app = "mybot"
event = "drive.file.bitable_record_changed_v1"
match = { "event.file_token" = ["bascnXXXXXXXX"], "event.table_id" = ["tblXXXXXXXX"] }
command = ["powershell", "-NoProfile", "-File", "on_approval.ps1"]
catchup = ["powershell", "-NoProfile", "-File", "on_approval.ps1", "-CatchUp"]
workdir = "C:\\jobs\\approvals"
retries = 1                              # don't run a "send" twice by itself
```

**Only a timer, no events** (e.g. a sync every 10 minutes, as a supervised job with alerts):

```toml
[[route]]
name = "nightly-sync"
app = "mybot"
tick = { every = "10m", command = ["python", "sync.py"] }
workdir = "C:\\jobs\\sync"
```

**A timer the script controls.** The script writes when it next needs to run:

```python
import os, datetime
next_run = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(hours=2)
with open(os.environ["GATEWAY_NEXT_RUN_FILE"], "w") as f:
    f.write(next_run.isoformat())       # or "none": only events wake it (checked every max_wait)
```

The gateway keeps the next run between `tick.every` and `tick.max_wait` (default `6h`), so a script can neither make it run faster than `every` nor sleep longer than `max_wait`.

## What your command receives

| | Event run | Catch-up run | Tick run |
|---|---|---|---|
| **stdin** | the event JSON, unchanged | empty | empty |
| `GATEWAY_REASON` | `event` | `catchup` | `tick` |
| `GATEWAY_EVENT_ID` | the event id | empty | empty |
| **timeout** | route `timeout` | route `timeout` | none (may run for hours) |

Every run also gets:

- `GATEWAY_ROUTE`, `GATEWAY_APP`, `GATEWAY_EVENT_TYPE`, `GATEWAY_LARK_DOMAIN` (`lark` or `feishu`);
- the `.env` keys listed in the route's `env`;
- with `pass_app_credentials = true`: `GATEWAY_LARK_APP_ID` and `GATEWAY_LARK_APP_SECRET`;
- on a route with a tick: `GATEWAY_NEXT_RUN_FILE`;
- a minimal system environment (`PATH`, `TEMP`, `SystemRoot`, `USERPROFILE` and similar), plus `PYTHONUTF8=1`. Nothing else from the gateway's own environment leaks in.

**Exit code:** `0` means done. Anything else means failed: the event is tried up to `retries` times in all (default 3), waiting 1, 5, then 15 minutes between tries, then moved to `failed/` with an alert. Everything the command prints (stdout and stderr, first 200 lines per run) goes into `logs/gateway.log`.

An event as your command sees it (shortened):

```json
{
  "schema": "2.0",
  "header": {
    "event_id": "5e3702a84e847582be8db7fb73283c02",
    "event_type": "im.message.receive_v1",
    "create_time": "1731900000000",
    "app_id": "cli_xxxxxxxx"
  },
  "event": {
    "sender": { "sender_type": "user", "sender_id": { "open_id": "ou_xxxxxxxx" } },
    "message": {
      "message_id": "om_xxxxxxxx",
      "chat_id": "oc_xxxxxxxx",
      "chat_type": "p2p",
      "message_type": "text",
      "content": "{\"text\":\"hello\"}"
    }
  }
}
```

**Matching:** each key in `match` is a dotted path into this JSON (`event.message.chat_type`), and each value is a list of accepted values. *Every* key must match one of its values. Numbers and booleans compare as JSON prints them (`"true"`, `"3"`). Only Lark v2 events (with `header.event_id`) are routed.

## Base record changes: the scope Lark doesn't document

To receive `drive.file.bitable_record_changed_v1` for a Base, the app needs **all** of:

1. The event added in the console (persistent connection), and the scope `docs:event:subscribe`.
2. **`bitable:app` granted as a *user* scope as well as a tenant scope.** With the tenant scope alone, subscribing succeeds and everything looks right, but Lark pushes no events at all. We found this by removing scopes one at a time on a test app.
3. The bot added to the Base with **Can manage** access.
4. The Base token in the app's `subscribe_bitable`. The gateway asks Lark for that Base's changes on every connect and retries each minute until Lark agrees (just after a power cut the network may not be up yet). The health page shows `subscribed` per Base, and an alert is posted if it keeps failing for 5 minutes.

Remember to publish a new app version after changing scopes.

## Configuration reference

`gateway.toml` sits beside the executable (or pass `-config path`). Relative paths are relative to that file's folder. Unknown keys are rejected, so typos are caught by `-check`. Secrets go in `.env` beside it (`KEY=value` lines, `#` comments); a real environment variable with the same name wins over `.env`.

### `[gateway]`

| Key | Default | Meaning |
|---|---|---|
| `spool_dir` | `"spool"` | Where events wait (`pending/`), and land (`done/`, `failed/`) |
| `log_dir` | `"logs"` | `gateway.log`, rotated at 10 MB, 10 files, 90 days |
| `health_addr` | `"127.0.0.1:8791"` | Health page address. Also a lock: a second copy can't start |
| `memory_limit_mb` | `32` | Soft memory limit for the Go runtime |
| `keep_done_days` | `14` | How long successful events stay in `done/` for replay |

### `[alert]`

| Key | Default | Meaning |
|---|---|---|
| `app` | | Which `[[app]]` posts alerts |
| `chat_id` | | The chat that receives them (the bot must be in it). Leave out to disable alerts |
| `down_minutes` | `5` | Alert when an app stays disconnected this long |

The same alert is sent at most once every 30 minutes.

### `[[app]]` (one per Lark app)

| Key | Default | Meaning |
|---|---|---|
| `name` | required | Your short name for the app, used by routes |
| `app_id` | required | `cli_...` from the developer console |
| `secret_env` | required | The `.env` key holding the app secret |
| `domain` | `"lark"` | `"lark"` (open.larksuite.com) or `"feishu"` (open.feishu.cn) |
| `subscribe_bitable` | `[]` | Base tokens whose record changes this app should receive |
| `ignore_events` | `[]` | Extra event types to accept silently. Common housekeeping events (bot added to a chat, message read, reactions…) are already accepted |

### `[[route]]` (one per job)

| Key | Default | Meaning |
|---|---|---|
| `name` | required | Letters, digits and `-`. Names the spool folder |
| `app` | required | Which `[[app]]` it listens on |
| `event` | | Lark event type, e.g. `im.message.receive_v1`. A route needs an `event`, a `tick`, or both |
| `match` | `{}` | `{ "dotted.path" = ["value", ...] }`; every path must hold one of its values |
| `command` | required with `event` | Program and arguments, as a list. Checked by `-check` |
| `workdir` | gateway folder | Folder the command runs in |
| `timeout` | `"10m"` | Longest an event or catch-up run may take |
| `retries` | `3` | Attempts per event before it is set aside |
| `catchup` | | Command run after every (re)connect |
| `tick` | | `{ every = "10m", command = [...], max_wait = "6h" }`. `every` is at least `10s` |
| `env` | `[]` | `.env` keys passed to the command |
| `pass_app_credentials` | `false` | Pass the app's id and secret to the command |

## Running it day to day

| Task | How |
|---|---|
| Is it healthy? | Open `http://127.0.0.1:8791/`, or read `state/health.json` (written every 30 s) |
| Check config after editing | `lark-gateway.exe -check`, then restart |
| Test alerts | `lark-gateway.exe -test-alert` |
| Stop / start (Windows) | `nssm stop LarkGateway` / `nssm start LarkGateway` |
| Update the program | Put the new build beside it as `lark-gateway.new.exe` and run `tools/swap-exe.cmd`: it checks the new build, keeps the old one as a dated backup and restarts |
| Re-run a failed event | Move its file from `spool/<route>/failed/` to `spool/<route>/pending/` |
| Replay a past event | Copy it from `spool/<route>/done/` to `pending/` |
| Add a bot or a job | Add an `[[app]]` or `[[route]]`, run `-check`, restart |

The health page looks like this:

```json
{
  "started": "2026-11-12T02:35:21+08:00",
  "apps": {
    "mybot": { "connected": true, "since": "2026-11-12T02:35:22+08:00",
               "last_event": "2026-11-13T12:06:40+08:00", "subscribed": { "bascnXXXX": true } }
  },
  "routes": {
    "echo": { "pending": 0, "last_ok": "2026-11-13T12:06:52+08:00", "next_tick": "0001-01-01T00:00:00Z" }
  }
}
```

**Alerts** are posted to the alert chat when an app stays disconnected for `down_minutes`, a Base subscription keeps failing for 5 minutes, an event fails all its retries, or a catch-up or tick run fails. To be told if the gateway *itself* stops, point an uptime monitor (Uptime Kuma, for example) at the health page and alert when it stops answering or shows `"connected": false`.

## Troubleshooting

**The app never shows `connected: true`.** Check the App ID and secret, and that `domain` matches your tenant (`lark` for larksuite.com, `feishu` for feishu.cn). `logs/gateway.log` shows the error Lark returned.

**Connected, but no events arrive.**
- Did you publish a new app version after adding the event and scopes?
- Is the subscription mode "persistent connection", not "request URL"?
- For group messages: is the bot in the group, and does it have the group-message scope?
- For Base changes: see [the undocumented scope](#base-record-changes-the-scope-lark-doesnt-document).
- Look in the log for `"event"` lines: `"routes": 0` means it arrived but no route matched. Use `tools/probe.py` to see the exact JSON and fix your `match`.

**Some events arrive, some don't.** Another program is connected with the same app: an old listener, a second gateway on another machine, a test script. Lark gives each event to one connection at random. Stop the other one.

**"another LarkGateway is already running on this machine".** Something already uses `health_addr`. Stop the other copy, or change the address.

**A command works by hand but fails in the gateway.** The command gets a minimal environment (see [What your command receives](#what-your-command-receives)). Pass what it needs with `env`, use full paths, and remember the service account (LocalSystem by default on Windows) may not see your user's drives or Python.

**Events pile up in `pending/`.** A route runs one event at a time. A slow or hanging command holds up the rest until it finishes or hits `timeout`.

### Finding a `chat_id`

Point a route at `tools/probe.py`, send a message in the chat, and read `chat_id` from `tools/probe-seen.ndjson`. For a direct chat with the bot, message the bot.

## FAQ

**Why not use Lark's webhook mode?** It needs a public HTTPS address on your machine. The persistent connection needs only outgoing internet. Either way, something still has to keep the event safe and run your program.

**Why not put Lark's SDK in each of my programs?** With two programs connected for the same app, Lark splits events between them at random and each silently misses some. You need exactly one listener per app, and that is what this is.

**Can I use AnyCross, n8n, Make or Zapier instead?** If your work can happen entirely in the cloud, yes, and you should. LarkGateway is for work that must happen on a machine you own, for example because your database or files are there.

**Can it send things to Lark?** The gateway itself only receives (and posts its own alerts). Your scripts write to Lark through Lark's API, using the app credentials it can pass them.

**Does it work with Feishu?** Yes, set `domain = "feishu"` on the app.

**Can I run it on two machines for redundancy?** Not with the same app: Lark would split events between them. Run it on one machine; the spool and catch-up cover restarts and outages.

More "can't I just…?" answers are in [docs/WHY.md](docs/WHY.md).

## Limitations

- One machine per Lark app (Lark's rule, not ours).
- Events are handled one at a time per route. Put independent work in separate routes.
- Lark does not replay missed events; your `catchup` command has to look for what changed.
- Routing looks at event fields only. Anything smarter belongs in your command.
- Built and run in production on Windows. Linux builds and should work, but we haven't run it there.

## Contributing

Issues and pull requests are welcome. Please run `go vet ./...` and `go test ./...` first, and keep the gateway free of business logic: anything about what an event *means* belongs in the command, not here.

This project is maintained for our own production use and provided as-is. We read everything, but can't promise support.

## Built on

- [`larksuite/oapi-sdk-go`](https://github.com/larksuite/oapi-sdk-go), Lark's official Go SDK, for the connection.
- [`BurntSushi/toml`](https://github.com/BurntSushi/toml) for the config and [`natefinch/lumberjack`](https://github.com/natefinch/lumberjack) for log rotation.
- [NSSM](https://nssm.cc/) to run it as a Windows service.

## License

[MIT](LICENSE) © 2026 Herbal Care Trading Sdn Bhd
