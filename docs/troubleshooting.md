# Troubleshooting

What to do when something does not work, then the maintenance tasks.
Start with these two commands:

```bash
google-mail-mcp status
google-mail-mcp doctor
```

`status` asks Google nothing. It prints the profile, the config
directory, the account (masked), where the token is stored, the scopes
granted and needed, and the switches in effect. `doctor` checks the
OAuth client JSON, the stored token, the granted scopes and whether the
Gmail API answers. Each failed check says what to do.

## `[auth] not signed in: run google-mail-mcp login`

The same cause shows as `credentials: no refresh token found`, and
`status` shows `token store: none`.

**Cause:** this profile has no refresh token. Nobody logged in with it,
or `logout` removed it.

**Fix (resolves it):** check that `status` shows the profile you expect,
then run `google-mail-mcp login`.

The server still starts and lists its tools; every call answers `[auth]`
until you log in.

## `[auth] the stored credentials could not be used`

It often appears about a week after a working login. `doctor` fails its
`access token` check the same way.

**Cause:** Google refuses the refresh token. On an External app in
Testing, every refresh token expires after seven days. Google also ends
one when you revoke access, or after six months unused, as
[Google's OAuth documentation](https://developers.google.com/identity/protocols/oauth2)
says under "Refresh token expiration".

**Fix:** run `google-mail-mcp login` again. For the seven-day expiry,
that is a workaround. An Internal app, open to a Workspace organization's
accounts, resolves it
([`docs/gcp-setup.md`](gcp-setup.md#2-oauth-consent-screen)).

## `[auth] the signed-in token lacks a scope … needs`

At startup the server warns `the configuration needs …, which the last
login was not granted`, and `doctor` fails `granted scopes`, naming each.

**Cause:** the token was granted under another configuration.
`GMAIL_READ_ONLY`, `GMAIL_ENABLE_DESTRUCTIVE` and `GMAIL_ENABLE_SETTINGS`
change the scopes, and a token never gains one. Google also lets you
untick scopes at consent.

**Fix (resolves it):** on an External app, add the scopes
[`docs/gcp-setup.md`](gcp-setup.md#2-oauth-consent-screen) lists for your
configuration. Then run `login` again and accept every scope. The tools
you did grant keep working meanwhile.

## `doctor` says `wider than this configuration needs`

**Cause:** the login granted more than the configuration uses, such as
`https://mail.google.com/` after `GMAIL_ENABLE_DESTRUCTIVE` was turned
off. The tools that scope covered are not registered.

**Fix (resolves it):** run `google-mail-mcp logout`, which revokes the
grant at Google, then `google-mail-mcp login`. Everything works without
this; it only narrows what the stored token can do.

## `doctor` fails `Gmail API (getProfile)`

It adds `if Google says the API is disabled, enable the Gmail API in the
Cloud project that issued this client` when Google refused the call.

**Cause:** the Gmail API is not enabled in the Cloud project your OAuth
client belongs to. It is the most common first-run failure.

**Fix (resolves it):** enable the **Gmail API** in that project, as
[`docs/gcp-setup.md`](gcp-setup.md#1-project-and-api) says, wait a
minute, and run `doctor` again.

## `this is a Web application client; create a Desktop app client instead`

**Cause:** the loopback redirect this server uses is a native-app flow,
which only a Desktop app client allows.

**Fix (resolves it):** create an OAuth client of type **Desktop app**, as
[`docs/gcp-setup.md`](gcp-setup.md#3-oauth-client) says, and pass its
JSON to `login --client-secret`.

## `auth: timed out waiting for the browser`

**Cause:** the browser could not reach the callback. Over SSH, the
callback goes to the loopback interface of the remote machine, while the
browser runs on yours.

**Fix (resolves it):** run `login --no-browser`. It prints the
authorization URL and the exact `ssh -L` line to run first, with the port
it picked.

## `auth: Google returned no refresh token`

**Cause:** Google completed the sign-in without a refresh token, which
the server needs to stay signed in.

**Fix (resolves it):** run `google-mail-mcp login --consent`. If that
does not help, remove the app at
[your Google account's third-party access page](https://myaccount.google.com/permissions)
and log in again.

## `keyring unavailable (…); refresh token saved in plaintext`

`status` names `file` as the token store, and every later use warns
`refresh token read from the plaintext file`.

**Cause:** the OS keyring could not be reached, for example on a headless
Linux machine with no Secret Service running.

**Fix (resolves it):** start a keyring, or log in from a desktop session,
and run `login` again. The token moves to the keyring and the file is
deleted.

**Impact:** the file is `0600` in the profile's directory; on Windows it
is restricted to your account by an access control list. Anyone who can
read it holds every scope you granted, which by default can send mail,
until you revoke it.

## `the profile records a refresh token in the OS keyring and the keyring returned nothing`

**Cause:** the keyring holds the token and will not hand it over. It is
more likely locked, or its session bus unreachable, than empty.

**Fix (resolves it):** unlock the keyring and try again.

**Workaround:** `login` writes a fresh token beside the first, which
leaves the cause in place.

## `GMAIL_REQUIRE_PROMPT was removed`

The server refuses to start.

**Cause:** the server asks the person nothing, and your client's approval
decides each call. It refuses this setting when it is set to anything but
`false`, rather than ignore it.

**Fix (resolves it):** unset it, and remove `--require-prompt` from your
client's arguments. For a client that runs with nobody watching, leave
`GMAIL_ENABLE_SEND`, `GMAIL_ENABLE_DESTRUCTIVE` and
`GMAIL_ENABLE_SETTINGS` off.

## A tool you expect is missing

**Cause:** the tools that reach other people, delete for good or change
a setting are registered only on request. `status` shows which switches
are on.

- `send_draft` needs `GMAIL_ENABLE_SEND=true`.
- `delete_permanently` and `delete_label` need
  `GMAIL_ENABLE_DESTRUCTIVE=true`.
- `update_signature` and `create_filter` need
  `GMAIL_ENABLE_SETTINGS=true`; `create_trash_filter` and
  `delete_filter` need it with `GMAIL_ENABLE_DESTRUCTIVE=true`, and
  `set_vacation` with `GMAIL_ENABLE_SEND=true`.
- `download_attachment` and `download_attachments` need
  `GMAIL_LOCAL_DIR`.
- `GMAIL_READ_ONLY=true` removes every write.

**Fix (resolves it):** set the switch as
[`docs/configuration.md`](configuration.md) shows, run `login` again if
it changes the scopes, and restart the server.

## `note: Gmail filed the draft in a new thread …, not the parent's`

**Cause:** Gmail threads a reply only when its subject matches the
parent's, and `update_draft` warns when a reply's subject changes. A
parent with no usable `Message-ID` gets no threading headers, and the
result says so.

**Fix (resolves it):** check that the result names the message you meant
to answer. `reply_to` takes a message id; `reply_to_thread` takes a
thread id and answers its newest message. Keep the parent's subject; if
`update_draft` changed it, set it back.

## `[stale] draft … changed since it was read`

**Cause:** the draft was saved after the read whose `message_id` you
passed: in Gmail, in another client, or by an earlier call. Every save
gives a draft a new message id, and each result names the current one.

**Fix (resolves it):** read the draft again with `get_draft`, check what
changed, and apply your change to that. Nothing was written.

## `[stale] subject is not the subject draft … holds`

The same class follows `first_line is not the first line of draft …'s
body` and `criteria are not filter …'s`.

**Cause:** what the call says it sends or deletes is not what Gmail
holds. The draft changed, or the subject, first line or criteria given
are not its own.

**Fix (resolves it):** read the draft with `get_draft`, or the filters
with `list_filters`, check it is still the one meant, and pass what they
show. Nothing was sent or deleted.

## `cursor expired: Gmail no longer keeps history from …`

`list_changes` says this instead of returning an empty list.

**Cause:** Gmail keeps history typically for a week or more, and
sometimes much less. The cursor is older than what it kept.

**Fix (resolves it):** the result carries the mailbox's current
`history_id`; call `list_changes` from it, and search for anything you
needed from the gap.

**Impact:** the changes after the old cursor cannot be listed.
Treat anything you held from before as possibly out of date.

## `[rate_limited] Google is rate limiting this account`

**Cause:** the account's per-minute quota is spent. The server spends
within it and waits rather than fail, so another client is likely using
the same Cloud project.

**Fix (resolves it):** wait a minute and retry.

Two other limits share the class, and the message says which.
`the Cloud project's daily Gmail quota is spent` resets at midnight
Pacific time. `Gmail's sending limit for this account is reached` lifts
within 24 hours; Google can report it minutes after the send that
crossed it. Do not resend in the meantime.

## `[blocked] … of draft …'s recipients are not in confirm_recipients`

**Cause:** the draft reaches someone the call did not name. Every
recipient must be written out in `confirm_recipients`, the people already
on the thread included. The refusal names them by field and position,
such as `cc[1]`.

**Fix (resolves it):** run `send_draft` with `dry_run: true`, which
lists the addresses, and name each one the person confirmed. A draft
with more than 50 recipients is refused outright: send it from Gmail.

## `[ambiguous_outcome] Google did not confirm the send of draft …`

**Cause:** the request left, and the answer did not come back. The
server has already read the mailbox to settle it, and the message gives
the verdict: `sent`, `not_sent` or `unknown`.

**Fix:** on `sent`, do nothing. On `not_sent` or `unknown`, look in
Gmail's Sent folder before deciding anything. The server never sends
again by itself.

**Impact:** a second send of the same draft can deliver the mail twice,
because Google does not deduplicate sends.

## Tasks

### Rotating the refresh token

```bash
google-mail-mcp login
```

The new token replaces the old one in the same store. Google keeps at
most 100 refresh tokens per account for each OAuth client and drops the
oldest without warning, so a script that logs in repeatedly signs out
your other machines.

### Rotating the OAuth client secret

Add a second secret to the same OAuth client in Google Cloud and
download its JSON. Then:

```bash
google-mail-mcp logout
google-mail-mcp login --client-secret ./new_client_secret.json
```

`logout` revokes the grant at Google, so no copy of the old token keeps
working. If your client configuration sets `GMAIL_CLIENT_SECRET`, point
it at the new file. Delete the old secret in Google Cloud last.

### Revoking access

```bash
google-mail-mcp logout
```

It revokes the grant at Google, then deletes the stored token and the
profile's state. Google revokes the whole grant, which covers every
client in the Cloud project, so `logout` names the other profiles of the
same account it signs out too. If the revoke fails, it says so and
removes the local copy anyway; revoke then from
[your Google account's third-party access page](https://myaccount.google.com/permissions).
A token in `GMAIL_REFRESH_TOKEN` is out of its reach.

### Suspected exposure

Treat a leaked refresh token as access to send mail as you, whatever
this server's switches say: the default scope can send. A copied home
directory, a shared machine or a `token.json` in a backup counts.

1. Run `google-mail-mcp logout` on any machine that still has it. The
   revoke ends the grant everywhere.
2. If you cannot, revoke at
   [your Google account's third-party access page](https://myaccount.google.com/permissions).
3. Check Gmail's Sent folder, **Settings → Forwarding and POP/IMAP**,
   **Filters**, the vacation reply and the signatures for anything you
   did not set up.
4. If the client JSON was exposed too, rotate the client secret, above.

### Two accounts on one machine

Profiles keep separate client secrets, accounts and tokens:

```bash
google-mail-mcp login --profile work --client-secret ./work_client_secret.json
google-mail-mcp login --profile personal --client-secret ./personal_client_secret.json
```

Then give each client entry its own `GMAIL_PROFILE`, as
[`docs/configuration.md`](configuration.md) shows.

### Reporting a problem

Include the `doctor` output, the version (`google-mail-mcp --version`),
how you installed it, which client you use, and the `GMAIL_*` switches
you set. For a failing call, turn the log level up first:

```bash
GMAIL_LOG_LEVEL=debug google-mail-mcp doctor
```

Logs carry no message content, addresses, label names or queries, and
ids are cut to six characters, so a debug log is safe to paste. **Do not
paste a tool result**: it is mail, and carries other people's words and
addresses. Describe it instead.

### Do the restricted scopes mean a security assessment?

Only if you publish an External app. Every Gmail scope that reads mail
is restricted. An Internal app in your own Workspace, or an External app
in Testing with yourself as a test user, needs nothing beyond the
unverified-app warning. A published External app with restricted scopes
needs Google's
[restricted scope verification](https://developers.google.com/identity/protocols/oauth2/production-readiness/restricted-scope-verification),
which includes an annual third-party security assessment.
