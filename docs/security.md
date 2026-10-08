# Security

## What this server talks to

Only Google, over HTTPS: `gmail.googleapis.com`, `www.googleapis.com`,
`oauth2.googleapis.com` and `accounts.google.com`. The allow-list is
checked **with the port** before any credential is attached, and again on
every redirect, so neither a redirect nor a misconfigured base URL can
send an access token somewhere else. There is no telemetry and no update
check. Nothing a message links to or embeds is ever fetched.

## Scopes

| Mode | Scope |
|---|---|
| `GMAIL_READ_ONLY=true` | `gmail.readonly` |
| default | `gmail.modify` |
| `GMAIL_ENABLE_SEND=true` | `gmail.modify` — unchanged |
| `GMAIL_ENABLE_DESTRUCTIVE=true` | `https://mail.google.com/` |
| `GMAIL_ENABLE_SETTINGS=true` | adds `gmail.settings.basic` |

`docs/gcp-setup.md` carries the same sets, generated from the code.

**The default token can send mail.** Google has no scope that allows
drafts but not sending: `gmail.compose` and `gmail.modify` both reach
`drafts.send` and `messages.send`. This server withholds sending by not
registering `send_draft` unless `GMAIL_ENABLE_SEND=true`. That stops
*this server* from sending. It does not stop anything else holding the
token, which is why the token lives in the OS keyring and never in a
place a tool can read.

**`https://mail.google.com/` is requested only for permanent deletion.**
It is the only scope Google accepts for `messages.delete` and
`threads.delete`, so asking for it by default would make the destructive
flag a label rather than a limit. Turning the flag on needs a new
`login`; `doctor` says so.

A missing scope is `[auth]`, naming the scope and saying to run `login`.

## Where the token lives

The refresh token goes to the OS keyring — Secret Service on Linux,
Keychain on macOS, Credential Manager on Windows. With no keyring, it goes
to a file under the profile directory restricted to the current user
(`0600` on Unix, an ACL on Windows, where `0600` protects nothing), and
the server warns on **every use**, not once. `GMAIL_REFRESH_TOKEN`
overrides both, for CI.

`logout` revokes the token at Google and then deletes the local copy. It
leaves an environment-provided token alone.

## Mail is untrusted input

Every subject, body, header, snippet and attachment name was written by
somebody other than the person using the server, and some of it is
written to steer an AI agent. The server cannot stop a model being
persuaded. It does four things instead:

- **Marks it.** Mail content is returned inside a delimited block naming
  its sender and message id, with a boundary token generated for each
  call so the content cannot close the block itself.
- **Shows what was hidden.** HTML is converted to text by the server.
  Text a person reading the mail would not see — hidden styles,
  zero-size text, zero-width and bidirectional control characters,
  comments — is removed, and the result says how much was removed.
  Legitimate mail rarely hides much; the count is the signal.
- **Fetches nothing.** No images, stylesheets, tracking pixels or links.
  A link whose visible text names a different host from its target is
  flagged.
- **Removes the writes that would make persuasion catastrophic.**
  Auto-forwarding, forwarding addresses, filters that forward and
  delegation are not implemented at all. Sending is absent unless
  enabled, and even then goes only through a draft, and every recipient
  not already in the thread must be named again in the send call.

No tool description or server instruction tells the model to act on
what a message says.

## What gets logged

Logs go to stderr through `slog`. Stdout carries JSON-RPC frames and
nothing else, held by a smoke test.

A log line carries the method, the tool name, the outcome, the duration,
the quota units spent and an id **truncated to six characters**. It
carries no address, subject, snippet, body, attachment name, label name
or search query. A Gmail search travels in a request URL's `q=`, so
requests are logged by operation rather than URL, and a transport
error's text — which names the URL — is replaced with the kind of failure
it was.

A test drives every registered tool at debug level with arguments that
are words existing nowhere else, and fails if any of them reaches the
log. That is what makes it safe to ask for a debug log in a bug report.

`doctor` and `status` mask the account address, the client-secret path
and the OAuth client id, for the same reason.

## What a tool result carries

Mail, which is what you asked for. A result is not masked: the server
holds your own token, and you can read the same mail in Gmail. The risk
that is real is the **paste path** — a result copied into a public issue
carries other people's addresses and words. The issue form says to
describe a result rather than paste it; the three artifacts meant to be
pasted — a debug log, `doctor` and `status` — are the ones that mask.

## What the server refuses to do

- **Send** unless `GMAIL_ENABLE_SEND=true`, and then only a draft, never
  retried on an ambiguous failure. Every recipient who has not written in
  the thread being answered, and whom the account has not sent to in it,
  must be written out in `confirm_recipients`, so
  an address a message talked into a draft is one the model has to type
  itself; over 50 recipients is refused outright. The draft is sent as it
  is stored, and refused as `[stale]` if it changed since it was read.
- **Delete permanently** unless `GMAIL_ENABLE_DESTRUCTIVE=true`, and then
  only with `confirm: true` on the call. Removal is otherwise trash, which
  Gmail keeps for 30 days. The one exception registered by default is
  `delete_draft`: Gmail deletes a draft for good, so it too takes
  `confirm: true`, and it can only ever remove an unsent draft.
- **Change a setting** unless `GMAIL_ENABLE_SETTINGS=true`, and then only
  a signature, a filter or the vacation reply. A filter cannot forward:
  the type it is written as has no field for it. A filter that trashes
  matching mail needs `confirm: true`, since it hides mail as it arrives.
  The vacation reply answers other people, so it also needs
  `GMAIL_ENABLE_SEND=true`; it answers only contacts or the account's
  domain, never every sender, and turning it on needs `confirm: true`.
  Forwarding, delegation and send-as identities are never written.
- **Make a write that takes `confirm` without asking you, when it can.**
  `confirm` and `confirm_recipients` are arguments the model writes, and
  a model persuaded by a message writes them too. So when the client
  supports MCP elicitation, the server asks you itself before each of
  those writes, naming what it touches; text from the mailbox in the
  question stands in backticks or code style, on one line, with no link drawn,
  so a client that shows the question as Markdown shows it as written.
  Only an accept writes. The answer is bound to the call it was
  asked for, spent once, and void after 5 minutes, and a client cannot
  answer before it is asked. A client that cannot ask gets no question,
  and `GMAIL_REQUIRE_PROMPT=true` refuses those writes there instead.
  An `accept` is still not proof that a person read the question: a
  client hook you configured can answer for you.
- **Overwrite a draft it did not read.** `update_draft` takes the message
  id `get_draft` returned, reads the draft again, and refuses with
  `[stale]` if it changed. Gmail offers no lock, so a change landing
  between that read and the save can still be lost; the window is one
  round trip.
- **Act on a query.** Writes take explicit ids, at most 100 per call.
- **Read or write files outside `GMAIL_LOCAL_DIR`.** Unset means no file
  transfer at all.
- **Change where mail goes.** Forwarding, delegation and send-as identities
  are read-only or absent, and a filter cannot forward.

Tool annotations and `requiresUserInteraction` are set, and are hints: a
host in an auto-approve mode runs an annotated tool without asking. For a
client that can ask, the tools that ask before every write go without
the mark, so the person answers once. Every
control above is server-side, and client-side approval is not counted as
one of them.

## What is in this repository

Nothing deployer-specific: no message, thread, draft or label ids, no
account or correspondent addresses, no Cloud project ids, no OAuth client
ids or secrets, and no subject, body, header or attachment name from a
real mailbox.

**gitleaks** covers credentials, in the pre-commit hook and in CI.
**`go run ./scripts/gates leaks`** covers identifiers, which a credential
scanner passes untouched; every rule in it is an allow-list anchored on a
shape the server's own generated values cannot take.

A mailbox's content is ordinary words, and no pattern separates an
invented subject from a real one. So that half is structural: **fixtures
are generated, never recorded**, and **the live driver reads only
messages it inserted itself**, under a label it created for the run.

## Reporting a vulnerability

Open a [private security
advisory](https://github.com/mmedum/google-mail-mcp/security/advisories/new)
rather than an issue. Please do not include anything from a real mailbox.
