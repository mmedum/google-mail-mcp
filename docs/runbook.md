# Runbook

What to do when something about credentials or the mailbox surprises
you. Setting up in the first place is [`gcp-setup.md`](gcp-setup.md).

Everything here is local to one machine and one profile. The whole of
this server's persistent state is a refresh token and a small non-secret
profile file.

## Where the state lives

| What | Where | Secret |
|---|---|---|
| Refresh token | OS keyring, service `google-mail-mcp`, account = the profile name | Yes |
| Refresh token, no keyring | `token.json` in the profile's config directory, restricted to you | Yes |
| Profile | `config.json`: client secret path, token store, granted scopes | No |
| OAuth client JSON | Wherever you downloaded it; the path is in the profile | No |

`google-mail-mcp status` prints the config directory, the signed-in
account (masked) and which token store is in use. Run it first.

## `[auth]` about a week after a working login

Your OAuth client is External and in Testing, and Google expires refresh
tokens issued to such clients after seven days. It is not a bug and
nothing is wrong with the token store. Run `google-mail-mcp login` again,
or move the client to Internal if your account is in a Workspace
organization (`gcp-setup.md` §2).

## `[auth]` naming a scope

The token was granted with a different configuration. Changing
`GMAIL_READ_ONLY`, `GMAIL_ENABLE_DESTRUCTIVE` or `GMAIL_ENABLE_SETTINGS`
changes the scopes, and
an existing token does not gain one. Run `login` again with the same
setting, and accept every scope asked for. `doctor` lists granted against
wanted, and the server warns at startup.

## `doctor` says the token is wider than needed

The login granted more than the configuration now uses — typically
`https://mail.google.com/` left over after `GMAIL_ENABLE_DESTRUCTIVE` was
turned off, or `gmail.settings.basic` after `GMAIL_ENABLE_SETTINGS` was.
Everything works; the tools that scope covered are simply not registered. To hold only what the configuration needs, run
`google-mail-mcp logout`, which revokes the token at Google, then
`google-mail-mcp login`.

## The token is in a file, not the keyring

`status` says `file`, and every run warns. The keyring was unavailable
when you logged in — typically a headless Linux machine with no Secret
Service. Start one (or log in from a desktop session) and run `login`
again; the token moves to the keyring and the file is removed.

## Rotating the token

```bash
google-mail-mcp login
```

The new token replaces the old one in the same store. An account holds at
most 100 refresh tokens per OAuth client, oldest invalidated first, so a
script that logs in repeatedly eventually logs out your other machines.

## Revoking access

```bash
google-mail-mcp logout
```

This revokes the token at Google and deletes the local copy. Revoking at
Google ends every token the account granted to the Cloud project, on
every machine and for every OAuth client in that project. `logout`
names any other profile of the same account in that project first.

## Suspected exposure

If a refresh token may have leaked — a copied home directory, a shared
machine, a `token.json` in a backup — treat it as **access to send mail
as you**, whatever this server's flags say: the default scope can send.

1. `google-mail-mcp logout` on any machine that still has it; this
   revokes the grant everywhere.
2. If you cannot, revoke at <https://myaccount.google.com/permissions>.
3. Check Gmail's Sent folder, **Settings → Forwarding and POP/IMAP**,
   **Filters**, the vacation reply and signatures for anything you did not
   set up.
4. Rotate the OAuth client secret in the Cloud console if the client JSON
   was exposed too.

## A draft did not land in the thread

The reply was built by the server from the message named in `reply_to`,
or the newest message of the thread named in `reply_to_thread`, and the
result says whether Gmail put it in that thread. If it did not, the
result names the message it replied to; check that it was the one you
meant, and that the subject was not changed since — Gmail threads only
on a matching subject, and `update_draft` warns when a reply's subject
changes. A parent with no `Message-ID` header cannot be answered with
the headers Gmail threads by; the result says so.

## `[stale]` from `update_draft`

The draft was saved since the `message_id` you passed was read: in
Gmail, in another client, or by an earlier call. Read it again with
`get_draft`, check what changed, and apply your change to that. Every
save gives a draft a new message id, and each result names the current
one.

## `list_changes` says the cursor expired

Gmail keeps history for at least a week and sometimes much less. Changes
between the old cursor and now cannot be recovered through this API. The
result carries the current `history_id`; start again from it, and search
for anything you needed from the gap.

## `[rate_limited]`

Two different limits share this class, and the message says which:

- **Request quota** — 6,000 units per user per minute. The server spends
  within it and waits rather than failing, so seeing this means another
  client is using the same project. Wait a minute.
- **Sending limit** — hundreds of messages or recipients a day. Google
  can report it minutes after the send that crossed it. Wait up to a day.

## `[blocked]` from `send_draft`

The draft reaches someone the call did not vouch for. Every recipient who
is not already on a message of the thread the draft answers — for a new
conversation, every recipient — must be written out in
`confirm_recipients`. The refusal names them by field and position,
`cc[1]`; `send_draft` with `dry_run: true` lists the addresses. A draft
with more than 50 recipients is refused outright: send it from Gmail.

## `[ambiguous_outcome]` from `send_draft`

The request left, and the answer did not come back. The server has
already read the mailbox to settle it, and the result says **sent**,
**not sent** or **unknown**. Do not send again on **unknown**; look at
Sent in Gmail first. The server never resends by itself.

## Do the restricted scopes mean a security assessment?

Only if you publish an OAuth app to the public. For an Internal client in
your own Workspace, or an External client in Testing with yourself as a
test user, there is nothing to do beyond the unverified-app warning.
Publishing externally with restricted scopes needs Google's verification
and an annual third-party security assessment.

## Reporting a problem

Include `google-mail-mcp doctor`, `google-mail-mcp --version`, how you
installed it and which client you use. For a failing call, turn the log
level up first:

```bash
GMAIL_LOG_LEVEL=debug google-mail-mcp doctor
```

Logs carry no message content, addresses, labels or queries, and ids are
truncated, so a debug log is safe to paste. **Do not paste a tool
result**: it is mail, and carries other people's words and addresses.
Describe it instead.
