# Security policy

## Reporting a vulnerability

Report security issues privately through GitHub's
[security advisory](https://github.com/mmedum/google-mail-mcp/security/advisories/new)
form, not in a public issue.

Say what you did, what happened, and what you expected. Do not include
real mail, email addresses, message ids or credentials: describe the
shape of the problem instead.

You should get an acknowledgment within a week.

## Scope

This server runs locally, speaks MCP over stdio, and acts as one user
against the Gmail API with that user's own OAuth client. There is no
hosted component.

In scope and worth reporting:

- Anything that puts mail content, an address, a query or a token into a
  log, an error message, a fixture or this repository.
- Mail content that the server presents as something other than
  untrusted data, or a tool description that tells the model to act on
  what a message says.
- A send without `GMAIL_ENABLE_SEND`, a permanent deletion without
  `GMAIL_ENABLE_DESTRUCTIVE`, or a send that is retried.
- A write that acts on a query rather than on the ids it was given.
- Anything that lets stdout carry something other than a JSON-RPC frame.
- A dependency vulnerability `make vuln` does not catch.

## What the design already assumes

Tool annotations are not a control. Clients treat them as untrusted, and
a host in an auto-approve mode runs an annotated tool without asking. A
tool that must not run unattended is not registered unless its flag is
set. Do not report "the client did not prompt"; do report a tool that is
registered while its flag is off.

The default token can send mail. Gmail has no scope that writes drafts
but cannot send. Leaving the send tool unregistered stops this server
sending; it does not stop anything else holding the token, which is why
the token lives in the OS keyring. `docs/security.md` says more.

## Verifying a release

Each release's `checksums.txt` is signed with a keyless Sigstore
certificate and every archive and the bundle carry build provenance. The
release notes give the exact commands.
