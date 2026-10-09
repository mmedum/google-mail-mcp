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

## [2.2.0] - 2026-10-09

### Added

- `--dump-schemas` names the build's version, so a recorded tool surface says which release it is.
- `search_threads` rows carry `drafts`, how many drafts a thread holds, and `latest_from_me`, whether this account sent the thread's latest message.
- `search_messages` rows and message reads carry `unsubscribe`: the web and mail addresses of the sender's `List-Unsubscribe` header, most preferred first, and `one_click` when the sender declares one-click unsubscribe. The server never visits them.
- A calendar invitation's attachment entry carries `invitation`: the event's UID, which a calendar server finds it by, its sequence, start and end, each with the time zone it names, organizer, title and how many events it holds. An invitation Gmail stored apart costs one more read, at most one per message.
- `download_attachments` saves several attachments of one message into `GMAIL_LOCAL_DIR`: those `part_ids` names, or every attachment but inline parts, emoji reactions and an invitation's calendar version of the body. Each file is saved or fails on its own, and the files saved before a failure are kept. Registered only when `GMAIL_LOCAL_DIR` is set.
- `create_draft` takes `forward`, a message id: the message goes attached to the draft as `<subject>.eml`, as Gmail stored it but without its `Bcc` header, under the subject `Fwd:` and the original's. The server asks Gmail to file the draft in the original's thread, which Gmail did when tested, and the result says each time whether it did. The result's `forwarded` names the message and the copy's size, and says whether a `Bcc` was left out.
- `create_draft` takes `quote` on a reply: the parent's text goes below the body after a blank line and `On <date>, <sender> wrote:`, each line after `> `, and in the HTML version made from the text too. A parent with only HTML is quoted as converted, with each link reduced to its host. The result's `reply` carries `quoted_chars` and `quote_from_html`. A later `update_draft` with `body` replaces the quote too.
- `read_attachment` reads one attachment as text: plain text, CSV, Markdown and JSON as written, HTML converted, a calendar file as written, or an attached email as `get_message` reads one. The text arrives inside the same marked boundaries as a body, with hidden text removed, under the same budget and `offset`. Any other type, and anything over 5 MB, is refused.

### Changed

- `send_draft`, `delete_permanently` and `delete_label` ask once in Claude Code, not twice: for a client that can ask, they drop the `requiresUserInteraction` mark, and the server's question is the confirmation. Every other write keeps the mark. A Claude Code `Elicitation` hook that accepts now confirms these alone.

### Fixed

- A `search_threads` row's date, snippet and sender are its newest message that is not a draft or in the trash. A draft reply made the row show the draft's date and credit its text to this account.
- An address in a result quotes its name unless the name is plain words, so it can be given back as one recipient. A name with a comma split in two, and a name holding an address could pass for it.
- `download_attachment` refuses, as `[unsupported]`, a part whose content Gmail gives neither inline nor by an attachment id. It wrote such a part as an empty file.
- A message or attachment read stays within `budget_chars` when the mail holds thousands of links whose text names another site: the note pairs as many as fit an eighth of the budget and counts the rest. Such mail made a read many times its budget.
- Invisible characters in an attachment's type or invitation method are removed, and counted in `hidden_chars_removed`. They reached the result as written.
- On Windows, `time_zone` and an invitation's time zone work without Go installed. Every zone was refused there; the binary now carries the zone database, about 400 KB, and reads it only when the system has none.

### Security

- A malformed `From` or other address header never shows an address written inside its quoted name or a comment as the address. `"Boss <boss@bank.example>" <attacker@evil.example> x` read as from the boss; it now reads as from the attacker, and a header with two candidates shows none.
- `rfc822:` takes exactly one Message-ID, written `<local@domain>`, and refuses anything else as `[invalid]` before searching. Text after the id went into Gmail's search, so a forward, reply or quote could take a message the caller never saw.
- Built with Go 1.27.2 and `golang.org/x/net` v0.60.0, which fix ten advisories in `net/http`, its HTTP/2 code, `crypto/tls` and `net/textproto` that `govulncheck` found reachable from this server.

## [2.1.0] - 2026-10-06

### Added

- `plain_only` on `create_draft` and `update_draft` keeps a draft as plain text alone, for a mailing list that refuses HTML; `update_draft` otherwise keeps a draft's shape.
- A web address in a signature, or in the HTML version of a draft, is a link whose text is the address.

### Fixed

- A draft written without `body_html` also carries an HTML version made from its text, so when a person sends it from Gmail its paragraphs flow to the reader's screen instead of arriving wrapped at 70 columns; `update_draft` makes it again from a new body.
- `update_signature` keeps every space of a run of spaces; a run of three lost one.

### Changed

- The `body` of `create_draft`, `update_draft` and `set_vacation` asks for each paragraph on one line, with a blank line between paragraphs.

## [2.0.4] - 2026-10-01

### Fixed

- A dry run of `modify_labels`, `trash` or `restore` reports the labels each item would have after the write, instead of an empty `labels_after` that read as every label being removed.

## [2.0.3] - 2026-09-30

### Fixed

- A question breaks a bare domain a fuzzy Markdown linkifier would link, like `evil.com`, not only one followed by a path; a file name like `report.pdf` and an email address stay as they are.
- The live driver's transcript masks label and filter ids.

## [2.0.2] - 2026-09-29

### Fixed

- A question the server asks breaks a link that follows an underscore or punctuation, sits right after another link, or is written in a non-Latin script, so no client draws it as a link.
- An answer other than accept is refused before the call reads the mailbox again, and a failure after an accept is never reported as "nothing was written".

## [2.0.1] - 2026-09-29

### Fixed

- A question the server asks quotes mail text between backticks, not double quotes, with a blank line between lines, so a client that draws it as Markdown, such as VS Code, shows the text as written rather than as links or emphasis.

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

[Unreleased]: https://github.com/mmedum/google-mail-mcp/compare/v2.2.0...HEAD
[2.2.0]: https://github.com/mmedum/google-mail-mcp/compare/v2.1.0...v2.2.0
[2.1.0]: https://github.com/mmedum/google-mail-mcp/compare/v2.0.4...v2.1.0
[2.0.4]: https://github.com/mmedum/google-mail-mcp/compare/v2.0.3...v2.0.4
[2.0.3]: https://github.com/mmedum/google-mail-mcp/compare/v2.0.2...v2.0.3
[2.0.2]: https://github.com/mmedum/google-mail-mcp/compare/v2.0.1...v2.0.2
[2.0.1]: https://github.com/mmedum/google-mail-mcp/compare/v2.0.0...v2.0.1
[2.0.0]: https://github.com/mmedum/google-mail-mcp/compare/v1.2.0...v2.0.0
[1.2.0]: https://github.com/mmedum/google-mail-mcp/compare/v1.1.1...v1.2.0
[1.1.1]: https://github.com/mmedum/google-mail-mcp/compare/v1.1.0...v1.1.1
[1.1.0]: https://github.com/mmedum/google-mail-mcp/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/mmedum/google-mail-mcp/compare/dc26f63...v1.0.0
