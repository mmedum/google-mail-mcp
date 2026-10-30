# Configuration

Every setting is an environment variable named `GMAIL_*`, and most also
have a command-line flag of the same name in lower case with dashes. A
flag passed explicitly wins over the environment; the environment wins
over the default. A variable set to spaces alone is unset, as an empty
one is, and the spaces around a value are dropped. Configuration is
environment-first because MCP clients pass only `command`, `args` and
`env` to a stdio server.

Everything is validated once at start, and every problem is reported at
once rather than one per run.

## Settings

| Variable | Flag | Default | Meaning |
|---|---|---|---|
| `GMAIL_PROFILE` | `--profile` | `default` | Named profile. Each keeps its own client secret, refresh token and account, so one machine can hold several Google accounts. A name is up to 64 characters from a to z, 0 to 9, - and _, starting with a letter or digit; A to Z are read as a to z. |
| `GMAIL_CLIENT_SECRET` | `--client-secret` | the profile's `client_secret.json` | Path to the Desktop-app OAuth client JSON from the Cloud console. |
| `GMAIL_READ_ONLY` | `--read-only` | `false` | Register only the read tools, and ask for `gmail.readonly` at login. Refused together with any `ENABLE_` setting. |
| `GMAIL_ENABLE_SEND` | `--enable-send` | `false` | Register `send_draft`. Scopes do not change: the default scope can already send, which is why the tool, not the scope, is the control. |
| `GMAIL_ENABLE_DESTRUCTIVE` | `--enable-destructive` | `false` | Register `delete_permanently` and `delete_label`, and ask for `https://mail.google.com/` at login — the only scope Google accepts for permanent deletion. With `GMAIL_ENABLE_SETTINGS` as well, also `create_trash_filter` and `delete_filter`. Each call still needs `confirm: true`. |
| `GMAIL_ENABLE_SETTINGS` | `--enable-settings` | `false` | Register `update_signature` and `create_filter`, and ask for `gmail.settings.basic` at login — the only scope Google accepts for those writes. With `GMAIL_ENABLE_DESTRUCTIVE` as well, also `create_trash_filter` and `delete_filter`; with `GMAIL_ENABLE_SEND` as well, also `set_vacation`. |
| `GMAIL_DRAFT_SIGNATURE` | `--draft-signature` | `false` | Sign new drafts: when `create_draft` leaves `signature` out, the signature Gmail holds for the From address goes below a new draft's body, as Gmail's composer adds one to a new email. A reply or a forward is signed only with `signature: true`, and `signature: false` always leaves it out. A body that already holds the signature, or a plain-text-only draft, goes without it, and the result says why. Scopes do not change, and it has no effect with `GMAIL_READ_ONLY`, which registers no `create_draft`. |
| `GMAIL_LOCAL_DIR` | `--local-dir` | unset | The one directory attachments are written to, and read from to attach to a draft. An absolute path to a directory that exists. **Unset means no file transfer**: `download_attachment` and `download_attachments` are not registered, and `create_draft` and `update_draft` refuse attachments. |
| `GMAIL_LOG_LEVEL` | `--log-level` | `info` | `debug`, `info`, `warn` or `error`. Logs go to stderr. |
| `GMAIL_LOG_FORMAT` | `--log-format` | `text` | `text` or `json`. |
| `GMAIL_HTTP_TIMEOUT` | `--http-timeout` | `60s` | Deadline for one attempt at a Google API call. |
| `GMAIL_API_BASE` | `--api-base` | Google | The Gmail API base URL, for tests against a local fake. Plain `http` is refused unless it points at a loopback address, so a token cannot be sent in the clear. |

## Settings with no flag

Read from the environment only, because they are consulted before the
flags are parsed.

| Variable | Default | Meaning |
|---|---|---|
| `GMAIL_CONFIG_DIR` | `os.UserConfigDir()/google-mail-mcp` | Where profiles, the client secret and the fallback token file live. Must be inside your home directory. |
| `GMAIL_REFRESH_TOKEN` | unset | A refresh token supplied directly, for CI. It overrides the keyring and the token file. `logout` cannot revoke or remove it. |

## Where things are stored

```
$GMAIL_CONFIG_DIR/                     (default: ~/.config/google-mail-mcp)
  client_secret.json                   the OAuth client you downloaded
  config.json                          account, token location, scopes at login
  token.json                           refresh token, only if no keyring was available
  profiles/<name>/                     the same three files, per non-default profile
```

The refresh token goes to the OS keyring first. With none available, it
goes to `token.json`, restricted to the current user — `0600` on Unix, an
ACL on Windows — and the server warns on stderr every time it uses it.

## Scopes

| Configuration | Requested at login |
|---|---|
| `GMAIL_READ_ONLY=true` | `https://www.googleapis.com/auth/gmail.readonly` |
| default | `https://www.googleapis.com/auth/gmail.modify` |
| `GMAIL_ENABLE_SEND=true` | unchanged |
| `GMAIL_ENABLE_DESTRUCTIVE=true` | `https://mail.google.com/` |
| `GMAIL_ENABLE_SETTINGS=true` | adds `https://www.googleapis.com/auth/gmail.settings.basic` to the above |
| `GMAIL_DRAFT_SIGNATURE=true` | unchanged |

All are restricted scopes; `docs/gcp-setup.md` says what that means for
your consent screen.

## Logging in

`google-mail-mcp login` opens a browser for Google's consent screen and
stores the refresh token. It asks for consent only when the scopes it
needs are not already granted; `--consent` forces the screen, for when a
stored token has gone bad in a way the server cannot see. `--no-browser`
prints the URL instead of opening it, for SSH.

## Changing a setting that affects scopes

`GMAIL_READ_ONLY`, `GMAIL_ENABLE_DESTRUCTIVE` and `GMAIL_ENABLE_SETTINGS`
change the scopes, so they need `google-mail-mcp login` again. The server compares the granted
scopes with the ones it needs at startup and says so on stderr, and
`doctor` names any that are missing. Going the other way — turning
destructive or settings off — needs no new login, but the stored token keeps the
wider scope until you `logout` and `login`.

## Approving a write

Your MCP client approves each call; the server does not ask you itself.
What the client prompts for depends on the tool's annotations and on
your settings. claude.ai and Codex prompt for the tools annotated
destructive: `delete_draft`, `send_draft`, `set_vacation`,
`update_signature`, every filter write and every delete. VS Code prompts
for every tool that is not read-only. Claude Code prompts on every call
of `delete_permanently` and `delete_label`, whatever your rules say, and
for the rest follows your `permissions.allow` and `permissions.ask`
rules; the README gives `ask` rules for the writes that send, change a
setting or delete.

What holds whatever the client does is in the server: a tool behind a
setting does not exist until you turn the setting on; `confirm: true`
on every delete and on `create_trash_filter` and `set_vacation`, which
the model sets as a witness that it meant the call; every recipient of
a send written out in `confirm_recipients`; the draft's `message_id` on
`update_draft`, `send_draft` and `delete_draft`, refused `[stale]` when
the draft changed since it was read; the draft's subject and first line
on `send_draft`, and the filter's criteria on `delete_filter`, refused
`[stale]` when they are not what Gmail holds, so the call your client
shows you says what it sends or deletes; and `dry_run` on every write.

A client that runs with nobody watching, such as `claude -p` with this
server's tools allowed, makes every write that is registered. Leave
`GMAIL_ENABLE_SEND`, `GMAIL_ENABLE_DESTRUCTIVE` and
`GMAIL_ENABLE_SETTINGS` off for one.

## Removed settings

`GMAIL_REQUIRE_PROMPT` went with the server's question, and its
flag `--require-prompt` is gone, so a flag left in your client's
arguments stops the server from starting. The variable is still read,
only to refuse it: set to anything but `false`, its old default, the
server refuses to start, because ignoring it would quietly allow what
you set it to stop. Unset it, and leave the `ENABLE_` settings off for a
client that runs with nobody watching.

## Reading the setup from a script

`google-mail-mcp status` prints what is configured and where the token
is, for a person. `google-mail-mcp status --json` prints the same state
as one JSON object, for a launcher or health check. Neither contacts
Google; `doctor` is the one that asks Google whether the token works.

`credentials.resolved` is the field to branch on: `true` when a refresh
token was found, `false` when every tool will answer `[auth]` until
`login` succeeds. The account is masked to its domain. `schema_version`
changes only when a field is removed or changes meaning; fields may be
added within it.
