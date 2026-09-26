# Changelog

All notable changes to this project are documented here. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The versioning contract is the MCP tool surface: tool names, input and
output schemas, resources, and documented behavior. Package layout, log
lines and error wording outside the `[class]` prefix are not part of it.

Sections appear in this order: Added, Changed, Deprecated, Removed,
Fixed, Security. One line per change. A change that needs the reader to
act starts with **Breaking:**. The section for a tag is its release note,
lifted verbatim.

## [Unreleased]

### Added

- `get_profile` reports which account is signed in and its mailbox totals.
- `search_threads` and `search_messages` search with Gmail's query syntax, bounded and paged.
- `get_thread` and `get_message` read mail with its content marked as untrusted data.
- `list_labels`, `list_drafts` and `get_draft` read labels and drafts.
- `list_changes` lists what changed since a history id, and says so when that cursor has expired.
- `get_settings` shows forwarding, the vacation reply, send-as addresses, IMAP, POP and language, read-only.
- `list_filters` shows the account's filters and flags any that forward mail.
- `download_attachment` saves an attachment into `GMAIL_LOCAL_DIR`, streamed, never overwriting a file.
- `get_message` and `get_thread` list each attachment's `part_id`.
- Resources `gmail://threads/{id}`, `gmail://messages/{id}` and `gmail://labels` carry the same text as the matching tools.
- `login`, `logout`, `status` and `doctor` manage and diagnose the OAuth login, with `--no-browser` for remote machines.
- Read-only mode (`GMAIL_READ_ONLY`) registers only the read tools and requests only `gmail.readonly`.
- Repository gates run by `make check` and CI: leaks, pins, error classes, API coverage, schema diff, smoke, staleness and more.
- Signed release archives for six platforms with SBOMs, build provenance, a Claude Desktop bundle and an MCP registry entry.

[Unreleased]: https://github.com/mmedum/google-mail-mcp/compare/dc26f63...HEAD
