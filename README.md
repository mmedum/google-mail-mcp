# google-mail-mcp

[![CI](https://github.com/mmedum/google-mail-mcp/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/mmedum/google-mail-mcp/actions/workflows/ci.yml)
[![Latest release](https://img.shields.io/github/v/release/mmedum/google-mail-mcp?sort=semver)](https://github.com/mmedum/google-mail-mcp/releases/latest)
[![Go Reference](https://pkg.go.dev/badge/github.com/mmedum/google-mail-mcp.svg)](https://pkg.go.dev/github.com/mmedum/google-mail-mcp)
[![License: Apache 2.0](https://img.shields.io/github/license/mmedum/google-mail-mcp)](./LICENSE)

Gmail as MCP tools. Read your mail without letting it read your assistant.

A single Go binary that speaks MCP over stdio. It runs as a subprocess of
your client, on your own machine, against your own Google account. There
is no server to host and no service account: you create a Google OAuth
client, log in once, and the refresh token stays in your OS keyring.

It works **inside** a mailbox. A calendar invitation's event, a linked
Drive file and a Chat space belong to servers built on those APIs.

## Why google-mail-mcp

Every message was written by somebody other than you, and some of it is
written to steer an AI assistant. So mail comes back inside blocks marked
with a per-call boundary token; text a reader of the mail would not have
seen — hidden styles, zero-width characters, text colored like its
background — is removed and counted; nothing a message links to or embeds
is fetched; and a link whose text names a different site from its target
is flagged.

The writes that would make a persuaded assistant dangerous are not there.
Forwarding, filters that forward and delegation are not implemented.
Sending is not registered unless you turn it on, and then only a draft
can be sent. Removal is trash; permanent deletion is not registered
unless you turn it on, and is the only thing that asks Google for full
mail access.

Reads are bounded. A thread comes back newest first within a character
budget, with quoted replies and signatures collapsed to a line saying how
much was hidden, and every omission says how to continue. Non-ASCII
subjects, names and filenames decode in any charset.

## Install

```bash
go install github.com/mmedum/google-mail-mcp/v2/cmd/google-mail-mcp@latest
```

Or take an archive from the
[latest release](https://github.com/mmedum/google-mail-mcp/releases/latest)
— Linux, macOS and Windows, on amd64 and arm64 — and verify it before you
run it:

```bash
sha256sum -c checksums.txt --ignore-missing

# The checksum file is signed with a keyless Sigstore certificate tied to
# the release workflow's identity.
cosign verify-blob checksums.txt \
  --bundle checksums.txt.bundle \
  --certificate-identity-regexp 'https://github\.com/mmedum/google-mail-mcp/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# And the archive itself carries build provenance.
gh attestation verify google-mail-mcp_*.tar.gz --repo mmedum/google-mail-mcp
```

Every archive ships an SBOM. Builds are reproducible: rebuilding a tag
gives the same bytes.

### Claude Desktop

Every release also carries a `.mcpb` bundle. Open it and Claude Desktop
installs the server and asks for your OAuth client JSON. It covers macOS,
Windows and Linux on both architectures. Its SHA-256 is in the same signed
`checksums.txt`.

The bundle does **not** log you in. Install the binary as well, run
`google-mail-mcp login --client-secret <your client JSON>` once, and the
bundle uses the same credentials.

## Set up Google

[`docs/gcp-setup.md`](docs/gcp-setup.md) has the whole of it: a Cloud
project with the Gmail API enabled, a consent screen, the scopes for how
you will run the server, and a **Desktop app** OAuth client. Then:

```bash
google-mail-mcp login --client-secret ~/path/to/client_secret.json
google-mail-mcp doctor
```

Every Gmail scope that reads mail is one Google calls restricted. For your
own use that costs nothing, but an External client in Testing gets refresh
tokens that expire after seven days; `docs/runbook.md` says what that
looks like.

### Logging in over SSH

The callback goes to the *remote* host's loopback address while your
browser is local, so forward the port. Run
`google-mail-mcp login --no-browser`: it prints the authorization URL and
the exact `ssh -L` line for the port it picked. Run that line in a second
local terminal, then open the URL locally.

## Connect a client

Claude Code:

```bash
claude mcp add google-mail -- google-mail-mcp
```

Or, in a client config file:

```json
{
  "mcpServers": {
    "google-mail": {
      "command": "google-mail-mcp"
    }
  }
}
```

Every setting is a `GMAIL_*` environment variable, listed in
[`docs/configuration.md`](docs/configuration.md). The ones worth knowing
now: `GMAIL_READ_ONLY=true` registers only the read tools and asks for
read-only access; `GMAIL_LOCAL_DIR` is the one directory attachments may
be saved to; `GMAIL_PROFILE` lets one machine hold several accounts.

## Tools

| Tool | What it does |
|---|---|
| `get_profile` | The signed-in account, its totals and the current history id |
| `search_threads` | Find conversations with a Gmail search |
| `search_messages` | Find single messages with a Gmail search |
| `get_thread` | Read a conversation, newest first, within a budget |
| `get_message` | Read one message, by id or by its `Message-ID` header |
| `list_labels` | Every label, optionally with counts |
| `list_drafts` | Unsent drafts |
| `get_draft` | Read one draft |
| `list_changes` | What changed since a history id, and when that cursor has expired |
| `get_settings` | Forwarding, vacation reply, send-as addresses, IMAP and POP, read-only |
| `list_filters` | The account's filters, with any that forward mail flagged |
| `download_attachment` | Save an attachment into `GMAIL_LOCAL_DIR`, never overwriting; only when that is set |
| `create_draft` | Save a new draft or a reply, threaded by the server; nothing is sent |
| `update_draft` | Change only the fields given, refusing a draft that changed since it was read |
| `delete_draft` | Delete a draft for good, with `confirm: true` |
| `modify_labels` | Add and remove labels on up to 100 messages or threads, reported per item |
| `trash` | Move up to 100 messages or threads to the trash, kept 30 days |
| `restore` | Take messages or threads out of the trash |
| `create_label` | Create a user label |
| `update_label` | Rename a user label or change how it shows |
| `send_draft` | Send a draft, naming every recipient who has not written in the thread; only with `GMAIL_ENABLE_SEND=true` |
| `delete_permanently` | Delete up to 100 messages or threads for good, with `confirm: true`; only with `GMAIL_ENABLE_DESTRUCTIVE=true` |
| `delete_label` | Delete a user label, with `confirm: true`; only with `GMAIL_ENABLE_DESTRUCTIVE=true` |
| `update_signature` | Set the signature of one of the account's addresses; only with `GMAIL_ENABLE_SETTINGS=true` |
| `create_filter` | Create a filter for mail that arrives from now on; it cannot forward; only with `GMAIL_ENABLE_SETTINGS=true` |
| `delete_filter` | Delete a filter, with `confirm: true`; only with `GMAIL_ENABLE_SETTINGS=true` |
| `set_vacation` | Turn the vacation reply on for contacts or the domain, or off; only with `GMAIL_ENABLE_SETTINGS=true` and `GMAIL_ENABLE_SEND=true` |

Searches take Gmail's own search language in `q`, plus `after` and
`before` as real instants — Gmail reads a date written inside `q` as
midnight Pacific time. Every result states the quota it spent. Every
write takes `dry_run`, names what it touches by id, and says what it
changed, read from Gmail's answer. `GMAIL_READ_ONLY=true` leaves the
writes out. The last seven tools are not registered at all unless their
setting is on.

For clients that attach rather than call, `gmail://threads/{id}`,
`gmail://messages/{id}` and `gmail://labels` carry the same text as
`get_thread`, `get_message` and `list_labels`.

## Safety

- **Mail is data.** It arrives marked, with what was hidden removed and
  counted, and no tool description tells a model to act on it.
- **Nothing reaches another person by default.** Drafts wait in Gmail
  to be sent, and by default no tool changes a setting. Forwarding is
  shown, never set.
- **Settings are opt-in.** With `GMAIL_ENABLE_SETTINGS=true` the server
  can set a signature and create or delete filters. A filter cannot
  forward, and one that trashes mail needs `confirm: true`. The vacation
  reply answers other people, so it also needs `GMAIL_ENABLE_SEND=true`,
  it only ever answers contacts or the account's domain, and turning it
  on needs `confirm: true`.
- **Sending is opt-in and goes through a draft.** With
  `GMAIL_ENABLE_SEND=true`, `send_draft` sends a draft exactly as it is
  stored. Every recipient not already in the thread must be written out
  in `confirm_recipients`, and a send Gmail does not confirm is never
  repeated.
- **Removal is trash.** Gmail keeps trashed mail for 30 days, and
  `restore` brings it back. By default the only permanent removal is
  `delete_draft`, which needs `confirm: true`. `delete_permanently` and
  `delete_label` exist only with `GMAIL_ENABLE_DESTRUCTIVE=true`, which
  also asks for the one scope that can delete; each call needs
  `confirm: true`.
- **You confirm what cannot be undone.** Every write above that takes
  `confirm` or `confirm_recipients` is also put to you, when your MCP
  client supports elicitation: the question names the label, the draft,
  the filter, the reply or every recipient, and nothing is written
  unless you accept. A client that runs with nobody to
  answer, such as `claude -p`, cannot make these writes. A client that
  cannot ask falls back to `confirm`; `GMAIL_REQUIRE_PROMPT=true` refuses
  instead. See [docs/configuration.md](docs/configuration.md#confirming-a-write).
- **Files go to one directory.** `download_attachment` writes only into
  `GMAIL_LOCAL_DIR`, under a name made safe, and never over an existing
  file.
- **Logs never carry mail.** No address, subject, body, label name or
  query; ids are cut to six characters. A test drives every tool with
  marker values and fails if one reaches a log, which is what makes a debug
  log safe to paste.
- Everything above is enforced in the server. Tool annotations are set,
  but they are hints a client may ignore.

[`docs/security.md`](docs/security.md) has the whole of it.

## Getting help

Run `google-mail-mcp doctor` first: it checks the client JSON, the token,
the granted scopes and whether the Gmail API is enabled, and names what
is missing. [`docs/runbook.md`](docs/runbook.md) covers the common
surprises. Then open an issue with `doctor`'s output and the version.
**Do not paste a tool result**: it is mail, and carries other people's
words and addresses.

## Versioning

Semantic versioning. The tool names and their output fields are the
public contract; [`CHANGELOG.md`](CHANGELOG.md) marks anything you have
to act on as **Breaking**.

## Development

```bash
make check
```

That is what CI runs. [`docs/development.md`](docs/development.md) says
what each gate holds and how the live driver works.

## Documentation

[`docs/README.md`](docs/README.md) is the index. The design, the platform
facts behind it and a verdict on every Gmail API method are in
[`docs/architecture.md`](docs/architecture.md).

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md).

## Security

Report a vulnerability through a private advisory; see
[`SECURITY.md`](SECURITY.md).

## Code of conduct

[`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md).

## License

[Apache 2.0](LICENSE).
