# JSON output and downloads

Results are JSON on stdout, on one line (indented when stdout is a terminal). `null`, blank strings, `[]` and `{}` are pruned, so test for a field's presence. One exception: the collection a list command is about is always present, `[]` when empty, so it can be iterated without a presence test: `messages` (`message list`, `search messages|all`), `files` (`search files|all`), `channels` (`channel list`, `unreads`), `users` (`user list`), `items` (`later list`, not with `--counts-only`), `workflows` (`workflow list`), `drafts` (`message draft list`), `scheduled_messages` (`message scheduled list`), `workspaces` (`auth whoami`; its `environment.error` says why `SLACK_TOKEN` cannot be used). Exceptions that print a line of text: `auth add`, `auth set-default`, `auth remove`, `auth import-chrome|import-brave|import-firefox`, `auth parse-curl`. Exit codes: 0 success, 1 error (message on stderr), 124 command timeout, 130 interrupted. Each command's `--help` states its exact output.

## Warnings, hints and stderr

Any JSON result can end with `warnings` and `hints`, two arrays of sentences: `warnings` say what went wrong without failing the command (a file that could not be downloaded, users or threads left out because Slack rate limited, an environment variable that was ignored); `hints` say what to do about the result (why a list is empty, how to get the rest of a cut canvas). A command that succeeds and prints JSON writes nothing on stderr, `message send` redirected by safe mode included (its `hints` says it was redirected).

Stderr is for failures: the error message, as plain text, after any warning that came before it (`Warning: …`). Two exceptions on success: the commands that print a line of text (listed above) write their notes there, and `message compose` writes where its editor is while it waits for a human, only when stderr is a terminal. So never `2>&1 | jq`: on a failure jq only reports `parse error` and the reason is lost. Use `2>err.txt`, check the exit code, and `cat err.txt` when it is not 0.

## Messages

A message is `{channel_id, ts, time, thread_ts?, reply_count?, author: {user_id | bot_id, name?}, content, files?, reactions?, forwarded_threads?}`. `ts` identifies the message; `time` is the same instant as a local date and time (`2026-02-04T01:31:49+01:00`). `content` is Markdown, cut at `--max-body-chars`. `reactions` (`[{name, users, count?}]`) appear only with `--include-reactions`. A root message with replies carries its own `ts` as `thread_ts`, and `reply_count`. `author.name` is present in search results (the handle) and with `--resolve-users` (the display name).

- `message get`: `{message, thread?: {ts, length}}`. `thread` means the message belongs to a thread of `length` messages; read it with `message list` on the same target.
- `message list`: `{channel_id, messages, system_messages_hidden?}`, oldest first, in the same shape for channel history and for a thread (the root and all replies, each with `thread_ts`). With `--expand-threads`, the replies of each thread follow its root, so the list is in thread order rather than strictly chronological. `system_messages_hidden` counts the joins, leaves and topic changes left out of channel history (`--include-system` lists them).
- Search results: `messages[]` have `channel_id, channel_name?, ts, time, author, content, files, permalink` (`channel_name` is absent for a DM, and with `--channel` for a channel given by ID); `files[]` have `title, mimetype, mode, path, permalink` (the file's permalink, not a message's), or `external, url` in place of `path`. `more_messages` / `more_files` (`{total?, before?, hint}`) mean the list was cut: follow `hint`, usually by repeating the search with `--before <before>`.
- `unreads`: `channels[]` of `{channel_id, channel_name, channel_type, unread_count?, unread_count_is_lower_bound?, mention_count, messages?, system_messages_hidden?, messages_truncated?, messages_error?}` and `threads?: {has_unreads, mention_count}`. A message is `{ts, time, author, content, thread_ts?, reply_count?}`. `unread_count` includes the system messages that `messages` leaves out (`system_messages_hidden` of them; `--include-system` shows them), so a conversation can be unread with no `messages`. It is never 0, and absent when unknown (`--counts-only`, `messages_error`, only unread thread replies). It is exact unless `unread_count_is_lower_bound` is true. `messages_truncated` means more messages are unread than the `--max-messages` fetched (the oldest unread ones).
- `later list`: `{counts, items[], next_cursor?}`; an item has `channel_id, channel_name, ts, state, date_saved, date_completed?, date_due?` and `message` or `message_error`.

- `channel list`: `{channels, next_cursor?}`; a channel is `{id, name?, user?, is_private?, is_im?, is_mpim?, is_archived?, num_members?}`: a flag is present only when true, and a DM has `user` (the other member) instead of `name`. `--raw` prints Slack's conversation objects instead (topic, purpose, creator …). With `--name`, the whole listing was read: there is no `next_cursor`.

User IDs in `content` and `reactions` are left as is. `--resolve-users` puts the display name of each author in `author.name` and adds `referenced_users`, a map from user ID to `{id, name, real_name, display_name, email, title, tz, ...}` covering the users mentioned too, served from a 24-hour per-workspace cache; `--refresh-users` fetches again.

Paged lists return `next_cursor` when there is more: pass it to `--cursor` with the same other flags.

## Writes

- `message send`: `{ok, channel_id, ts, thread_ts?, permalink?}`. `permalink` is missing only when the token names no workspace and Slack would not give it; `warnings` says so, and the message was sent: do not send it again. With `--schedule`/`--schedule-in`: `{ok, channel_id, scheduled_message_id, post_at, thread_ts?}` (keep `channel_id` and `scheduled_message_id` for `message scheduled cancel`). With `--attach`: `{ok, channel_id, thread_ts?, files: [{id, name, size}]}`, without `ts` or `permalink`.
- `message compose`: `{ok, sent, channel_id, ts, thread_ts?, permalink?, text}` (`permalink` as for `send`) where `text` is what the human actually sent, or `{ok, cancelled, reason}` when nothing was sent. `ok: true` alone does not mean sent: check `sent`. Skipped in CI: `editor: "skipped"` and no `text`. A `message send` redirected by safe mode prints the same plus `safe_mode: true, redirected_from: "send"`.
- `message edit`, `delete`, `react add/remove`: `{ok: true}`. `channel mark`: `{ok, channel_id, ts}`.
- `message scheduled list`: `{ok, scheduled_messages: [{id, channel_id, post_at, date_created, text}], next_cursor?}`; `cancel`: `{ok, channel_id, scheduled_message_id}`.
- `message draft list`: `{ok, drafts, count}`; `create`/`update`: `{ok, draft}`; `delete`: `{ok, draft_id}`. A draft is `{id, text, destinations: [{channel_id, thread_ts?, broadcast?, channel_name?}], last_updated_ts, date_created, date_scheduled?, file_ids?}`.
- `channel invite`: `{channel_id, invited_user_ids, already_in_channel_user_ids, unresolved_users, failed: [{target, error}]}` (with `--external`: `invited_emails`, `already_invited_emails`, `failed`). Non-zero exit when anything is in `unresolved_users` or `failed`; the JSON is still printed.
- `later save|complete|archive|reopen|remove`: `{ok, action, channel_id, ts}`; `remind` adds `remind_at`, `remind_at_local`.
- `canvas create`: `{canvas: {id, title?, channel_id?}}`; `canvas edit`: `{ok, canvas: {id, operation}}`; `canvas get`: `{canvas: {id, title?, markdown, truncated?, total_chars?}}`. When `truncated` is true the Markdown was cut at `--max-chars`: pass `--max-chars -1` for all of it.
- `workflow run`: `{function_execution_id, trigger_execution_id, is_slow_workflow}`: Slack accepted the trigger, which says nothing about whether the workflow's steps succeeded.

## Downloaded files

`message get`, `message list` and `search` download attached files into the `downloads` directory of the OS cache directory (`~/Library/Caches/slackcli/downloads` on macOS, `$XDG_CACHE_HOME/slackcli/downloads` or `~/.cache/slackcli/downloads` on Linux). Canvases attached to messages are saved as Markdown (`.md`).

- A downloaded file entry has an absolute `path` plus `name` (or `title` for `search files`), `mimetype` and `mode`.
- A failed download keeps its entry, with `error` and a `path` to a `<file-id>.download-error.txt` file holding the reason; `warnings` names the file too. This holds for `message get/list` and for every `search` command. Check for `error` before reading `path`.
- A file that Slack does not host (Google Docs, Slides, Drive, Dropbox …) is only a link: its entry has `external: true` and `url` (open that link), no `path`, and no warning.
- `search --no-download` lists files without `path`, and does not mark the external ones.
- For a download-then-reply task, use `search messages --content-type file`: its results carry the source message's `permalink`, which is a valid reply target.
