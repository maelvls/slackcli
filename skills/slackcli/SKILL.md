---
name: slackcli
description: "Slack CLI for agents: read message URLs/threads/channel history/unreads/inbox (who is waiting on the user)/Later/canvases/workflows, search messages and files, download attachments, look up users, list/create/invite channels, open DMs, create/edit canvases from Markdown, hand a message to a human in the compose editor, manage Slack-native drafts, schedule sends, and explicit sends/edits/deletes/reactions/mark-read/uploads."
---

# slackcli

Use `slackcli` from `$PATH`. If it is missing, tell the user: it is installed from a checkout of its repository with `go install ./cmd/slackcli`.

`slackcli --help` and each subcommand's `--help` are authoritative: they document every flag and the JSON each command prints. Read the relevant help before guessing. If a capability named here is absent from the installed help, report version skew instead of guessing.

## Safety

- Read and search freely.
- Perform a write only when explicitly requested: `message send/edit/delete`, `message react`, `message scheduled cancel`, `message draft create/update/delete`, uploads (`--attach`), `channel new/invite/mark`, `user dm-open`, `later save/complete/archive/reopen/remove/remind`, `canvas create/edit`, `workflow run`. `workflow run` is not idempotent and can trigger downstream actions: read `workflow get` first.
- For a compose- or review-only request, return the proposed text without invoking Slack, or use `message draft create`: it posts nothing and leaves a draft in the user's Slack client.
- `message compose <target> [text]` hands the text to a human: it opens an editor in their browser and blocks, with no timeout, until they send or cancel. Use it only when the user asks for the editor. You cannot send from it yourself; without a browser it fails unless stderr is an interactive terminal (in safe mode it always fails: tell the user to run it themselves). Exception: when the `CI` environment variable is set it skips the editor and sends `[text]` immediately, so in CI treat it as a send.
- Safe mode (`SLACKCLI_SAFE_MODE=1` or the global `--safe-mode`) is enforced by the tool: `message send` is redirected to the compose editor (and refuses `--attach`, `--blocks`, `--schedule`, `--schedule-in`, `--reply-broadcast`, CI, and a machine where no browser can be opened). Refused in safe mode: `message edit`, `message delete`, `message scheduled cancel`, `canvas create`, `canvas edit`, `workflow run`, `channel new` and `channel invite`. Not covered by safe mode: `message react`, `message draft *`, `channel mark`, `later *` and `user dm-open`; the rule above still applies to those. Do not turn safe mode off to get a write through; tell the user.

## Workflow

1. Run the command you need: credentials are imported on the fly from Slack Desktop or a browser. Only if it reports none, run `auth import-desktop` (or `import-chrome`, `import-brave`, `import-firefox`), then `auth test`. `auth whoami` (local, redacted) lists the workspaces and the default one.
2. Prefer a Slack message URL as the target: it carries workspace, channel and timestamp. Quote it.
3. Choose the narrowest read: `message get` for one message, `message list` for a thread or channel history, `search messages|files|all` for discovery, `unreads` for what is unread, `inbox` for who is waiting on an answer.
4. Bound the output with `--limit`, `--max-body-chars`, `--max-content-chars`, `--counts-only`, `--no-download`.
5. For a requested write, run only that mutation and check the JSON it prints.

## Common reads

One call each. Messages carry `time` (their `ts` as a local date and time); add `--resolve-users` when names matter: it puts `author.name` on each message (24-hour cache).

- What was said in a channel since a day: `message list "#name" --oldest 2026-01-30 --expand-threads --resolve-users` (`--latest` ends the window; `--expand-threads` adds the replies of each thread after its first message).
- One thread: `message list "<message-url>"`.
- The DM with someone: `user get <handle|email|name>` for their ID, then `message list U…`. It fails when no DM exists; nothing is opened.
- Who said what, anywhere: `search messages "<words> from:@handle in:#channel" --no-download`. Results carry `channel_name`, `time`, `author.name` and a `permalink` to pass to `message list`.
- A channel whose exact name you do not know: `channel list --name <text>` (add `--all` for channels you are not in). A known name needs no lookup: pass `#name`.
- What is unread: `unreads --counts-only`, then `message list` on what matters. Plain `unreads` prints up to 10 messages per conversation.
- Who is waiting on the user (read or not): `inbox` (`--since 12h`, `2d` or `2026-01-31`; 3 days by default). Each item is a DM (`dm`, `mpim`), an unanswered @mention (`mention`) or new replies in a thread the user posted in (`thread`), with the newest message (`ts`, `author`, `text`, `permalink`) and how many came since the user's last one (`count`, `first_ts`). Pass the `permalink` to `message list` for the thread. `truncated: true` means not everything was looked at: raise `--max-conversations` or shorten `--since`.

## Non-obvious behaviour

- Channel history (`message list <channel>`) has top-level messages only; a message with `reply_count` started a thread: list it, or pass `--expand-threads`. System messages (joins, leaves, topic changes) are left out and counted in `system_messages_hidden`; `--include-system` lists them. `--oldest`/`--latest` take a day (`2026-01-31`, from 00:00 local time), a date and time (`2026-01-31T14:30`) or a ts. Thread listings reject history-only flags (`--limit`, `--oldest`, `--latest`, `--expand-threads`, `--include-system`, reaction filters). Reaction filters require `--oldest`.
- Output is one line of JSON (indented only on a terminal). `channel list` prints `{id, name, user, is_private, is_im, is_mpim, is_archived, num_members}`; `--raw` prints Slack's own objects, twenty times larger.
- `search` without `--channel` uses Slack's search API (operators allowed; not available to bot tokens). With `--channel` (repeatable or comma-separated) the channels are scanned and matched locally: operators are rejected, thread replies are not seen, and it can be slow: bound it with `--after`. `--after`/`--before` are exclusive days.
- `later`, `unreads`, `message draft` and `channel list --via-counts` need browser credentials (xoxc + cookie); `inbox` needs search, so not a bot token. On an Enterprise Grid that restricts `client.counts`, `unreads` and `channel list --via-counts` read `users.counts` instead: `unreads` then has no `threads` and lists a muted conversation only when the user is mentioned in it. `canvas edit` and `canvas create --channel` need a standard token with `canvases:write`; browser credentials can only create standalone canvases.
- `message send` text is Slack mrkdwn; lists are converted to real Slack lists. Inside them write links as `<URL|label>`: CommonMark `[label](URL)` is not converted. `--blocks` replaces that conversion; with `--attach` the text is the files' plain comment. Several `--attach` flags make one message.
- Text that starts with a dash: `message send`, `edit`, `compose` and `draft create/update` take it as text when it has several words (`- item`, `-1 for me`). A single dash-word (`-1`, `--force`) is read as a flag: put it after `--`. For `message react`, write the `-1` emoji as `:-1:`.
- Scheduling: prefer `--schedule` with an ISO 8601 timestamp and explicit offset. `--schedule-in` day names, and `later remind --in` day names, use the local timezone of the machine running slackcli (9:00 by default): confirm it matches the user's intent or pass a Unix timestamp.
- `message draft update --attach` adds files; it never removes any. Drafts with a scheduled time or several destinations are refused.
- `canvas edit` applies one operation per call; the default `replace` without `--section-id` overwrites the whole canvas.
- `channel invite` exits non-zero on partial failure but still prints which invites went through: read the JSON before retrying.
- Commands are cut after 30 s (exit code 124; `SLACKCLI_COMMAND_TIMEOUT_MS`), except `message compose` and the uploading ones (`message send --attach`, `message draft create/update`). A rate-limited call fails at once unless `SLACKCLI_RATE_LIMIT_MAX_WAIT_MS` is set. After a timeout or an interrupt (exit code 130) of a write, the change may still have gone through: check before retrying.

## Conditional references

- Read [references/targets.md](references/targets.md) only when choosing between a message URL, channel or user target, or when several workspaces are configured.
- Read [references/output.md](references/output.md) only when handling returned message, search or write metadata, resolved users, or downloaded and failed attachments.
