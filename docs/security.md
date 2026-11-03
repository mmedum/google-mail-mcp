# Security model

The threat model and the invariants the code holds.
[`SECURITY.md`](../SECURITY.md) says how to report a vulnerability.

It applies to one binary, running as a subprocess of your MCP client, on
your machine, with your own OAuth client and your own token (D-01). There
is no network listener except while `login` waits for the browser, no
shared deployment, no telemetry and no update check.

## Trust boundaries

| Boundary | Crosses | Assumption |
|---|---|---|
| MCP client ↔ this server | An OS pipe | Both run as the same OS user, with no authentication between them. A process that can write to the server's stdin can already read its memory. |
| This server ↔ Google | TLS | Only Google's API and OAuth origins. The allow-list is checked with the port before a credential is attached, and again on every redirect (D-81). |
| This server ↔ disk | The file system | The profile directory under `GMAIL_CONFIG_DIR`, which must be inside your home directory, and `GMAIL_LOCAL_DIR` for attachments (D-25). Nothing else is read or written. |
| Browser ↔ the login listener | The loopback interface, on a random port | Alive only while `login` waits. PKCE and `state` are checked on the callback (D-71). |
| Correspondents ↔ the model | Mail content | Written by someone else, sometimes to steer an assistant (D-09). |

## What is worth stealing

- **The refresh token.** Long-lived, and it carries every scope you
  granted. The default scope, `gmail.modify`, can send mail: Google has
  no scope that writes drafts but cannot send. It lives in the OS
  keyring where there is one, and in an owner-only file where there is
  not.
- **The access token in flight.** Short-lived, with the same scopes.
- **Mail.** Other people's words and addresses. It passes through tool
  results to your client, and nowhere else.
- **Your OAuth client secret.** Google treats a Desktop client's secret
  as public, which is why the loopback flow uses PKCE. It is still yours.

## Adversaries

**A correspondent writing to steer your assistant.** Anyone can mail you,
and the server cannot stop a model being persuaded. So it limits what
persuasion reaches. Mail is marked as data inside a per-call boundary,
and never speaks in the server's voice (D-09, D-10). Text a reader would
not see is removed and counted (D-11). Nothing a message links to or
embeds is fetched (D-12). The writes that redirect mail, such as
forwarding, delegation and send-as, do not exist (D-13).

**A malicious MCP client, or a persuaded model driving an honest one.**
It can call any registered tool under your credentials. A client may
ignore tool annotations, so registration is the control. Sending,
permanent deletion and settings writes register only with their switches
(D-48, D-52, D-55). A write that cannot be undone takes `confirm` and
repeats what it acts on (D-47, D-66); those are witnesses the model
writes, never your approval. An address a message talked into a draft is
one the model has to type again in `confirm_recipients` (D-49). Past
that, Google's per-scope authorization is the bound, and
`GMAIL_READ_ONLY` is yours.

**A co-resident process on your machine.** It cannot take the login
socket, and the token file is readable by your account alone.

## Invariants the code holds

A test or a gate holds each of these. A way to break one is a
vulnerability.

- **Stdout carries only JSON-RPC.** Logs go to stderr; forbidigo and the
  smoke test hold it (D-02).
- **Logs never carry the payload**, at any level, and ids are cut to six
  characters. A transport error loses its URL, which carries the query.
  `TestLogsNeverCarryThePayload` and `TestTransportErrorsCarryNoURL` hold
  it (D-03).
- **A credential goes only to Google's origins**, checked on every
  redirect. `TestARedirectOffTheAllowlistIsRefused` holds it (D-81).
- **Mail stays inside its block.** `TestInjectedTextStaysInsideItsBlock`
  and `TestSenderTextNeverReachesTheServersVoice` hold it (D-09, D-10).
- **The gated tools are absent without their switches.**
  `TestGatedToolsRegisterOnlyWithTheirFlag` holds it (D-64).
- **A send goes only through a draft, names every recipient, and is
  never repeated.** Over 50 recipients is refused.
  `TestSendDraftRecipientGuard` and `TestASendIsNeverRepeated` hold it
  (D-49, D-50).
- **Writes take explicit ids, at most 100, never a query** (D-44).
- **A dry run writes nothing.**
  `TestWithoutWritesRefusesEveryWriteBeforeTheNetwork` holds it (D-43).
- **A filter cannot forward.** Its write type has no field for it, and
  `TestAFilterCannotForward` holds it (D-56).
- **Files move only through `GMAIL_LOCAL_DIR`**, never over an existing
  file, and unset means none (D-25).
- **No tool changes the server's own settings.**
  `TestNoToolWritesTheServersOwnSettings` holds it (D-69).
- **The server asks the person nothing** (D-65). The
  `requiresUserInteraction` mark (D-68) is a hint, never the control.
- **Read-only mode asks for `gmail.readonly` only**, so Google refuses a
  write too (D-70).
- **The token file is written only when the keyring was unavailable**,
  owner-only, and every use of it warns (D-71).
- **`logout` revokes at Google before deleting locally** (D-76).
- **Nothing in the repository identifies a real mailbox or person**
  (D-04). Fixtures are generated, never recorded (D-05), and the live
  driver reads only what it wrote (D-87).

## Not a vulnerability

Do not report these as vulnerabilities. Open an ordinary issue if one
surprises you.

- **The client did not prompt.** Annotations and marks are hints. A tool
  registered while its switch is off is a vulnerability.
- **What runs as you can act as you.** Any process running as you can
  read the token and use it, with or without this server.
- **A tool doing what its registration allows.** A client or a model
  calling a registered tool under your credentials is the design. The
  controls are `GMAIL_READ_ONLY`, `GMAIL_ENABLE_SEND`,
  `GMAIL_ENABLE_DESTRUCTIVE`, `GMAIL_ENABLE_SETTINGS` and
  `GMAIL_LOCAL_DIR`.
- **A model following instructions in a message.** That is the client's
  concern. A message that makes the server itself break an invariant is
  a vulnerability.
- **Mail in a tool result.** A result is your mail, unmasked. Pasting
  one into a public issue discloses other people's words, so the issue
  form asks you not to.
- **Your Workspace administrator reading your mail.** They can, with or
  without this server.
- **Someone else's code.** A flaw in Google's OAuth service or APIs goes
  to Google, and one in a dependency, such as the MCP Go SDK, to that
  project. We will help route it.

## Dependency vulnerabilities

`govulncheck` runs in `make check` and in CI on every pull request and
push to `main`. It fails when this server's code can reach a vulnerable
symbol.

A CVE in a dependency is a vulnerability here only when `govulncheck`
reports it reachable. It is then fixed in a release whose CHANGELOG entry
names the CVE or GHSA. Otherwise the dependency is updated in the normal
course, and a report about it is not a vulnerability.

## What you are responsible for

- **Your own OAuth client and Cloud project.** Each person creates their
  own, so nobody else's rollout can revoke your access.
  [`docs/troubleshooting.md`](troubleshooting.md#tasks) says how to rotate
  the secret, revoke access and handle a suspected exposure.
- **The switches you turn on.** `GMAIL_ENABLE_SEND`,
  `GMAIL_ENABLE_DESTRUCTIVE` and `GMAIL_ENABLE_SETTINGS` arm tools that
  reach other people or cannot be taken back. Leave them off for a
  client that runs with nobody watching, and use `GMAIL_READ_ONLY=true`
  where reading is enough.
- **The scopes you grant.** A tool whose scope you declined fails with
  `[auth]` naming it. That is a supported state, not a broken one.
- **Where the token lives.** Keep a keyring available, keep
  `GMAIL_CONFIG_DIR` somewhere only you can read, and keep
  `GMAIL_REFRESH_TOKEN` out of shared shells and logs.
- **Keeping the client secret file out of version control.**
