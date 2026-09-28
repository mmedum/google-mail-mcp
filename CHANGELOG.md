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

## [2.0.0] - 2026-09-28

### Added

- Before `delete_draft`, `delete_label`, `delete_permanently`, `delete_filter`, a `create_filter` that trashes, a `set_vacation` that turns the reply on, and `send_draft`, the server asks you through the MCP client (form elicitation) when the client supports it, naming what the write touches, and showing the body before a send or a vacation reply; only an accept writes, on top of `confirm` and `confirm_recipients`.
- `GMAIL_REQUIRE_PROMPT` (`--require-prompt`) refuses those writes as `[blocked]` when the client cannot ask you.

### Changed

- **Breaking:** the Go module path is now `github.com/mmedum/google-mail-mcp/v2`, as Go requires from v2 on; install with `go install github.com/mmedum/google-mail-mcp/v2/cmd/google-mail-mcp@latest`.
- **Breaking:** those writes are now `[blocked]` ("not confirmed by the person") whenever a client that declares elicitation does not come back with an accept, so an unattended run such as `claude -p`, which cancels every question, can no longer make them; run them from a client you are watching, or from one that does not declare elicitation.
- The descriptions of the tools that take `confirm` say the server also asks the person and how many units asking adds, and the server's instructions say a call the person did not confirm is not to be repeated.

## [1.2.0] - 2026-09-28

### Added

- `list_filters`, `create_filter` and `delete_filter` report `never_spam` on a filter that keeps matching mail out of spam.

### Fixed

- Filter, signature and vacation changes sent in parallel run one at a time, and filter writes are spaced 5 seconds apart, instead of failing with Gmail's "Precondition check failed"; on a filter write that refusal is retried once, then reported as `[unavailable]` in Google's words.
- A `create_filter` that Google did not confirm reads the filters afterwards and says whether it was created, not created, or still unknown.
- A filter that removes `SPAM` is shown as never sending matching mail to spam, rather than as removing a label.
- `create_filter` refuses an archiving filter that already exists when Gmail has since added `SPAM` to the stored one.
- `omitted_ids` no longer names a row the text shows, and the text counts every row it leaves out; `list_filters` shows forwarding filters first.
- `search_threads`, `search_messages` and `list_drafts` say that the structured result carries every row in full, and that a client with a per-result size limit should ask for a smaller `max`.
- A `delete_filter` that Google did not confirm reads the filters afterwards and says whether it was deleted.

## [1.1.1] - 2026-09-27

### Fixed

- The Claude Desktop bundle's descriptions and the docs name the settings flag wherever they list what changes the scopes or what is off by default.
- `logout` names the other profiles of the same account in the same Cloud project, which its revoke signs out, instead of every profile sharing the client file; `login` records the project to match on.
- The runbook says revoking at Google ends every token the account granted to the Cloud project, not only one client's.

## [1.1.0] - 2026-09-27

### Added

- `GMAIL_ENABLE_SETTINGS` registers `update_signature`, `create_filter` and `delete_filter`, and asks for `gmail.settings.basic` at login.
- `update_signature` sets the signature of one of the account's addresses, as plain text.
- `create_filter` makes a filter for mail that arrives from now on; it cannot forward, and one that trashes needs `confirm: true`.
- `delete_filter` deletes a filter, with `confirm: true`.
- `set_vacation` turns the vacation reply on for contacts or the account's domain, or off; it also needs `GMAIL_ENABLE_SEND`.
- The Claude Desktop bundle has a switch for settings changes.

## [1.0.0] - 2026-09-27

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
- `get_thread` shows a thread's unsent drafts after the conversation, in a section of their own.
- Resources `gmail://threads/{id}`, `gmail://messages/{id}` and `gmail://labels` carry the same text as the matching tools.
- `create_draft` saves a draft, or a reply the server threads from its parent with `reply_to` or `reply_to_thread`.
- `update_draft` changes only the fields given and refuses with `[stale]` a draft that changed since it was read.
- `delete_draft` deletes a draft for good, with `confirm: true`.
- `modify_labels` adds and removes labels on up to 100 messages or threads, with each item's labels before and after.
- `trash` and `restore` move up to 100 messages or threads in and out of the trash, item by item.
- `create_label` and `update_label` create and change user labels, checking names the way Gmail does.
- Every write takes `dry_run`, and drafts over 5 MB are sent as a media upload.
- `send_draft` sends a draft, only with `GMAIL_ENABLE_SEND=true`; every recipient who has not written in the thread, and whom the account has not sent to in it, must be named in `confirm_recipients`.
- A send Gmail does not confirm is never retried; `send_draft` reads the mailbox and says whether it was sent.
- `delete_permanently` deletes up to 100 messages or threads for good, and `delete_label` deletes a user label, only with `GMAIL_ENABLE_DESTRUCTIVE=true` and `confirm: true`.
- `doctor` says when the token holds more access than the configuration needs, and how to narrow it.
- `login`, `logout`, `status` and `doctor` manage and diagnose the OAuth login, with `--no-browser` for remote machines.
- `login` says whether Google issued a new refresh token or kept the stored one.
- Read-only mode (`GMAIL_READ_ONLY`) registers only the read tools and requests only `gmail.readonly`.
- `make evals` scores a model driving the tools over an in-memory mailbox, including mail written to steer it.
- Repository gates run by `make check` and CI: leaks, pins, error classes, API coverage, schema diff, smoke, staleness and more.
- Signed release archives for six platforms with SBOMs, build provenance, a Claude Desktop bundle and an MCP registry entry.

[Unreleased]: https://github.com/mmedum/google-mail-mcp/compare/v2.0.0...HEAD
[2.0.0]: https://github.com/mmedum/google-mail-mcp/compare/v1.2.0...v2.0.0
[1.2.0]: https://github.com/mmedum/google-mail-mcp/compare/v1.1.1...v1.2.0
[1.1.1]: https://github.com/mmedum/google-mail-mcp/compare/v1.1.0...v1.1.1
[1.1.0]: https://github.com/mmedum/google-mail-mcp/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/mmedum/google-mail-mcp/compare/dc26f63...v1.0.0
