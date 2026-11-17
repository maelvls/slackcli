# slackcli

Slack automation CLI for AI agents: a single static Go binary that reads, searches and writes Slack as you.

- **Token-efficient**: results are compact JSON with `null`, blank and empty fields pruned, so an LLM can consume them cheaply.
- **Zero-config auth**: credentials are read from Slack Desktop, with Chrome, Brave and Firefox as fallbacks. Standard Slack tokens work too.
- **Human-in-the-loop**: `message compose` opens an editor in your browser where a person reviews and sends; safe mode makes that review mandatory.

slackcli is a Go rewrite of [stablyai/agent-slack](https://github.com/stablyai/agent-slack) (MIT). See [Migrating from agent-slack](#migrating-from-agent-slack).

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/maelvls/slackcli/main/install.sh | sh
```

The script downloads the release archive for your machine, verifies its SHA-256 against the release's `checksums.txt` (this cannot be skipped) and installs the binary into `~/.local/bin`. Set `PREFIX` to install into `$PREFIX/bin`, `SLACKCLI_INSTALL_DIR` to choose the directory, `SLACKCLI_VERSION=v1.2.3` to pin a release, and `SLACKCLI_REPO=owner/name` to download from another GitHub repository (a fork).

Alternatives:

```bash
go install github.com/maelvls/slackcli/cmd/slackcli@latest
```

or download `slackcli_<darwin|linux|windows>_<amd64|arm64>.tar.gz` (`.zip` on Windows) and `checksums.txt` from the [GitHub releases](https://github.com/maelvls/slackcli/releases).

## Quick start

```bash
slackcli auth import-desktop     # or just run a command: credentials are imported on the fly
slackcli auth test

slackcli message get "https://acme.slack.com/archives/C0123ABCDEF/p1770165109628379"
slackcli message list "#general" --limit 20
slackcli search messages "deploy failed" --after 2026-01-01
slackcli message send "#general" "Hello from slackcli"
```

`slackcli --help` and `slackcli <command> --help` are the reference for every command, flag and output; this README only gives the map.

## Agent skill

The repository ships an agent skill in `skills/slackcli/` (Claude Code, Codex and other agents that read `SKILL.md`). From a checkout, install it into `~/.claude/skills` and `~/.agents/skills` with:

```bash
bash ./scripts/install-skill.sh
```

## Authentication

Credentials are looked up in this order:

1. The `SLACK_TOKEN` environment variable (with `SLACK_COOKIE_D` when it is an `xoxc-` browser token).
2. The workspaces saved by the `auth` commands.
3. Slack Desktop (macOS, Linux, Windows), then Chrome and Brave (macOS), then Firefox (macOS, Linux), imported on the fly and saved.

When saved browser credentials have expired, they are re-imported from Slack Desktop once and the command is retried.

Manual imports and checks:

```bash
slackcli auth whoami            # configured workspaces, default, redacted tokens (local only)
slackcli auth test              # calls Slack's auth.test
slackcli auth import-desktop
slackcli auth import-chrome     # also import-brave; needs "Allow JavaScript from Apple Events"
slackcli auth import-firefox
pbpaste | slackcli auth parse-curl   # a Slack API request copied as cURL, read from stdin
slackcli auth set-default acme
slackcli auth remove acme
```

Save a token without putting it on the command line:

```bash
SLACK_TOKEN=xoxb-... slackcli auth add --workspace-url https://acme.slack.com --from-env
```

`auth add` prints what was saved, for which workspace, and which workspace is now the default. It refuses a value that is not a Slack token (or cookie), and a `--workspace-url` that is not an https Slack workspace URL.

Or use environment variables only:

```bash
export SLACK_TOKEN="xoxc-..."                        # browser token
export SLACK_COOKIE_D="xoxd-..."                     # the browser's "d" cookie
export SLACK_WORKSPACE_URL="https://acme.slack.com"  # required with a browser token unless the target is a Slack URL
```

Credentials are only sent over https to Slack hosts (`*.slack.com`, `*.slack-gov.com`), and API calls never follow redirects. See [Environment variables](#environment-variables) for the values these variables accept.

### Browser tokens and standard tokens

| | Browser session (`xoxc-` token + `d` cookie) | Standard token (`xoxp-` user, `xoxb-` bot) |
| --- | --- | --- |
| Source | Slack Desktop or a browser, imported automatically | Created in a Slack app; `SLACK_TOKEN` or `auth add` |
| Acts as | You, with everything your Slack client can do | The app, limited to its scopes |
| Required for | `later`, `unreads`, `message draft`, `channel list --via-counts` | `canvas edit`, `canvas create --channel` (scope `canvases:write`) |
| Cannot do | `canvas edit`, `canvas create --channel` | The commands in the cell on the left; a bot token (`xoxb-`) also cannot use Slack's search API (`search --channel` still works) |

Commands that need a browser session use Slack's internal, undocumented client endpoints: they act only as you, but their behaviour may change without notice.

## Command map

```text
slackcli
├── auth
│   ├── whoami | test | add | set-default <workspace> | remove <workspace>
│   ├── import-desktop | import-chrome | import-brave | import-firefox
│   └── parse-curl
├── message [<target>]              # short for "message get <target>"
│   ├── get     <target>            # one message (+ thread summary)
│   ├── list    <target>            # channel history, or a whole thread
│   ├── send    <target> [text]     # send, reply or schedule; --attach, --blocks
│   ├── edit    <target> <text>
│   ├── delete  <target>
│   ├── react   add|remove <target> <emoji>
│   ├── scheduled [list] | cancel <id>
│   ├── compose <target> [text]     # browser editor; a human sends
│   └── draft   list | create <target> <text> | update <draft-id> <text> | delete <draft-id>
├── search  all|messages|files <query>
├── channel list | new | invite | mark <target>
├── user    list | get <user> | dm-open <users...>
├── later   [list] | save|complete|archive|reopen|remove|remind <target>
├── unreads
├── canvas  create | get <canvas> | edit <canvas>
├── workflow list <channel> | preview <trigger> | get <id> | run <trigger>
└── update  [--check]
```

Global flag: `--safe-mode`. Most commands take `--workspace`. `slackcli --version` (`-V` or `-v`) prints the version. `slackcli completion bash|zsh|fish|powershell` prints a shell completion script (the command is not listed in the help).

## Targets

Most commands take a `<target>`:

- **Message URL** `https://<workspace>.slack.com/archives/<channel-id>/p<digits>[?thread_ts=...]`: names the workspace, the channel and the message. Prefer it. Quote it: an unquoted `&` is cut by the shell.
- **Channel**: `#general`, `general`, or an ID (`C...`, `G...`, `D...`). Commands that act on one message then need `--ts`.
- **User ID** (`U...`/`W...`): the direct message with that user. Accepted by `message send`, `message compose`, `message draft create`, and the `--channel` flag of `message draft update` and `message scheduled`.

```bash
slackcli message get "#general" --ts 1770165109.628379
slackcli message list "#general" --thread-ts 1770165109.000001
```

With several workspaces configured, a channel name is ambiguous: pass `--workspace <url-or-unique-substring>`, set `SLACK_WORKSPACE_URL`, or run `slackcli auth set-default`. A Slack URL target names its own workspace.

A flag that repeats what a URL target already says (`--workspace`, `--ts`, `--thread-ts`) is accepted; one that contradicts it is an error. The exception is `channel mark`, where `--ts` overrides the timestamp of the URL.

## Examples

### Read

```bash
# One message, with a thread summary if it has a thread
slackcli message get "https://acme.slack.com/archives/C0123ABCDEF/p1770165109628379"

# The whole thread that message belongs to
slackcli message list "https://acme.slack.com/archives/C0123ABCDEF/p1770165109628379"

# Recent channel history (top-level messages only, at most 1000 per call)
slackcli message list "#general" --limit 50 --resolve-users

# Channel history filtered by reaction (needs --oldest to bound the scan)
slackcli message list "#general" --with-reaction eyes --oldest 1770165109.000000

# Inbox: every conversation with unread messages (nothing is marked as read)
slackcli unreads --max-messages 5
```

Files attached to messages are downloaded and listed with their absolute local `path`.

### Search

```bash
# Slack's search API, with its operators
slackcli search messages 'in:#alerts "smoke tests failed"' --after 2026-01-01 --before 2026-02-01

# Scan specific channels and match locally (works with any token)
slackcli search all "rollback" --channel "#alerts,#deploys" --after 2026-01-01

# Files, without downloading them
slackcli search files "roadmap" --content-type image --no-download
```

`--after` and `--before` are exclusive days. When more results match than `--limit`, the output has `more_messages` / `more_files` with a hint on how to continue.

### Write

```bash
slackcli message send "https://acme.slack.com/archives/C0123ABCDEF/p1770165109628379" "I can take this."
slackcli message send "#general" "Report attached" --attach ./report.md --attach ./chart.png   # one message, both files
slackcli message send "#announcements" "Deploy starts at 6pm." --schedule "2026-06-15T18:00:00-07:00"
slackcli message send U0123ABCDEF "Heads up before standup" --schedule-in "monday 9am"
slackcli message send "#general" "fallback text" --blocks ./blocks.json
slackcli message edit "#general" "Updated text" --ts 1770165109.628379
slackcli message react add "https://acme.slack.com/archives/C0123ABCDEF/p1770165109628379" eyes
slackcli message scheduled list
slackcli message scheduled cancel Q1234ABCD --channel C0123ABCDEF
```

Text is Slack mrkdwn; bullet and numbered lists are sent as real Slack lists. Text of several words may start with a dash (`- item`, `-1 for me`); a single word that looks like a flag (`-1`, `--force`) must come after `--`, as in `slackcli message send "#general" -- "-1"`. This holds for `message send`, `edit`, `compose` and `draft create/update`.

### Compose and drafts

```bash
# Open the editor in the browser, pre-filled; a human edits, then sends or cancels
slackcli message compose "#general" "Here's my update"

# Create a Slack-native draft: nothing is posted, it appears in the user's Slack client
slackcli message draft create "#general" "Here's my update" --attach ./q3.png
slackcli message draft list
```

`message compose` waits for the human (no timeout) and prints what was sent (`ts`, `permalink`, `text`) or `{ok, cancelled, reason}`. The editor only works from a real browser session: when no browser can be opened, the command prints a one-time link only if stderr is an interactive terminal, and fails otherwise; in safe mode it always fails and never prints the link. With a message URL target, `--thread-ts` is redundant: it may repeat the thread of that message but not name another one (`message send` and `message draft create/update` apply the same rule). When the `CI` environment variable is set, the editor is skipped and the supplied text is sent immediately (except in safe mode, where the command fails).

### Channels and users

```bash
slackcli channel list --limit 50
slackcli channel new --name incident-war-room --private
slackcli channel invite --channel incident-war-room --users "U0123ABCDEF,@alice,bob@example.com"
slackcli channel mark "https://acme.slack.com/archives/C0123ABCDEF/p1770165109628379"
slackcli user get alice@example.com
slackcli user dm-open @alice @bob
```

`channel invite` tries every target, prints what happened to each (`invited_user_ids`, `already_in_channel_user_ids`, `unresolved_users`, `failed`) and exits non-zero when any target was unresolved or refused.

### Later, canvases, workflows

```bash
slackcli later list --state all
slackcli later remind "https://acme.slack.com/archives/C0123ABCDEF/p1770165109628379" --in "tomorrow at 9:30am"

slackcli canvas get "https://acme.slack.com/docs/T0123ABCD/F0123ABCDEF"
generate-report | slackcli canvas create --file - --title "Weekly report"
slackcli canvas edit F0123ABCDEF --operation insert_at_end --markdown "- [ ] One more thing"

slackcli workflow list "#helpdesk"
slackcli workflow get Ft0123ABCDEF
slackcli workflow run Ft0123ABCDEF --channel "#helpdesk"   # starts the workflow: not idempotent
```

## Safe mode

Skill instructions such as "never send without asking" are guidance an agent can ignore. Safe mode enforces human review in the tool itself:

```bash
export SLACKCLI_SAFE_MODE=1                       # anything but empty, 0, false, no or off
slackcli --safe-mode message send "#general" "hello"
```

While safe mode is on:

| Command | Behaviour |
| --- | --- |
| `message send` | Nothing is posted: the compose editor opens with the text for a human to send. The output adds `"safe_mode": true` and `"redirected_from": "send"`. `--attach`, `--blocks`, `--schedule`, `--schedule-in` and `--reply-broadcast` are refused, and so is running under `CI`. |
| `message compose` | Unchanged, except that it fails instead of sending under `CI`, and fails when no browser can be opened: the editor link, which lets whoever holds it send, is never printed in safe mode. A human must then run `slackcli message compose` themselves in a graphical session. |

Refused in safe mode: `message edit`, `message delete`, `message scheduled cancel`, `canvas create`, `canvas edit`, `workflow run`, `channel new` and `channel invite`.

Not covered by safe mode: `message react`, `message draft *`, `channel mark`, `later *` and `user dm-open` (the asterisk stands for every subcommand). These only change your own state (a reaction, an unposted draft, a read marker, the saved-for-later list, an open DM) and still take effect immediately.

Safe mode fails closed: the flag and the environment can each turn it on, and neither can turn it off for the other (`--safe-mode=false` does not undo `SLACKCLI_SAFE_MODE=1`). A refusal names what turned safe mode on.

## Output conventions

- Results are JSON on stdout, indented with two spaces, with `null`, blank strings, empty arrays and empty objects removed. Hints and warnings go to stderr.
- The collection a list command is about is the exception: it is always present, `[]` when empty (`messages`, `files`, `channels`, `users`, `items`, `workflows`, `drafts`, `scheduled_messages`, and `workspaces` of `auth whoami`).
- Some `auth` commands (`add`, `set-default`, `remove`, `import-chrome`, `import-brave`, `import-firefox`, `parse-curl`) print a line of text instead.
- Messages are compact: `channel_id`, `ts`, `thread_ts`, `author` (`user_id` or `bot_id`), `content` (Markdown), `files`, `reactions`. User IDs are kept as is; `--resolve-users` adds their profiles under `referenced_users`.
- Long content is cut at `--max-body-chars` / `--max-content-chars`; lists are paged with `--limit` and `--cursor` (`next_cursor` in the output).
- Downloaded files are listed with an absolute `path`; a failed download keeps its entry with an `error`.

## Environment variables

| Variable | Effect |
| --- | --- |
| `SLACK_TOKEN` | Token used instead of the saved workspaces. It must be a Slack token: a browser token (`xoxc-`) or a standard one (`xoxb-`, `xoxp-`; also `xoxa-`, `xoxs-`, `xoxe.`, `xapp-`). Any other value is refused. |
| `SLACK_COOKIE_D` | The browser `d` cookie, required when `SLACK_TOKEN` is an `xoxc-` token. It must be an `xoxd-` value (`d=xoxd-...` and the percent-encoded form are accepted). `SLACK_COOKIE` is read as a fallback. |
| `SLACK_WORKSPACE_URL` | Default for `--workspace`: a workspace URL, or a unique substring of the URL or name of a saved workspace. With `SLACK_TOKEN`, this variable or `--workspace` must be an https Slack workspace URL or designate a saved workspace, otherwise the command fails. |
| `SLACKCLI_SAFE_MODE` | Any value other than empty, `0`, `false`, `no` or `off` turns safe mode on (it fails closed: an unrecognised value means on). |
| `SLACKCLI_COMMAND_TIMEOUT_MS` | Time limit of a whole command: a positive number of milliseconds (default 30000). Does not apply to `message compose`, `update`, or the commands that upload files (`message send --attach`, `message draft create`, `message draft update`). |
| `SLACKCLI_API_TIMEOUT_MS` | Time limit of one Slack API request: a positive number of milliseconds (default 20000). The older `SLACK_API_TIMEOUT_MS` is read as a fallback. |
| `SLACKCLI_RATE_LIMIT_MAX_WAIT_MS` | How long to wait and retry when Slack rate-limits a request: a number of milliseconds, 0 or more (default 0: fail at once). |
| `SLACKCLI_KEYCHAIN_TIMEOUT_MS` | Time limit of a macOS keychain call: a positive number of milliseconds (default 3000). |
| `SLACKCLI_NO_UPDATE_CHECK` | `1`, `true`, `yes` or `on` disables the background update notice. |
| `CI` | When set (other than `0`/`false`): no update notice, and `message compose` skips the editor. |
| `DISPLAY`, `WAYLAND_DISPLAY` | On Linux, `message compose` opens a browser only when one of them is set: without a graphical session there is no browser to open. |
| `HTTPS_PROXY`, `NO_PROXY` | Honoured for every HTTP request, as by any Go program. |
| `XDG_CONFIG_HOME` | Moves the configuration directory (absolute path). |
| `SLACKCLI_INSTALL_DIR`, `SLACKCLI_VERSION`, `SLACKCLI_REPO`, `PREFIX` | Read by `install.sh` only. |

A `SLACKCLI_*_MS` variable with a value it does not accept (not a number, negative, or `0` where a positive number is required) prints one warning on stderr, and the default applies. No value means "no time limit": `0` is not accepted as a timeout.

For the legacy prefix of the `SLACKCLI_*` variables, see [Migrating from agent-slack](#migrating-from-agent-slack).

## Files and locations

| What | Where |
| --- | --- |
| Credentials | `$XDG_CONFIG_HOME/slackcli/credentials.json`, else `~/.config/slackcli/credentials.json` (mode 0600), on every OS |
| Secrets on macOS | Login keychain, service `slackcli`; the credentials file then holds placeholders. A secret the keychain refuses (locked, prompt not answered) stays in the credentials file, with a warning; the next save that reaches the keychain moves it there; `auth whoami` reports where they are in `secrets_storage` (`keychain` or `file`). On Linux and Windows the secrets stay in the credentials file. |
| Lock file | `credentials.json.lock` next to the credentials file (briefly also `credentials.json.lock.takeover`), while a command saves credentials. One left behind by a dead process is removed automatically. |
| Cache (user cache, update-check state) | The OS user cache directory + `/slackcli`: `~/Library/Caches/slackcli` on macOS, `$XDG_CACHE_HOME/slackcli` or `~/.cache/slackcli` on Linux, `%LocalAppData%\slackcli` on Windows |
| Downloads (attachments, canvases) | `downloads/` inside the cache directory |

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | Error (the message is on stderr), usage errors included |
| 124 | The command exceeded `SLACKCLI_COMMAND_TIMEOUT_MS` |
| 130 | Interrupted (Ctrl-C or SIGTERM) |

- A usage error (wrong arguments, a bad flag, an unknown command or subcommand, `slackcli help <unknown>`, an argument after `--version`) ends with `Run "slackcli <command> --help" for usage.`
- After a timeout, or an interrupt of a command that writes, the message warns that the change may still have gone through: check before retrying.
- A Slack error is printed as `Slack API error calling <method>: <code>`. For the common codes (`invalid_auth`, `not_authed`, `token_revoked`, `token_expired`, `account_inactive`, `missing_scope`, `not_in_channel`, `channel_not_found`, `ratelimited`) the message goes on to say what to do.

## Self-update

```bash
slackcli update --check   # only report
slackcli update           # download, verify the checksum, replace the binary
```

Only release binaries (installed with `install.sh` or downloaded by hand) replace themselves; for other installations the command fails and prints the command to run instead. On a build without a version (built from source), `update --check` reports `"status": "development_build"` without asking GitHub. slackcli also checks for a new release in the background, at most once every 24 hours, only when stderr is a terminal and never when `CI` is set; `SLACKCLI_NO_UPDATE_CHECK=1` disables it.

## Migrating from agent-slack

Automatic:

- **Credentials**: the workspaces in `~/.config/agent-slack/credentials.json` (and the `agent-slack` keychain service on macOS) that slackcli does not have yet are copied over, once. The credentials file records that it was done (`legacy_migrated`); when it cannot be done (keychain locked, directory not writable), a warning is printed and the next run tries again. The old files are left untouched.
- **Environment variables**: `AGENT_SLACK_*` variables are still read when the `SLACKCLI_*` one is unset. Safe mode is the exception: `SLACKCLI_SAFE_MODE` and `AGENT_SLACK_SAFE_MODE` each turn it on, and neither can turn it off for the other.

Changed:

- The binary is `slackcli`; environment variables use the `SLACKCLI_` prefix.
- Distribution: release binaries, `install.sh` and `go install` only. There is no npm package and no Nix flake any more.
- Downloads and caches moved from `~/.agent-slack` to the OS cache directory (see [Files and locations](#files-and-locations)).
- Safe mode also blocks `canvas create`, `canvas edit`, `workflow run`, `channel new`, `channel invite` and `message scheduled cancel`, and never prints the compose editor link.
- `message send <message-url> --thread-ts` and `message compose <message-url> --thread-ts` accept the thread the URL designates and refuse another one.
- List commands always print their collection, `[]` when empty. `--limit` of `message draft list` and `message scheduled list` is an integer flag like the others.
- `message send --attach a --attach b` posts one message with all the files. Message text may start with `- `.
- `message list --limit` goes up to 1000; a thread listing rejects history-only flags (`--limit`, `--oldest`, `--latest`, reaction filters). Numeric flags are validated everywhere.
- `search --channel` is repeatable or comma-separated (not space-separated); `--after`/`--before` are exclusive; `--no-download` is new; `more_messages`/`more_files` say how to continue. `search files` keeps files whose download failed, with an `error`.
- `message compose` accepts user-ID targets, prints `ts`, `permalink` and the `text` that was sent, and fails rather than print its link when no browser can be opened and stderr is not a terminal.
- `channel invite` reports `failed` targets and exits non-zero on partial failure.
- `auth whoami` shows `default`, `secrets` and `secrets_storage` per workspace; `auth add --from-env` is new.
- `canvas get` reports `truncated`/`total_chars`; `canvas create/edit --file -` reads stdin; channel-canvas URLs are accepted.
- `workflow preview/get/run` accept shortcut URLs.
- `later list --cursor` pages; `later` write commands print `{ok, action, channel_id, ts}`; `unreads` adds `messages_truncated` and `messages_error`.

## Development

```bash
make build   # static binary in build/
make test    # go test ./...
make lint    # go vet + gofmt check
```

Releases are cut by pushing a `v*` tag; see [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT, see [LICENSE](LICENSE). slackcli is derived from [stablyai/agent-slack](https://github.com/stablyai/agent-slack), copyright (c) 2026 Stably.
