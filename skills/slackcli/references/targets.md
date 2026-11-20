# Target selection

Prefer a Slack message URL whenever one is available, and quote it: the shell cuts an unquoted URL at `&`, silently dropping part of the query.

```text
https://<workspace>.slack.com/archives/<channel_id>/p<digits>[?thread_ts=...]
```

A message URL supplies the workspace, channel and message timestamp, and with `?thread_ts=` the thread. Flags that repeat what the URL says (`--ts`, `--thread-ts`, `--workspace`) are accepted; flags that contradict it are an error.

- `message get`, `edit`, `delete`, `react add/remove` and `later save/complete/archive/reopen/remove/remind` act on that message.
- `message list` returns the whole thread the message belongs to.
- `message send`, `message compose` and `message draft create` reply in that message's thread; so does `message draft update --channel <url>`.
- `channel mark` moves the read cursor to that message; here `--ts` overrides the URL's timestamp.

A Slack URL that points at a channel rather than a message is a channel target. Rejected: hosts other than `*.slack.com` / `*.slack-gov.com`, a URL with a port, and a `thread_ts` that is not a timestamp.

Use a channel (`#name`, `name`, or a `C...`/`G...`/`D...` ID) when there is no URL:

- `message get`, `edit`, `delete`, `react`, `channel mark` and the `later` write commands require `--ts <seconds>.<micros>`.
- `message list` reads channel history, unless `--ts` or `--thread-ts` selects a thread (both given must be equal). `--oldest` must be before `--latest`.
- `message send` posts to the channel; add `--thread-ts` to reply.

A user ID (`U...` or `W...`, equivalent) means the direct message with that user:

- `message get` (with `--ts`) and `message list` read the DM that exists with that user. They open none: without one they fail with "there is no direct message with user …".
- `message send`, `message compose`, `message draft create`, `message draft update --channel` and `message scheduled list/cancel --channel` write to it, opening it if need be.
- `message edit/delete/react`, `channel mark` and the `later` write commands reject it: pass a message URL, or the DM's channel ID (`D…`, the `channel_id` that `message list U…` prints) with `--ts`.

A `@handle` is not a target: get the ID with `user get <handle>`, or with `user search <words>` when you only know part of the name. For a group DM, `user dm-open <users...>` (2 to 8 other users; IDs or emails are fastest) prints its `dm_channel_id`; it is a write, as it opens the conversation.

Other argument kinds:

- `<canvas>`: a canvas URL (`https://<workspace>.slack.com/docs/T.../F...`), a channel-canvas URL (`https://<workspace>.slack.com/canvas/C...`) or a canvas ID (`F...`).
- `<trigger>` for `workflow preview/run`: a trigger ID (`Ft...`, from `workflow list`) or a shortcut URL (`https://slack.com/shortcuts/Ft.../...`). `workflow get` also takes a workflow ID (`Wf...`). `workflow run` requires `--channel`.
- Users (`user get`, `--user`, `--users`): a user ID, an email, `@handle`/`handle`; `user get`, `user dm-open` and `search --user` also take an exact display name. Handles and names scan the directory: prefer IDs or emails.

## Workspaces

Targets that are not workspace URLs carry no workspace. The workspace is then `--workspace <url-or-unique-substring>`, else `SLACK_WORKSPACE_URL`, else the default workspace (`auth whoami` shows it; `auth set-default` changes it; importing several workspaces at once sets none). With several workspaces configured and no default, a channel name without `--workspace` is rejected as ambiguous. Pass `--workspace` for channel, user, canvas and workflow IDs that are not in the default workspace.
