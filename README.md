# slackcli

A command-line client for Slack, made to be driven by AI agents. It reads, searches and writes Slack as you, and prints compact JSON.

It is a Go rewrite of [stablyai/agent-slack](https://github.com/stablyai/agent-slack).

## Install

You need Go.

```bash
git clone https://github.com/maelvls/slackcli
cd slackcli
make install
```

`make install` runs `go install ./cmd/slackcli` (into `~/go/bin`) and copies
the agent skill into `~/.claude/skills/slackcli` (`make install-bin` or
`make install-skill` for only one of them, `SKILLS_DIR=...` for another skills
folder).

To update, pull and run `make install` again. To skip the copy, link the
skill to the checkout instead; it then follows whatever branch is checked out,
so keep that checkout on `main` and rerun `go install` after each pull:

```sh
ln -s "$PWD/skills/slackcli" ~/.claude/skills/slackcli
```

## Getting started

Open Slack Desktop and sign in, then:

```bash
slackcli auth test
slackcli message list "#general" --limit 20
slackcli message list "#general" --oldest 2026-01-30 --expand-threads
slackcli search messages "deploy failed" --after 2026-01-01
slackcli message send "#general" "Hello from slackcli"
```

There is nothing to configure: the first command imports your session from Slack Desktop, or from Chrome, Brave or Firefox if Slack Desktop is not there.

`slackcli --help` and `slackcli <command> --help` describe every command, flag and output. This page only covers what the help does not.

## Commands

```text
slackcli
├── auth      whoami | test | add | set-default | remove | import-desktop |
│             import-chrome | import-brave | import-firefox | parse-curl
├── message   get | list | wait | send | edit | delete | react add|remove |
│             scheduled list|cancel | compose | draft list|create|update|delete
├── search    all | messages | files
├── channel   list | info | new | invite | mark
├── user      list | search | get | dm-open
├── later     list | save | complete | archive | reopen | remove | remind
├── unreads
├── inbox
├── watch
├── canvas    create | get | edit
├── workflow  list | preview | get | run
└── audit     path | tail | stats
```

Most commands take a target:

- **A message URL**, such as `https://acme.slack.com/archives/C0123ABCDEF/p1770165109628379`. It names the workspace, the channel and the message, so prefer it. Quote it.
- **A channel**: `#general`, `general` or an ID. A command that acts on one message then also needs `--ts`.
- **A user ID** (`U…`), for a direct message. Reading never opens one: `message list U…` fails when you have no DM with that user.

With several workspaces saved, a channel name needs `--workspace <url-or-name>`, or a default set with `slackcli auth set-default`.

## What is waiting on you

`unreads` lists what you have not read. `inbox` lists who is waiting for an answer, read or not: DMs and group DMs whose newest message is not yours (`dm`, `mpim`), @mentions you have not answered (`mention`), and new replies in threads you posted in (`thread`). Your last message or your reaction answers what came before it. It marks nothing as read.

```bash
slackcli inbox                      # the last 3 days
slackcli inbox --since 12h          # or 2d, or 2026-01-31 (UTC)
slackcli inbox --no-threads --max-body-chars 120
```

```json
{
  "user_id": "U123",
  "since": "2026-11-07T00:00:00.000Z",
  "truncated": false,
  "items": [
    {
      "kind": "mention",
      "channel_id": "C123",
      "channel_name": "general",
      "thread_ts": "1794219159.861309",
      "ts": "1794222538.567359",
      "first_ts": "1794222538.567359",
      "count": 1,
      "author": { "user_id": "U456", "name": "jane" },
      "text": "...",
      "permalink": "https://acme.slack.com/archives/C123/p1794222538567359?thread_ts=1794219159.861309&cid=C123"
    }
  ]
}
```

Items are newest first. `count` and `first_ts` cover the messages from others since your last one there; `ts`, `author`, `text` and `permalink` describe the newest. `thread_ts` is absent for the top level of a DM. `truncated` means there was more to look at than was: more candidates than `--max-conversations` (60), in which case raise it, or more matches than the 200 read from each of the three searches behind the inbox, or a conversation with more than 1000 messages in the window (its item may undercount, or be answered already): for these two only a shorter `--since` helps.

A thread is found through your own posts in it, searched back to two days before `--since`: a thread whose last post of yours is older than that is not listed, however recent its replies, unless one of them mentions you.

## Letting a human review

`message compose` opens an editor in your browser with the text filled in. The command waits until you send or cancel, then prints what happened.

```bash
slackcli message compose "#general" "Here's my update"
```

`message draft create` leaves a draft in your Slack client instead, without posting anything.

## Safe mode

Safe mode makes that review mandatory, whatever the agent was told:

```bash
export SLACKCLI_SAFE_MODE=1
```

`message send` then opens the compose editor instead of posting.

Refused in safe mode: `message edit`, `message delete`, `message scheduled cancel`, `canvas create`, `canvas edit`, `workflow run`, `channel new` and `channel invite`.

Not covered by safe mode: `message react`, `message draft *`, `channel mark`, `later *` and `user dm-open`. These only change your own state (a reaction, an unposted draft, a read marker, your saved items, an open DM).

Safe mode is on for any value other than empty, `0`, `false`, `no` or `off`. The `--safe-mode` flag turns it on for one command; nothing turns it off once the variable is set.

## Authentication

Credentials are looked up in this order:

1. `SLACK_TOKEN`, with `SLACK_COOKIE_D` when it is a browser token (`xoxc-`).
2. The workspaces saved by the `auth` commands.
3. Slack Desktop, then Chrome, Brave and Firefox, imported and saved on the fly.

To use a token from a Slack app instead of your own session:

```bash
SLACK_TOKEN=xoxb-... slackcli auth add --workspace-url https://acme.slack.com --from-env
```

Your own session can do everything your Slack client can. An app token is limited to its scopes: it cannot use `later`, `unreads`, `watch` or `message draft` (nor, for a bot token, `inbox` and `search`), but it is the only way to run `canvas edit` and `canvas create --channel`.

On Enterprise Grid, Slack refuses a few methods to the session of a workspace (`team_is_restricted`) and usually allows them to the session of the organisation, `https://<org>.enterprise.slack.com`, which `auth import-desktop` saves along with the workspaces. The `message draft` commands use it on their own when one of their calls is refused this way (`drafts.list` and `drafts.delete` are; `drafts.create` is not): the same call is made again with the organisation's session, once `auth.test` has confirmed that it is the organisation of that workspace and the same person. Nothing else is retried this way, and `SLACK_TOKEN` is never swapped for a saved session. For any other command that fails with `team_is_restricted`, pass `--workspace <org>.enterprise.slack.com` to run it as the organisation.

Saved credentials live in `~/.config/slackcli/credentials.json`. On macOS the secrets are in the login keychain and the file only holds placeholders. Credentials are only ever sent to Slack, over https (and wss for `watch`).

## Output

- Results are JSON on stdout, with empty fields removed: on one line, or indented when stdout is a terminal.
- Warnings and hints are part of the result, in its `warnings` and `hints` arrays, so a successful command that prints JSON writes nothing on stderr and `slackcli … 2>&1 | jq` still parses. Stderr gets the error of a command that failed (and the warnings that came before it), the notes of the few `auth` commands that print text, the connection notes of `watch` (a stream, not one result), and, when it is a terminal, the progress of `message compose` while it waits for a human. An error is not JSON: keep stderr apart (`2>err.txt`) and check the exit code before parsing.
- Messages have `ts`, which identifies them, and `time`, the same instant as a local date and time.
- A list command always prints its list, `[]` when empty, and `next_cursor` when there is more (pass it to `--cursor`).
- Files attached to messages are downloaded to the cache directory and listed with their local `path`. A file that Slack does not host, such as a Google Docs link, is listed with `"external": true` and its `url` instead. `--no-download` (`message get`, `message list`, `search`) lists them without downloading anything.
- Message content is cut at `--max-body-chars` (8000 characters by default, `-1` for no limit): the same flag and default on `message get`, `message list`, `search`, `unreads` and `later list`.

| Exit code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | Error, with the message on stderr |
| 124 | The command took longer than 30 seconds |
| 130 | Interrupted |

After a timeout or an interrupt, a command that writes may still have gone through: check before retrying.

A command that times out says on stderr what it spent its time on and what it was still waiting for (Slack calls by method, rate-limit waits, downloads, keychain reads, the credentials lock, credential imports). The limit also holds on the wall clock: a command that the machine slept through is ended when it wakes up.

To see where the time of any command goes, set `SLACKCLI_DEBUG=1`. Every Slack call, with its duration, HTTP status, error code and attempt number, every wait and the exit code are written to stderr as they happen, one line each, prefixed with `[slackcli +<time since start>]`. Nothing of it is written otherwise. The trace, like the timeout message, names Slack methods and never their parameters: no token, cookie, command argument, channel, file name or message text. Of an error it keeps the first line, with URLs cut down to their host, and file paths and anything between double quotes removed. Calls made in parallel each count their own time, so the total of a kind can exceed the time that went by.

```console
$ SLACKCLI_DEBUG=1 slackcli message list general --limit 3 >/dev/null
[slackcli +1ms] slackcli 1.0.0, command "message list": command timeout 30s, API timeout 20s
[slackcli +30ms] keychain find-generic-password: 28ms
[slackcli +60ms] keychain find-generic-password: 27ms
[slackcli +294ms] api search.messages: 231ms, HTTP 200
[slackcli +441ms] api conversations.history: 146ms, HTTP 200
[slackcli +732ms] api users.info: 127ms, HTTP 200
[slackcli +737ms] api users.info: 131ms, HTTP 200
[slackcli +739ms] exit code 0; time spent: api users.info 258ms (2 calls), api search.messages 231ms, api conversations.history 146ms, keychain 55ms (2 calls)
```

## Audit log

Every request slackcli makes to Slack is appended, as one JSON line, to `~/.config/slackcli/audit.jsonl` (next to `credentials.json`; `$XDG_CONFIG_HOME/slackcli` when that is set): each attempt of an API call, rate-limited ones and retries included, each file download or upload request, and each opening of the `watch` WebSocket. It is there to answer, after the fact, how often Slack was called, with which methods, by which program and which command, and what failed or was rate limited.

A line has `ts`, `pid`, `ppid`, the names of the parent processes (`parent`, `ancestors`; macOS and Linux, which cut them to 15 or 16 characters), `cmd` and `args`, `kind` (`api`, `file`, `websocket`), `host`, `method`, `status`, `ok`, `error`, `duration_ms`, `attempt`, `rate_limited` and `retry_after_s`. It holds no token, cookie, message or search query: of the arguments only the names of the flags the command defines (never their values), Slack IDs, timestamps, numbers and Slack URLs (without their query) are kept; everything else is `…`, including text that starts with a dash and every argument after `--`. Of a file request only the host is kept, and its line is written once the transfer is over, so that `duration_ms` covers it and a transfer that breaks halfway counts as a failure.

In `audit stats`, `by_source` counts the requests per program and command; the program is the first parent process that is not a shell or a wrapper (`sh`, `bash`, `zsh`, `env`, `timeout`, `nohup`…), so that a daemon running slackcli through `sh -c` is named. `audit path` prints the bare path, and fails when the log is off.

The file is created `0600` (an existing one with a broader mode is made `0600`) and moved to `audit.jsonl.1` once it reaches 20 MB. A failure to write it never fails a command. `SLACKCLI_AUDIT_LOG=<path>` writes it elsewhere and `SLACKCLI_AUDIT_LOG=off` turns it off.

```console
$ slackcli audit stats --since 1h   # totals, per method, per program and command, per hour, peak minute, errors
$ slackcli audit tail -n 50 | jq -c 'select(.rate_limited)'
$ tail -f "$(slackcli audit path)"
```

## Environment variables

| Variable | Effect |
| --- | --- |
| `SLACK_TOKEN` | A Slack token to use instead of the saved workspaces. |
| `SLACK_COOKIE_D` | The browser `d` cookie (`xoxd-…`), needed with an `xoxc-` token. |
| `SLACK_WORKSPACE_URL` | Default for `--workspace`. |
| `SLACKCLI_SAFE_MODE` | Turns safe mode on. |
| `SLACKCLI_COMMAND_TIMEOUT_MS` | Time limit of a command (default 30000). `message compose` and uploads have none. |
| `SLACKCLI_API_TIMEOUT_MS` | Time limit of one Slack request (default 20000). |
| `SLACKCLI_RATE_LIMIT_MAX_WAIT_MS` | How long to wait and retry when Slack rate-limits (default 0: fail at once). |
| `SLACKCLI_KEYCHAIN_TIMEOUT_MS` | Time limit of a macOS keychain call (default 3000). |
| `SLACKCLI_DEBUG` | Set to 1 to trace every Slack call and wait on stderr, with durations (see [Output](#output)). |
| `SLACKCLI_AUDIT_LOG` | Where the audit log of every request to Slack is written (default `~/.config/slackcli/audit.jsonl`); `off` turns it off (see [Audit log](#audit-log)). |
| `CI` | When set, `message compose` sends the text without opening the editor. |

## Coming from agent-slack

The workspaces saved by agent-slack are copied over on the first run, and the old files are left untouched. `AGENT_SLACK_*` variables are still read when the `SLACKCLI_*` one is unset.

`message send <target> --text "…"` still works: `--text` is taken for the positional text.

## Development

```bash
go build ./...
go vet ./...
go test ./...
```

Tests never talk to Slack. One package per command lives in `internal/cmd`, the Slack logic in `internal/slack`, and the API client in `internal/slackapi`.

## License

MIT, see [LICENSE](LICENSE). Derived from [stablyai/agent-slack](https://github.com/stablyai/agent-slack), copyright (c) 2026 Stably.
