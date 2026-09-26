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
go install github.com/mmedum/google-mail-mcp/cmd/google-mail-mcp@latest
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

Searches take Gmail's own search language in `q`, plus `after` and
`before` as real instants — Gmail reads a date written inside `q` as
midnight Pacific time. Every result states the quota it spent.

## Safety

- **Mail is data.** It arrives marked, with what was hidden removed and
  counted, and no tool description tells a model to act on it.
- **Nothing reaches another person** from the tools above.
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
