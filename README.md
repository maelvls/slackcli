# slackcli

A command-line client for Slack, made to be driven by AI agents. It reads, searches and writes Slack as you, and prints compact JSON.

It is a Go rewrite of [stablyai/agent-slack](https://github.com/stablyai/agent-slack).

## Install

You need Go.

```bash
git clone https://github.com/maelvls/slackcli
cd slackcli
go install ./cmd/slackcli                 # into ~/go/bin
cp -R skills/slackcli ~/.claude/skills/   # optional: the agent skill
```

To update, pull and run the same commands again.

## Getting started

Open Slack Desktop and sign in, then:

```bash
slackcli auth test
slackcli message list "#general" --limit 20
slackcli message list "#general" --oldest 2026-01-30 --expand-threads --resolve-users
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
├── message   get | list | send | edit | delete | react add|remove |
│             scheduled list|cancel | compose | draft list|create|update|delete
├── search    all | messages | files
├── channel   list | new | invite | mark
├── user      list | get | dm-open
├── later     list | save | complete | archive | reopen | remove | remind
├── unreads
├── canvas    create | get | edit
└── workflow  list | preview | get | run
```

Most commands take a target:

- **A message URL**, such as `https://acme.slack.com/archives/C0123ABCDEF/p1770165109628379`. It names the workspace, the channel and the message, so prefer it. Quote it.
- **A channel**: `#general`, `general` or an ID. A command that acts on one message then also needs `--ts`.
- **A user ID** (`U…`), for a direct message. Reading never opens one: `message list U…` fails when you have no DM with that user.

With several workspaces saved, a channel name needs `--workspace <url-or-name>`, or a default set with `slackcli auth set-default`.

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

Your own session can do everything your Slack client can. An app token is limited to its scopes: it cannot use `later`, `unreads` or `message draft`, but it is the only way to run `canvas edit` and `canvas create --channel`.

Saved credentials live in `~/.config/slackcli/credentials.json`. On macOS the secrets are in the login keychain and the file only holds placeholders. Credentials are only ever sent to Slack, over https.

## Output

- Results are JSON on stdout, with empty fields removed: on one line, or indented when stdout is a terminal. Hints and warnings go to stderr.
- Messages have `ts`, which identifies them, and `time`, the same instant as a local date and time.
- A list command always prints its list, `[]` when empty, and `next_cursor` when there is more (pass it to `--cursor`).
- Files attached to messages are downloaded to the cache directory and listed with their local `path`. `--no-download` (`message get`, `message list`, `search`) lists them without downloading anything.
- Message content is cut at `--max-body-chars` (8000 characters by default, `-1` for no limit): the same flag and default on `message get`, `message list`, `search`, `unreads` and `later list`.

| Exit code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | Error, with the message on stderr |
| 124 | The command took longer than 30 seconds |
| 130 | Interrupted |

After a timeout or an interrupt, a command that writes may still have gone through: check before retrying.

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
| `CI` | When set, `message compose` sends the text without opening the editor. |

## Coming from agent-slack

The workspaces saved by agent-slack are copied over on the first run, and the old files are left untouched. `AGENT_SLACK_*` variables are still read when the `SLACKCLI_*` one is unset.

## Development

```bash
go build ./...
go vet ./...
go test ./...
```

Tests never talk to Slack. One package per command lives in `internal/cmd`, the Slack logic in `internal/slack`, and the API client in `internal/slackapi`.

## License

MIT, see [LICENSE](LICENSE). Derived from [stablyai/agent-slack](https://github.com/stablyai/agent-slack), copyright (c) 2026 Stably.
