# Architecture — google-mail-mcp

**Status: design only, 2026-09-24. Nothing is built or tagged.** This
document is the whole of phase −1: the platform facts, the design bets,
a verdict on all 79 published API methods, the phase plan and the
spikes that must answer before the phases that depend on them. Phase 0
(§16) starts from this file and `CLAUDE.md` alone, after the maintainer
says "go".

## 1. Mission and scope

A production-grade Go MCP server for Gmail, distributed to other people.
One binary, stdio, per-user OAuth against the person's own Desktop OAuth
client, no hosted deployment.

The server works **inside one mailbox**: finding mail, reading threads
and messages, attachments, labels, drafts, and — only when the person
opts in — sending. It stops at the edge of the message. A calendar
invitation is an `.ics` attachment here; the event belongs to a server
built on the Calendar API. A linked Drive file is a URL here; the file
belongs to a Drive server. Mailbox administration — delegation,
forwarding, send-as identities, S/MIME and client-side-encryption keys —
is out of scope, and §8a says why method by method.

### Why build it (research summary, checked 2026-09-24)

Four servers were surveyed; their issue trackers are where §3 comes
from.

- **Google's hosted Gmail MCP server** (`gmailmcp.googleapis.com`,
  configured through the Workspace MCP guide). Remote HTTP; needs the
  person's own Cloud project with the Gmail MCP API enabled. Requests
  `gmail.readonly` and `gmail.compose`, and ships **no send, no trash
  and no delete tool** — although `gmail.compose` can send (§2.10). Its
  surface is search, read, drafts and labelling. It is the strongest
  evidence that registration, not scope, is how a Gmail server withholds
  sending, and §4.2 takes the same position.
- **The claude.ai Gmail connector.** Hosted; asks for approval before
  each write by default; attachment metadata only.
- **The most-starred community server.** Archived, unmaintained since
  2025-08. Permanent delete registered by default (and failing, because
  it never requested the scope that allows it), attachments attached
  from arbitrary local paths, tokens in a plaintext file.
- **The most-starred Workspace-wide community server.** Active and
  broad; its Gmail tracker is the richest record of what goes wrong:
  threading, MIME, double sends, token churn and prompt injection.

What none of them offers together, and what this server is for: a local
stdio binary with the token in the OS keyring; **trash and restore as
the removal path, with permanent deletion structurally absent** unless
enabled; sending that is **absent unless enabled and then only through
a draft**; replies that thread every time; MIME that round-trips
non-ASCII names, subjects and filenames; attachments that move only
through one configured directory; reads with a stated budget; a
`list_changes` that says when its cursor expired rather than returning
nothing; and releases that can be verified from outside.

Google's hosted server is the one to watch. If it gains trash, local
attachment transfer and bounded reads, most of this list is answered by
Google, and §17.6 records the question.

### Non-goals

- **Push.** `watch` delivers to a Cloud Pub/Sub topic; a stdio process
  owns none. `list_changes` polls `history.list` (§7.6).
- **Bulk mailbox operations.** No "archive everything from X", no "empty
  trash", no mass relabel by query. §4.7.
- **Mail settings that redirect mail.** Auto-forwarding, forwarding
  addresses, filters that forward, delegation, send-as. §4.1, §8a.
- **Mailbox migration.** `messages.import` and `messages.insert`
  deliver mail that was never sent. §8a.
- **Rendering HTML.** HTML becomes text; nothing is fetched. §4.1.
- **A contacts directory.** Addresses come from the caller or from the
  message being answered; resolving a name to an address belongs to a
  People API server.
- **Workspace administration**, domain-wide delegation and service
  accounts.

## 2. Hard constraints from the platform

Each is checked against the Gmail v1 discovery document (revision
20260917, fetched 2026-09-24) or a Google page, and §18 has the row.

1. **Quota is counted in units per user per minute.** 6,000 units per
   user per project per minute, 1,200,000 per project per minute, for
   projects created on or after 2026-05-01; older projects keep their
   previous quotas. Costs differ by method by a factor of 100:
   `labels.list` 1, `history.list` 2, `messages.list` 5, `modify` 5,
   `threads.list` 10, `drafts.create` 10, `messages.get` 20,
   `attachments.get` 20, `trash` 20, `threads.get` 40, `batchModify`
   50, `drafts.send` 100. A search that opens twenty threads costs 810
   units. §4.9.
2. **Rate-limit signals are several, and one of them is a sending
   limit.** 403 `userRateLimitExceeded` and `rateLimitExceeded` ask for
   backoff; 403 `dailyLimitExceeded` is the project's day; **429 covers
   per-user daily limits including the mail-sending limit, which may
   arrive minutes late**, and an undocumented per-user concurrency
   limit. Google's backoff is `min(2^n + jitter, 32–64 s)`.
3. **Sending is not idempotent.** `messages.send` and `drafts.send`
   take only `userId`; there is no request id and no documented
   deduplication. A retried POST whose first attempt landed is a second
   delivery to every recipient. §4.3.
4. **There is no optimistic concurrency.** No `etag` field and no
   `If-Match` anywhere in gmail/v1. `drafts.update` is PUT-only and
   replaces the whole draft; the last writer wins. §4.4.
5. **Threading needs three things at once.** The message's `threadId`
   set, `In-Reply-To` and `References` per RFC 5322, and a matching
   `Subject`. Any one missing and the reply may start a new
   conversation. The headers carry RFC 5322 `Message-ID` values, which
   are not the API's message ids. §4.5.
6. **Size.** Media upload through `messages.send` and every `drafts.*`
   write is capped at 36,700,160 bytes (35 MB) by the discovery
   document; a consumer account's attachment limit is 25 MB. Google
   recommends simple upload only up to 5 MB, and documents no cap on the
   JSON `raw` path. §7.4.
7. **`raw` is base64url of RFC 5322.** Header lines must be at most 998
   characters and should be at most 78; an RFC 2047 encoded-word is at
   most 75. Non-ASCII in a header without encoding is refused as an
   invalid header. §4.10.
8. **A history cursor expires.** Records are "typically available for
   at least one week" and "might be significantly shorter"; a stale
   `startHistoryId` is **404**, and the client "must perform a full
   sync". A 404 here is not "no such mailbox". §7.6.
9. **Search is the web UI's syntax, with differences.** `q` accepts most
   operators, reads dates as midnight **Pacific** time, and does no
   alias expansion. `q` is refused under `gmail.metadata`. `maxResults`
   defaults to 100 and caps at 500. §6.3.
10. **Scopes cannot separate drafting from sending.** `gmail.compose`
    ("Manage drafts and send emails") and `gmail.modify` both grant
    `messages.send` and `drafts.send`. Permanent deletion
    (`messages.delete`, `messages.batchDelete`, `threads.delete`)
    accepts **only** `https://mail.google.com/`. §4.2, §4.6.
11. **Every useful Gmail scope is restricted.** `gmail.readonly`,
    `gmail.compose`, `gmail.modify`, `gmail.metadata`, `gmail.insert`,
    the two settings scopes and `https://mail.google.com/` are
    restricted; `gmail.send` is sensitive; `gmail.labels` is not. A
    person's own client in **Testing** status gets refresh tokens that
    **expire after 7 days** and is capped at 100 test users; a Google
    account holds at most 100 refresh tokens per client, oldest
    invalidated first. §10.
12. **Labels.** System label ids are their names (`INBOX`, `UNREAD`,
    `STARRED`, `IMPORTANT`, `SPAM`, `TRASH`, `SENT`, `DRAFT`,
    `CATEGORY_*`), and Google calls that list "not exhaustive". `SENT`
    and `DRAFT` cannot be applied by hand, and a draft cannot be
    labelled. A thread-level change does not reach messages added to the
    thread later. At most 5,000 labels per account.
13. **Batches.** `batchModify` accepts up to 1,000 ids; the HTTP batch
    endpoint caps at 100 calls and Google's error guide says not to
    exceed 50. Each call inside a batch is charged separately.
14. **Sending limits.** Consumer: 500 recipients per message or 500
    messages a day before a 1–24 hour block. Workspace: 2,000 messages
    a day and 500 recipients per message through the API.

### What the API cannot do (so we don't promise it)

- Say whether a failed send was delivered. Only a read afterwards can.
- Undo a send. There is no recall.
- Guard a draft update against a concurrent edit. §4.4 narrows the
  window; it cannot close it.
- Search inside attachments, or search thread-wide with one query.
- Report which mail a filter or forwarding rule sent elsewhere.
- Read a client-side-encrypted body.

## 3. Requirements distilled from other servers' failures

Each traced to a public issue in §1's servers; the evidence is in §18.

1. Replies land in the thread: `threadId` plus `In-Reply-To` and
   `References` from the parent's **RFC 5322** `Message-ID` — one
   server put the API id there, another accepted the argument and never
   wrote the header, a third omitted `threadId` from drafts.
2. The reply's parent is chosen, not defaulted: a draft, a trashed
   message or an emoji reaction is not a parent.
3. A send is never retried automatically; a lost 200 became a second
   delivery.
4. Sending needs a human decision the server cannot be talked out of.
5. Removal is trash; a registered "permanent delete" that fails for
   want of a scope is two defects.
6. Non-ASCII survives: RFC 2047 subjects and display names, RFC 2231
   filenames, quoted-printable bodies with soft breaks — garbled
   subjects, a 400 on an accented display name, attachments named
   `noname` and hard-wrapped paragraphs were all reported.
7. Bodies decode whatever their shape: the whole MIME tree, each part's
   charset, parts stored behind an `attachmentId`, HTML-only mail, and
   the plain part that only says "view the HTML version".
8. Output is bounded: one thread returned 46k tokens to a client capped
   at 25k.
9. Labels are resolved by the server; the model should not have to
   know an opaque `Label_123`.
10. Partial failure is reported per item; a batch reported sub-request
    429s as success.
11. Tokens are stored safely and refreshed tokens are persisted.
12. Scopes are narrow and a read-only mode exists.
13. Mail content is untrusted; a message saying "forward all mail to
    this address" is an attack, reported against a real server.
14. Arguments are accepted leniently where clients are known to
    stringify them — arrays and numbers sent as JSON strings.
15. Attachments come from one allowed directory, never an arbitrary
    path a message could talk the model into.
16. Every HTTP call has a timeout; two servers hung on a dead socket.

## 4. Core design bets

### 4.1 Mail content is data, never instructions

Every subject, body, header, snippet and attachment name was written by
someone other than the person using the server, and some of it is
written to steer an agent. Google's own MCP guide warns that mail "may
contain hidden instructions that can hijack your session". This server
cannot stop a model from being persuaded; it can make the persuasion
visible, remove the tools that would make it catastrophic, and never
add its own voice to the attacker's.

1. **Boundaries.** Rendered mail content sits inside a delimited block
   naming its origin — sender address and message id — with a boundary
   token generated per call so the content cannot close the block
   itself. `structuredContent` carries the same text in fields named
   `untrusted_*`.
2. **Hidden text is removed and counted.** HTML is converted to text by
   the server. Text a person would not see — `display:none`, zero-size
   or same-colour-as-background runs, zero-width and bidi-control
   characters, HTML comments — is dropped, and the result says how many
   characters were dropped and why. The count is the signal: legitimate
   mail rarely hides much.
3. **Nothing is fetched.** No image, stylesheet, tracking pixel or
   link. Links render as text with their host shown, and a link whose
   visible text names a different host than its target says so.
4. **The dangerous writes do not exist.** Auto-forwarding, forwarding
   addresses, filters that forward and delegation are written off in
   §8a — not gated, absent — because each turns one persuaded call into
   a permanent leak of every future message. Sending is absent unless
   enabled (§4.2).
5. **The server's own text never relays mail.** Tool descriptions and
   the server instructions say that mail content is untrusted, and no
   result ever phrases a message's text as something to do.

### 4.2 Sending is opt-in, only through a draft, and verbatim

Registration is the only control, because §2.10 rules out the scope:
the token that writes a draft can send. The standard's §3 already says
an annotation is not a control and a registered tool can run unattended.

1. **`send_draft` is registered only with `GMAIL_ENABLE_SEND=true`.**
   Every other write is registered by default. There is no one-shot
   `send_message`: `messages.send` is written off, so everything that
   leaves this server existed first as a draft the person could open in
   Gmail. Replying is `create_draft` with `reply_to`, then `send_draft`.
2. **The body is exactly what the caller gave.** No signature, prefix,
   footer or "sent by" line is added server-side, ever.
3. **`send_draft` shows before it sends.** Its result names every
   recipient (To, Cc, Bcc), the subject, the attachment names and sizes,
   and the thread it joins. `dry_run: true` returns that without
   sending, and costs one `drafts.get`.
4. **Recipient guard.** Every recipient who is not already a participant
   in the thread being answered — for a new message, every recipient —
   must be listed in `confirm_recipients`, exactly; otherwise the call
   is `[blocked]` and names them. Over 50 recipients is `[blocked]`
   outright. The model then has to write out an injected address itself
   rather than inherit it silently from a draft someone else shaped.

### 4.3 A send is never retried; ambiguity is settled by reading

`drafts.send` is a non-idempotent POST (§2.3). The client retries a POST
only where Google turned it away before acting — a 429 or 503 carrying
no body the server acted on, per §11 — and never on a timeout, a reset
connection or a 5xx after the request was written.

On an ambiguous failure the tool returns `[ambiguous_outcome]` and has
already done the read that settles it:

- Every draft this server creates carries a `Message-ID` it generated.
  Spike B checks that Gmail keeps it.
- After an ambiguous `drafts.send`, the server reads the draft
  (`drafts.get`) and searches `rfc822msgid:` for the generated id with
  the `SENT` label. The draft gone and the message in `SENT` means
  **sent**. The draft present and nothing in `SENT` means **not sent**.
  Anything else stays **unknown**, and the result says so and says not
  to resend.
- This state is reported, never acted on: the server does not resend on
  "not sent" either. The person decides.

### 4.4 A write never destroys what it cannot see

**Labels are patched.** `labels.update` (PUT) is written off.

**A draft update is a read-merge-write against a witness.** Gmail has no
ETag (§2.4), and `drafts.update` replaces the whole draft. So:

1. `get_draft` returns, beside the content, the id of the message
   currently inside the draft. Spike A checks that `drafts.update`
   changes that id; if it does not, the witness becomes a hash of the
   draft's raw bytes.
2. `update_draft` requires that witness. It re-reads the draft, compares
   the witness and refuses with `[stale]` if the draft moved; otherwise
   it merges the fields given onto what it read and PUTs the result.
3. **Omitted means unchanged.** An attachment, a Cc or a header the
   caller did not mention is carried over, never dropped by omission.
   Removing an attachment is an explicit `remove_attachments` list.
4. The window between the re-read and the PUT is not closed, and the
   result does not claim it is. This is a recorded deviation (§17b).

### 4.5 Threading is constructed, never hoped for

A reply is built by the server from the parent message, never assembled
by the model:

- `reply_to` takes a **message id**. A thread id is accepted, and the
  server picks the newest message that is not a draft, not in `TRASH`
  and not a reaction, and names the chosen message in the result.
- The server reads the parent's `Message-ID`, `References`, `Subject`,
  `From`, `Reply-To`, `To` and `Cc`, and writes `In-Reply-To` = the
  parent's `Message-ID`, `References` = the parent's `References` plus
  that id, `threadId` = the parent's thread, and `Subject` = the
  parent's, with a `Re:` prefix only if there is none.
- `reply_all: true` expands recipients from the parent, drops the
  account's own addresses (from `sendAs.list`) and says who was added.
- The result states whether the draft landed in the parent's thread, by
  reading the created draft's `threadId` rather than asserting it.

### 4.6 Removal is trash

`trash` and `restore` are ordinary writes, registered by default, because
trash is recoverable for 30 days and gating it would teach people to set
the flag that also arms permanent deletion. A gate that everyone turns
on protects no one.

`delete_permanently` and `delete_label` are **unregistered** unless
`GMAIL_ENABLE_DESTRUCTIVE=true`, and each call still needs
`confirm: true`. That flag is also **the only setting that requests
`https://mail.google.com/`** (§2.10), so a default token cannot delete
permanently even if a tool were registered by mistake. Turning the flag
on requires a fresh `login`, and `doctor` says so.

### 4.7 Writes take ids, never a query, and at most 100 at a time

A search is a read the model has seen; a write names the messages it
touches. `modify_labels`, `trash`, `restore` and `delete_permanently`
take explicit message or thread ids — up to 100 per call, far below
Google's 1,000 — and every one takes `dry_run`. There is no
"apply to all results" parameter, because that is how a single injected
instruction empties a mailbox.

Per-item outcomes are reported per item. A call that trashed 97 of 100
says which three failed and why; it never reports the batch as one
success.

### 4.8 Reads are bounded and say what they left out

Every read has a budget in characters, stated in the result. A thread
renders newest-first up to the budget; quoted text and signature blocks
in each message are collapsed to a line saying how much was collapsed;
messages beyond the budget are listed by id, sender and date with a
cursor. A long body is cut at a paragraph boundary with a marker and the
offset to continue from. Listings use `format=metadata` with a fixed
header set, never `full`.

The rule is that **a read never silently returns less than it found**:
every omission is named and continuable.

### 4.9 Quota is counted in units, not requests

A request-rate limiter is wrong by up to 100×, because §2.1's costs
differ that much. The client carries Google's unit cost per method,
spends it against a per-user budget of 6,000 units a minute, and waits
rather than earning a 429. Each tool result and each log line reports
the units the call spent. `search_threads` defaults to 20 results
because 20 thread reads is 800 units, and says so in its description.

### 4.10 MIME is ours, and it round-trips

`internal/mime` parses and builds RFC 5322 with MIME (RFC 2045–2047) and
RFC 2231 parameters, on the standard library's `mime`, `mime/multipart`,
`mime/quotedprintable` and `net/mail`. It is the core package of this
server, as the time model is of a calendar server, and it is built and
table-tested first (phase 0). The property that matters is that
**what the server builds, it can parse back to the same fields**, and a
fuzz test holds that over generated names, subjects, bodies and
filenames in scripts nobody on the project reads.

### 4.11 Own wire types, raw REST

Hand-written structs for the fields used, REST called directly. §8b
records every field the discovery document publishes with a verdict.

### 4.12 Results say what changed

Every write reports the state it produced, read back rather than
assumed: labels before and after per message, the draft's thread, the
recipients a send reached, the messages a trash moved. A write that
changed nothing says so — "already in `TRASH`" — rather than reporting
success.

## 5. Module layout

As `CLAUDE.md` "Where things go"; the staleness gate holds that list
against `go list ./...`, so this section does not repeat it.

### 5a. Shared machinery: what the current version must carry

The sibling servers improved shared machinery wherever a problem
happened to surface, so on 2026-09-24 no single sibling held the current
version of any large piece. Each row below lists every fix the
up-to-date component must carry, merged from all six. Phase 0 builds to
this list, and a gate or test is named for each claim where one exists.
When a sibling fixes something shared, it is added here with the date.

**Pins, checked against upstream on 2026-09-24, not copied from a
sibling.** Three siblings' newest pins already trailed upstream:

| Tool | Version |
|---|---|
| Go | 1.27.1 (`go-version-file: go.mod` in CI) |
| MCP Go SDK | v1.8.0 |
| golangci-lint | v2.13.2 |
| goreleaser | v2.18.2 |
| cosign | v3.1.3 (`cosign-release:` on the installer) |
| syft | v1.52.0 (`syft-version:` on the installer) |
| mcp-publisher | v1.8.1, its own sigstore bundle verified before extraction |
| gitleaks | v8.30.1 |
| govulncheck | v1.8.0 |
| go-licenses | §17.3 |
| actionlint | v1.7.12, run through `go run`, not Docker |
| codeql-action | v4.38.1 |
| mcpb manifest schema | v2.1.2 tag, `manifest_version` 0.3 |

Every action pinned to a full SHA with the version in a trailing
comment, re-resolved at scaffold time.

**CI and release.**

| Component | Must carry |
|---|---|
| `ci.yml` | every step is a `make` target, so parity holds by construction; ubuntu/macos/windows matrix; `defaults.run.shell: bash`; `timeout-minutes` on every job; `permissions: contents: read` at top; `persist-credentials: false` on every checkout; `concurrency`; `workflow_dispatch`; a full-history secrets job on every PR; `goreleaser check` and actionlint as targets |
| `codeql.yml` | push, PR, weekly; bash default; a timeout |
| `release.yml` | refuses a tag whose CI run is not green; write permission raised only in the goreleaser job; `go test` before signing; notes lifted from the CHANGELOG into `$RUNNER_TEMP`; **a reproducible-build check that builds one target twice and compares hashes**, which is the only thing that proves §9's byte-identical claim; `subject-path` naming the archives, `checksums.txt` and `dist/*.mcpb`, comma-separated |
| `publish-mcp.yml` | its own workflow, callable from the release and by dispatch for an old tag; **`cosign verify-blob` on `checksums.txt` with the certificate identity pinned to this repository's `release.yml` at the exact tag, before the hash is read**; `server.json` generated at publish time and validated against the vendored registry schema |
| `.goreleaser.yaml` | `go mod download` in the hook, never `tidy`; `{{ .Version }}` through ldflags; `-trimpath`; `mod_timestamp: {{ .CommitTimestamp }}`; `universal_binaries` kept out of the archives by `ids`; the bundle packed in the universal binary's post hook and named in both `checksum.extra_files` and `release.extra_files`; a keyless cosign `--bundle` over the checksums; an SBOM per archive; a footer naming the verification commands that actually match the artifacts |
| `dependabot.yml` | weekly gomod and actions; `github/codeql-action*` grouped so init and analyze move together; gomod grouped; no labels that do not exist |
| `Makefile` | every tool pinned, goreleaser included, so `pins` compares the Makefile's pin with the workflow's; `release-rehearse`; `tidy` as `go mod tidy -diff`; `hooks` setting `core.hooksPath`; no comment claiming to be "everything CI runs" |
| pre-commit | `.githooks/pre-commit` running `go run ./scripts/gates precommit`; no Python pre-commit framework |
| `.golangci.yml` | `default: none` with an explicit set incl. gosec (minus G104), gocritic, unconvert, prealloc, makezero, govet minus fieldalignment/shadow; forbidigo on `fmt.Print*` and `os.Stdout` with `analyze-types`, excluded for `cmd/` and `scripts/`; `run.build-tags: [live, evals]` so tagged code is linted |
| `.gitleaks.toml` | Google OAuth client id, `GOCSPX-` secret and `1//0` refresh-token rules; allow-list for `example`, `test`, `invalid` domains and `go.sum` |
| `.gitattributes` | `* text=auto eol=lf`; `*.png`, `*.pdf`, `*.eml` binary |
| issue and PR templates | the bug form asks for `doctor`, version, install method, client and flag state, explains why those are safe to paste, and says what never to paste — "a tool result is mail too" |
| `packaging/mcpb/manifest.json` | `$schema` at the pinned tag; `manifest_version` 0.3; placeholder version `0.0.0-dev`; `claude_desktop >= 0.10.0`; per-platform commands; a `support` URL; a `long_description` saying the bundle does not log you in |
| Linux launcher | generated by the packer from the same staging table that names the binaries, so the launcher's names cannot drift from the packer's; `exec`s; unknown architecture to stderr, non-zero; a missing or non-executable binary named |

**Gates (`scripts/gates`, one command, one registry).**

| Gate | Must carry |
|---|---|
| registry | each entry declares `manual`, `inCheck` or `inRelease`, with the zero value refused by a test; arity declared |
| `parity` | targets mapped to CI steps by recipe, Makefile variables expanded, `run: \|` blocks read, a step's `name:` not counted as running it; every `vet -tags=` both ways; release-only entries excused by name with a non-empty reason; floors on gates, prerequisites and matched lines |
| `checklist` | `CLAUDE.md`'s definition-of-done block against the `check:` prerequisites, both ways |
| `pins` | every action classified as installer or not, an unknown one failing; installer tool pins required; the Makefile's goreleaser pin equal to the workflow's; workflows parsed as YAML, not line-split; `${{ env.X }}` resolved; pre-commit and CI gitleaks versions equal |
| `staleness` | prose counts held against the lists beside them; the package map against `go list`; every repository path a document names exists, with a floor; every env var and gate documented; the status line against the newest tag or CHANGELOG heading; no version in prose; **the OAuth scope block in `docs/gcp-setup.md` generated from `internal/scopes` for every mode and compared exactly** — not a list checked for mentions |
| `changelog`, `changelog-links` | a PR adds an entry unless it is a release cut; every version heading has a link reference |
| `release-notes` | the CHANGELOG section lifted verbatim, refusing an empty one |
| `api-coverage` | offline, three directions; the row bound to its method by verb and path from the client's AST; out-of-scope methods derived from discovery scopes; a minimum length on a write-off reason; floors on methods, verdicts and client calls |
| `api-diff` (manual) | one fetch covering methods and fields; written through a temp file and rename; a network failure leaves the committed snapshot untouched, **held by a test against a closed port** |
| `api-fields` | recursive over inline objects; every struct the wire package declares judged; floors on published and modelled fields |
| `schema-diff` | a committed baseline so it works before the first tag; the tool surface **and resources**; fails on a removed tool, a lost input or output field, or a new required input; the whole `mcp.Tool` dumped, `_meta` and output schema included; a floor on tools |
| `leaks` | allow-list rules, each anchored on a shape generated fields cannot take (an `@` with a dotted domain; a Gmail URL prefix; a keyword before a hex id; the OAuth shapes); every allow-list entry carries a reason, asserted; tracked files and untracked-unignored files both scanned; a tracked binary (ELF, Mach-O, PE magic) is a finding; history mode covering blobs, commit messages and tags with a derived floor; findings printed redacted |
| `transcript` | drivers cannot reach `os.Stdout`, `os.Stderr` or `log.*`; exactly one exempt package with exactly one write site, which redacts; an unlisted driver fails; a listed directory that is missing fails; a floor on mentions |
| `live-cover` | per tool **option**, not per tool, recorded at run time through the stdio client; waivers in a TSV with a reason each, a ceiling, and a failure on a waiver whose option is now driven |
| `smoke` | the shipped binary over stdio at two protocol revisions, the current one asserted present in the SDK's supported list; resources and templates; **stdin closed the moment the last message is written**, so an abrupt disconnect with a request in flight must exit 0 (-32004/-32003 matched by code, never by message text); awaited ids derived; the child reaped on EPIPE; the Windows `.exe` path |
| `classes` | the closed vocabulary of §6.5 derived from the code and asserted both ways; duplicates found by count, not by adjacent comparison; a floor |
| `coverage` | per package, scored on its own files and not its subpackages', `cmd/` included; each exemption with a reason; floors on packages and profile lines |
| `outcomes` | every write's result states its outcome on every branch |
| `mcpb` | the committed manifest validated against the **vendored** schema chosen by its own `manifest_version`, with a property-count floor on the vendored file; `$schema` matched as a whole URL at a full tag or SHA; `manifest_version` equal to the URL's; a floor at 0.3; unknown keys preserved by decoding twice; entry point, every command and every override naming a staged file; every override a claimed platform; each claimed platform spawning the file staged **for it**; every `${user_config.x}` in a composed value declared; the no-login sentence; a `support` URL |
| `mcpb-pack` (release) | decode–encode stamping, never text substitution; `0755` on every staged binary; a fixed mtime asserted against a literal; globs matching exactly one file; the staged binary's `--version` read back, as the fifth version check; the packed archive read back |
| `release` | goreleaser's config held against the release workflow: universal binaries via `ids`, the bundle in both `extra_files`, `subject-path` with a valid separator, signing and SBOM pins |
| `server-json` | the hash taken from `checksums.txt` with exactly one `.mcpb` row, **after** the signature check; validated against the vendored registry schema; the registry's unwritten rules — HTTPS, release URL, identifier shape — held by a test; `schema-refetch` (manual) compares the vendored bytes' hashes upstream with a timeout and writes nothing |

**Server core.**

| Component | Must carry |
|---|---|
| `cmd` | `run(args, stdin, stdout, stderr, env)` so the serve path is testable; `help`/`-h` exit 0; an unknown command prints usage to stderr and exits non-zero; errors printed through a redactor that masks addresses and client ids; disconnect matched by JSON-RPC code; the token warmed off the startup path; version from ldflags with a `debug.ReadBuildInfo` fallback |
| `login`/`logout`/`status`/`doctor` | `--no-browser` printing the URL and the exact `ssh -L` line; `status --json`; the account and client-secret path masked in every output; `doctor` walking client JSON → token → granted scopes → API enabled → one `getProfile`, naming what is missing; the keyring replaced package-wide in tests by `TestMain`, with a decoy test proving the replacement happened |
| `auth` | loopback on `127.0.0.1:0`; PKCE S256; `state` checked; a bounded HTTP client on refresh; tokeninfo errors stripped of their URL, which carries the token; `invalid_grant` mapped to "log in again"; granted scopes stored |
| `scopes` | one source of truth per mode; an implication table (`https://mail.google.com/` ⊃ `gmail.modify` ⊃ `gmail.readonly`, `gmail.compose`, `gmail.labels`) with `Satisfied` and `Missing`; the generator for `docs/gcp-setup.md` |
| `credentials` | resolution **env → keyring → file**, documented as that order (four sibling documents describe the fallbacks and got the precedence wrong); a silent keyring told apart from a missing login; a warning on every use of the plaintext file |
| `fileperm` | 0600 on Unix; an ACL on Windows restricting the file to the current user, because 0600 there protects nothing |
| `userconfig` | profiles; the config-dir override refused outside the home directory, compared by real path |
| `config` | `Define(fs, env)` and `Build()` with errors joined; `GMAIL_` prefix; `READ_ONLY`, `ENABLE_SEND`, `ENABLE_DESTRUCTIVE`, `LOCAL_DIR` (refused if it does not exist, naming the variable); base-URL overrides for tests; an exported list of every variable, which the staleness gate reads |
| `redact` | account, address, path and client-id masking for product output; id truncation and the redacting printer for maintainer tooling |
| `server` | per-call log line with method, tool, outcome, milliseconds and units; the SDK's own logger only at debug; instructions built from the configuration, so read-only and send-enabled servers say different things; the schema dump taking the SDK version from build info rather than a constant (two siblings' constants are already wrong) |
| `app` | startup assembly reachable without `main`, used by the schema dump; settings that redact the token when logged |
| `tools` | one `register` deciding annotations, gating, `_meta`, the dry-run context and the rendering from one `Kind`; an explicit output schema with `date-time` for times; `Content` set so the SDK does not duplicate the JSON |
| `gapi` | write and repeatability derived from the HTTP method, POST failing closed, declared exceptions only; a POST retried only on a turned-away status; `Retry-After` honoured as a minimum; full-jitter backoff; the unit budget of §4.9; a body cap; transport errors stripped of the URL; **a context under which the client refuses every write**, which is what `dry_run` is; a closed `Class` type with `Retryable()`; addresses masked in API error text; a per-call counter of requests and units |
| logging test | every registered tool driven with canary values at debug; asserts the logs are non-empty and contain no canary |

## 6. Addressing

### 6.1 Messages, threads and drafts

By the ids Gmail gives them, which are stable and opaque, and which the
standard's §2 allows ("a stable id the platform gives you"). The model
never sees an index or a position. A result that lists messages gives
each id beside it. A draft has two ids — the draft's and the message
inside it — and every draft result names both, labelled, because
confusing them is how a reply lands on the wrong message.

RFC 5322 `Message-ID` values are shown as `rfc822_message_id` and
accepted wherever a message id is, prefixed with `rfc822:`; the server
resolves them through `rfc822msgid:` search and refuses `[ambiguous]` if
two messages carry one (mailing lists do that).

### 6.2 Labels

By id or by exact name. System labels by their id in any case (`inbox`
is `INBOX`). A user label's name resolves through `labels.list`, which
is cached per call, not per process, because a label renamed in the web
UI between two calls must not resolve to the old one. Whether Gmail
treats two names differing only in case as one label is spike I; until
it answers, a case-insensitive match that finds two labels is
`[ambiguous]` with both ids.

### 6.3 Time and search

`q` passes through verbatim: it is the person's own search language,
and rewriting it would make the server's results differ from the web
UI's for the same text. Two parameters sit beside it because §2.9 makes
dates in `q` read as Pacific midnight: `after` and `before` accept
RFC 3339 instants or dates plus a `time_zone`, and the server appends
them to `q` as `after:<epoch>` / `before:<epoch>`, stating the instants
it used. A date written inside `q` is left alone and the description
warns about the Pacific reading.

### 6.4 Addresses

Parsed with `net/mail`. A display name with non-ASCII is RFC 2047
encoded when building and decoded when reading. An address the parser
refuses is `[invalid]` naming which argument, never passed through.

### 6.5 Error classes

Closed vocabulary, derived from the code by `scripts/gates classes` and
asserted in both directions. The standard's six, plus six this API
forces:

| Class | Means | Caller should |
|---|---|---|
| `invalid` | malformed or under-specified arguments, or too large (§2.6) | fix the arguments |
| `not_found` | no such message, thread, draft or label | check the id |
| `auth` | not signed in, token revoked or expired (§2.11), or scope missing | run `login` |
| `forbidden` | signed in, but the organisation or account refuses this | ask an administrator |
| `conflict` | the state refuses it: label name taken, `SENT` applied by hand | read and reconsider |
| `stale` | the draft moved since the witness was read (§4.4) | re-read and retry |
| `ambiguous` | a label name or `rfc822:` id matched more than one | pass an id |
| `blocked` | a guard refused what the API would have allowed (§4.2, §4.7) | pass the override, or don't |
| `rate_limited` | §2.2's per-user, project or sending limits | wait; the message says how long |
| `unavailable` | a transient upstream failure | retry |
| `unsupported` | the API cannot do this (§2, "cannot do") | see §2 |
| `ambiguous_outcome` | a send may or may not have happened (§4.3) | read the result's verdict; never resend blind |

`rate_limited` distinguishes the sending limit from request-rate limits
in its message, because "wait a minute" and "wait up to a day" are
different advice. `stale` and `conflict` stay apart for the reason a
sibling's log gives: they ask the caller to do different things.

## 7. Reading and writing

### 7.1 Finding mail

`search_threads` (the default choice; threads are what a person reads)
and `search_messages` (when a single message's labels or attachments are
the point) take `q`, `after`/`before`, `labels`, `include_spam_trash`,
`max` (default 20, at most 100) and `page_token`. Each result is an id,
the participants, the subject, the date, labels by name, the message
count, whether it has attachments, and Gmail's snippet — the snippet
inside an untrusted boundary (§4.1). Every listing says whether it is
complete, per the sibling lesson that an empty page with a token is not
the end.

### 7.2 Reading a thread or a message

`get_thread` and `get_message` walk the whole MIME tree (§3.7): the
preferred body is `text/plain` unless it is a placeholder or absent,
then HTML converted to text (§4.1.2); every part's charset decoded;
parts stored behind an `attachmentId` fetched when they are body parts
and listed when they are attachments. Headers shown are From, To, Cc,
Reply-To, Date, Subject, `Message-ID`, and `List-Unsubscribe` when
present; `headers: all` shows the rest. Budget and collapsing per §4.8.
Calendar invitations are listed as attachments with their method
(`REQUEST`, `CANCEL`) and no further parsing.

### 7.3 Attachments

`download_attachment` writes one attachment into `GMAIL_LOCAL_DIR` and
is registered only when that is set. The filename is taken from the
part's RFC 2231 or RFC 2047 name, reduced to a safe base name, and never
overwrites: a clash gets a numeric suffix, and the result names the path
written. Streaming to disk, never held whole in memory. Attachments are
never inlined into a tool result.

### 7.4 Drafts and replies

`create_draft` takes `to`, `cc`, `bcc`, `subject`, `body` (plain text;
`body_html` optional and sent as `multipart/alternative` with the plain
part kept), `attachments` (base names inside `GMAIL_LOCAL_DIR` only; a
path, a `..` or a symlink out of it is `[invalid]`), `from` (one of
`sendAs.list`) and `reply_to`/`reply_all` (§4.5). Up to 5 MB it uses the
JSON `raw` path; above it, multipart media upload; above 35 MB it
refuses before building (§2.6). `update_draft` per §4.4; `delete_draft`
with `confirm: true` (§17.1); `list_drafts`, `get_draft`.

### 7.5 Organising

`modify_labels` adds and removes labels on up to 100 message or thread
ids. Archiving is removing `INBOX`, marking read is removing `UNREAD`,
starring is adding `STARRED`; the description says so rather than
growing a tool per verb, and the result names the verb it amounted to.
`trash`, `restore` (§4.6). `create_label`, `update_label` (patch: name,
colour, visibility). `delete_label` gated.

### 7.6 Changes

`list_changes` takes a `history_id` (from `get_profile` or a previous
call) and returns added, deleted and relabelled messages since, with the
next `history_id`. A 404 is not `not_found`: it is reported as
**"cursor expired — history before this point is gone"** with the
current `history_id` to restart from and a note that changes in between
cannot be recovered (§2.8). It never returns an empty list for an
expired cursor.

### 7.7 Settings, read-only

`get_settings` returns vacation responder, auto-forwarding state and
address, forwarding addresses, IMAP, POP, language and send-as
identities. `list_filters` returns filters, each forward action flagged.
Showing that mail is being forwarded is the useful half of those APIs;
changing it is written off (§4.1).

### 7.8 Sending

`send_draft` per §4.2 and §4.3. Registered only with
`GMAIL_ENABLE_SEND=true`.

## 8. Tool surface

Twenty-three tools: twenty by default, twelve in read-only mode, one
fewer in each when `GMAIL_LOCAL_DIR` is unset.
Annotations come from `Kind` in one place (`CLAUDE.md` rule 11);
`openWorldHint` is true only where the call reaches another person.
`_meta["anthropic/requiresUserInteraction"]` is set on Send and
Destructive kinds, as a signal and not a control.

| Tool | Kind | Registered | Scope needed | Units |
|---|---|---|---|---|
| `get_profile` | Read | always | `gmail.readonly` | 1 |
| `search_threads` | Read | always | `gmail.readonly` | 10 + 40/result |
| `search_messages` | Read | always | `gmail.readonly` | 5 + 20/result |
| `get_thread` | Read | always | `gmail.readonly` | 40 (+20/fetched part) |
| `get_message` | Read | always | `gmail.readonly` | 20 (+20/fetched part) |
| `list_labels` | Read | always | `gmail.readonly` | 1 (+1/label with counts) |
| `list_drafts` | Read | always | `gmail.readonly` | 5 + 20/result |
| `get_draft` | Read | always | `gmail.readonly` | 20 |
| `list_changes` | Read | always | `gmail.readonly` | 2/page |
| `get_settings` | Read | always | `gmail.readonly` | 1 each, 7 |
| `list_filters` | Read | always | `gmail.readonly` | 1 |
| `download_attachment` | Read (local write) | `GMAIL_LOCAL_DIR` set | `gmail.readonly` | 20 + 20 |
| `create_draft` | Write | not read-only | `gmail.modify` | 10 (+20 reply parent) |
| `update_draft` | Write | not read-only | `gmail.modify` | 20 + 10 |
| `delete_draft` | Write | not read-only | `gmail.modify` | 20 + 10 |
| `modify_labels` | Write | not read-only | `gmail.modify` | 5/message or 50/batch |
| `trash` | Write | not read-only | `gmail.modify` | 20 each |
| `restore` | Write | not read-only | `gmail.modify` | 20 each |
| `create_label` | Write | not read-only | `gmail.modify` | 5 |
| `update_label` | Write | not read-only | `gmail.modify` | 5 |
| `send_draft` | Send | `GMAIL_ENABLE_SEND` | `gmail.modify` | 20 + 100 |
| `delete_permanently` | Destructive | `GMAIL_ENABLE_DESTRUCTIVE` | `https://mail.google.com/` | 10 each |
| `delete_label` | Destructive | `GMAIL_ENABLE_DESTRUCTIVE` | `gmail.modify` | 5 |

The staleness gate holds those counts against the table.

Resources, for clients that attach rather than call:
`gmail://threads/{id}` and `gmail://messages/{id}` carry the same text
as `get_thread` and `get_message` under the same budget and boundaries;
`gmail://labels` carries `list_labels`.

### 8a. Every published method, with a verdict

All 79 methods of the discovery document, revision 20260917. Phase 0
moves this table into `testdata/api-coverage.tsv`, and the `api-coverage`
gate holds it from then on; this table is then deleted from here and
replaced by a pointer, so that there is one copy.

Thirty-four used, four gated, three deferred to §17, thirty-eight
written off.

| Method | Verb | Verdict |
|---|---|---|
| `drafts.create` | POST | Used: `create_draft`, including replies (§4.5) |
| `drafts.delete` | DELETE | Used: `delete_draft`, with `confirm: true`. Permanent, but only ever the caller's own unsent text; §17 argues why it is not behind the destructive flag |
| `drafts.get` | GET | Used: `get_draft`, and the witness read before `update_draft` (§4.4) |
| `drafts.list` | GET | Used: `list_drafts` |
| `drafts.send` | POST | Gated: `send_draft`, registered only with `GMAIL_ENABLE_SEND=true`; never retried; the outcome is settled by reading (§4.3) |
| `drafts.update` | PUT | Used: `update_draft`. PUT-only and no ETag; guarded by the message-id witness of §4.4, a recorded deviation (§17b) |
| `getProfile` | GET | Used: `get_profile`; also the login email lookup and `doctor`'s probe |
| `history.list` | GET | Used: `list_changes` (§7.6) |
| `labels.create` | POST | Used: `create_label` |
| `labels.delete` | DELETE | Gated: `delete_label`, behind `GMAIL_ENABLE_DESTRUCTIVE`; it strips the label from every message and cannot be undone |
| `labels.get` | GET | Used: `list_labels` with `counts: true` (list omits the counts) |
| `labels.list` | GET | Used: `list_labels`, and label-name resolution (§6.3) |
| `labels.patch` | PATCH | Used: `update_label` |
| `labels.update` | PUT | Written off: PUT replaces the whole label; `patch` covers every edit (§4.4) |
| `messages.attachments.get` | GET | Used: `download_attachment` |
| `messages.batchDelete` | POST | Written off: "provides no guarantees that messages were not already deleted or even existed", so its result cannot be reported truthfully; `delete_permanently` loops `messages.delete` under a cap instead |
| `messages.batchModify` | POST | Used: `modify_labels` on 2–100 explicit ids (§4.7 caps it far below Google's 1000) |
| `messages.delete` | DELETE | Gated: `delete_permanently`, registered only with `GMAIL_ENABLE_DESTRUCTIVE=true`; the only scope it accepts is `https://mail.google.com/` |
| `messages.get` | GET | Used: `get_message`, `download_attachment` (to find the part), reply construction (to read `Message-ID`/`References`) |
| `messages.import` | POST | Written off: mailbox migration, not an assistant task; it runs spam and virus classification as though delivered |
| `messages.insert` | POST | Written off for the server: it plants mail that was never delivered, bypassing the classification a received message gets. Used by the live driver only, to write the messages it reads (§9.1) |
| `messages.list` | GET | Used: `search_messages` |
| `messages.modify` | POST | Used: `modify_labels` on one message |
| `messages.send` | POST | Written off: sending goes through `drafts.send` only, so what leaves is always something that existed as a draft the person could see (§4.2); `send_draft` covers every case a one-shot send would |
| `messages.trash` | POST | Used: `trash` |
| `messages.untrash` | POST | Used: `restore` |
| `settings.cse.identities.create` | POST | Written off with CSE |
| `settings.cse.identities.delete` | DELETE | Written off with CSE |
| `settings.cse.identities.get` | GET | Written off with CSE |
| `settings.cse.identities.list` | GET | Written off: client-side encryption is Workspace-admin key management; an encrypted body is unreadable through this API regardless |
| `settings.cse.identities.patch` | PATCH | Written off with CSE |
| `settings.cse.keypairs.create` | POST | Written off with CSE |
| `settings.cse.keypairs.disable` | POST | Written off with CSE |
| `settings.cse.keypairs.enable` | POST | Written off with CSE |
| `settings.cse.keypairs.get` | GET | Written off with CSE |
| `settings.cse.keypairs.list` | GET | Written off with CSE |
| `settings.cse.keypairs.obliterate` | POST | Written off with CSE: permanent key destruction |
| `settings.delegates.create` | POST | Written off: grants another person the mailbox; `gmail.settings.sharing` |
| `settings.delegates.delete` | DELETE | Written off: `gmail.settings.sharing` |
| `settings.delegates.get` | GET | Written off with `delegates.list` |
| `settings.delegates.list` | GET | Written off: delegation is account administration; `gmail.settings.basic` |
| `settings.filters.create` | POST | Deferred (§17): a filter's `action.forward` is auto-forwarding by another route; buildable only with forward refused structurally |
| `settings.filters.delete` | DELETE | Deferred with `filters.create` |
| `settings.filters.get` | GET | Used: `list_filters` (one filter) |
| `settings.filters.list` | GET | Used: `list_filters` |
| `settings.forwardingAddresses.create` | POST | Written off: exfiltration path (§4.1); `gmail.settings.sharing` |
| `settings.forwardingAddresses.delete` | DELETE | Written off: `gmail.settings.sharing`, which this server never requests |
| `settings.forwardingAddresses.get` | GET | Used: `get_settings` |
| `settings.forwardingAddresses.list` | GET | Used: `get_settings` (read) |
| `settings.getAutoForwarding` | GET | Used: `get_settings` (read, so a person can see whether mail is leaving) |
| `settings.getImap` | GET | Used: `get_settings` |
| `settings.getLanguage` | GET | Used: `get_settings` |
| `settings.getPop` | GET | Used: `get_settings` |
| `settings.getVacation` | GET | Used: `get_settings` |
| `settings.sendAs.create` | POST | Written off: `gmail.settings.sharing`; identity administration |
| `settings.sendAs.delete` | DELETE | Written off: `gmail.settings.sharing` |
| `settings.sendAs.get` | GET | Used: `create_draft` validates a chosen `from` |
| `settings.sendAs.list` | GET | Used: `get_settings`, and the `from` choices of `create_draft` |
| `settings.sendAs.patch` | PATCH | Written off: `gmail.settings.basic`/`sharing`; signature editing is deferred to §17 |
| `settings.sendAs.smimeInfo.delete` | DELETE | Written off: key administration |
| `settings.sendAs.smimeInfo.get` | GET | Written off with `smimeInfo.list` |
| `settings.sendAs.smimeInfo.insert` | POST | Written off: uploads a private key |
| `settings.sendAs.smimeInfo.list` | GET | Written off: S/MIME key administration |
| `settings.sendAs.smimeInfo.setDefault` | POST | Written off: key administration |
| `settings.sendAs.update` | PUT | Written off: PUT, and as `patch` |
| `settings.sendAs.verify` | POST | Written off: `gmail.settings.sharing` |
| `settings.updateAutoForwarding` | PUT | Written off: turning forwarding on sends all future mail elsewhere; the single most dangerous write this API offers to an injected instruction (§4.1) |
| `settings.updateImap` | PUT | Written off: client-protocol configuration, not an assistant task |
| `settings.updateLanguage` | PUT | Written off: display preference, no assistant task |
| `settings.updatePop` | PUT | Written off: as IMAP |
| `settings.updateVacation` | PUT | Deferred (§17): an auto-reply writes to every sender, so it is sending by another name and needs its own argument |
| `stop` | POST | Written off with `watch` |
| `threads.delete` | DELETE | Gated: `delete_permanently` on a thread, as `messages.delete` |
| `threads.get` | GET | Used: `get_thread` |
| `threads.list` | GET | Used: `search_threads` |
| `threads.modify` | POST | Used: `modify_labels` on a thread |
| `threads.trash` | POST | Used: `trash` on a thread |
| `threads.untrash` | POST | Used: `restore` on a thread |
| `watch` | POST | Written off: push goes to a Cloud Pub/Sub topic, which a stdio process cannot own or receive. `list_changes` polls `history.list` instead |

### 8b. Field coverage

Phase 0 records a verdict for every field of `Message`, `MessagePart`,
`MessagePartHeader`, `MessagePartBody`, `Thread`, `Draft`, `Label`,
`LabelColor`, `History` and its four record types, `Profile`,
`ListMessagesResponse`, `ListThreadsResponse`, `ListDraftsResponse`,
`ListHistoryResponse`, `VacationSettings`, `AutoForwarding`,
`ForwardingAddress`, `ImapSettings`, `PopSettings`, `LanguageSettings`,
`SendAs` and `Filter` with its criteria and action — modelled, or left
out with a reason — held by `api-fields`. Two are called out now:
`Message.classificationLabelValues` (Workspace classification; read and
shown, never written, since `batchModify` can change it and that is an
administrator's decision) and `Filter.action.forward` (read and flagged,
§7.7).

## 9. Confidentiality, security, safety

Nothing deployer-specific ever enters the repository — `CLAUDE.md` rule
1 lists it. A mailbox is the worst payload in the family: every message
carries third parties' addresses, words they wrote to someone else, and
a signature with their phone number and employer.

### 9.1 What may never enter the repository, and what stops it

Two structural rules, not matters of care:

1. **Fixtures are generated, never recorded.** `gmailtest` builds its
   mailbox from code: names from a fixed synthetic list, addresses at
   `example.com`, `example.org` and `.invalid`, bodies from templates,
   ids from a counter formatted as Gmail's hex. A fixture copied from a
   live response is itself the leak, whatever a scanner says about it.
2. **The live driver reads only what it wrote.** It creates a label
   named for the run, **inserts** its own messages with
   `messages.insert` (§8a: used by the driver, written off for the
   server), and every read it makes is constrained to that label. It
   never searches the mailbox unconstrained. Sending, which only the
   send spikes do, goes to addresses the maintainer names on the command
   line, and the transcript records their redacted form only. At the
   end of a run it trashes what it inserted and deletes its label,
   unless `-keep` is set.

The leak gate is an allow-list anchored on shapes the server's own
generated fields cannot take: an `@` with a dot-suffixed domain outside
the example and invalid set; `mail.google.com/mail/u/` URLs; a keyword
(`message`, `thread`, `draft`, `msg`) followed by a 16-hex id not in the
fixture range; the Google OAuth shapes. The exemption list is asserted,
so a new entry is an argued decision.

### 9.2 Logging

Method, tool, outcome, duration, units, and an id truncated to its first
six characters. Never: an address, a subject, a snippet, a body, a
filename, a label name or a query. `q` reaches a log through a request
URL, so transport errors are stripped of their query string before
logging. `TestLogsNeverCarryThePayload` drives every registered tool
with canary values in every string argument, at debug, and asserts the
log is non-empty and carries none of them.

### 9.3 What a tool result carries

Mail. The result is the one place the payload is supposed to go, so the
rule there is §4.1's rather than §9.2's: it is marked as untrusted, and
nothing the server adds is phrased as something to do.

### 9.4 What the flags do

| Setting | Registers | Requests |
|---|---|---|
| `GMAIL_READ_ONLY=true` | the Read kind | `gmail.readonly` |
| default | Read and Write | `gmail.modify` |
| `GMAIL_ENABLE_SEND=true` | adds `send_draft` | `gmail.modify` (no change: §2.10) |
| `GMAIL_ENABLE_DESTRUCTIVE=true` | adds `delete_permanently`, `delete_label` | `https://mail.google.com/` |

`READ_ONLY` with either enable flag is refused at startup, naming both.
`docs/security.md` says plainly what the table implies: **the default
token can send**. Leaving `send_draft` unregistered stops this server
sending; it does not stop anything else that holds the token. That is
why the token lives in the keyring and nowhere a tool can read.

## 10. Auth, config, process model

The family's settled login (standard §3b): loopback on `127.0.0.1:0`,
PKCE S256, no out-of-band flow, `--no-browser` printing the URL and the
exact `ssh -L` command, the client JSON by `--client-secret` or
`GMAIL_CLIENT_SECRET` over the stored profile, the refresh token in the
OS keyring with a warned 0600 file fallback.

Gmail adds three things:

- **Every scope is restricted** (§2.11). The person's own client is
  either **Internal** to a Workspace organisation, or **External** in
  Testing with themselves as a test user — whose refresh tokens expire
  after seven days. `docs/gcp-setup.md` says which to choose and why,
  `doctor` reports the client's type where the token response shows
  it, and an `invalid_grant` after about a week is explained in
  `docs/runbook.md` as this, not as a bug.
- **Consent is re-asked only when the scope set grows.** Forcing
  `prompt=consent` on every login mints a new refresh token each time,
  and an account holds only 100 per client. Spike J checks what Google
  does when it is omitted and a refresh token already exists.
- **Changing a flag that changes scopes needs a new login**, and the
  server says so at startup rather than failing on the first call:
  `doctor` and startup compare granted with required scopes.

Configuration: `GMAIL_PROFILE`, `GMAIL_CLIENT_SECRET`, `GMAIL_READ_ONLY`,
`GMAIL_ENABLE_SEND`, `GMAIL_ENABLE_DESTRUCTIVE`, `GMAIL_LOCAL_DIR`,
`GMAIL_LOG_LEVEL`, `GMAIL_LOG_FORMAT` (`text` default), `GMAIL_HTTP_TIMEOUT`
(60 s default), `GMAIL_CONFIG_DIR`. Each also a flag. `docs/configuration.md`
is checked against the exported list.

Process: one stdio session; starts before authentication so `doctor` and
`--dump-schemas` work signed out; a disconnect is exit 0.

## 11. Reliability

- **Repeatability comes from the HTTP method.** GET is retried. POST
  is not, except `modify`, `batchModify`, `trash`, `untrash` and
  `labels.patch`, which are declared repeatable at the call site with
  the reason (applying a label twice is applying it once). `drafts.create`
  is not repeatable: a retry makes two drafts. `drafts.send` is never
  retried (§4.3).
- **Turned-away statuses** — 429, and 403 with a rate-limit reason —
  are retried for any method, since Google did not act; `Retry-After`
  is a minimum; full jitter; at most four attempts; the sending-limit
  429 is not retried at all, because its wait is hours.
- **The unit budget** of §4.9 in front of every call.
- **Timeouts** on every request (§3.16), a 32 MiB cap on a response
  body, and attachments streamed.
- **Per-item outcomes** for every multi-id write (§4.7).

## 12. Distribution and setup

`go install`, six platform archives with SBOMs, a signed checksum file,
build provenance, the `.mcpb` bundle for Claude Desktop and an MCP
registry entry. All of it is in phase 0, not a late phase: a sibling
built its release path in its fifth phase and found the packer's macOS
glob "could never have matched anything", and the release gates of §5a
run in `make check` from the first commit, so the pipeline they check
might as well exist. The first tag is the canary for every one of those
paths, and `docs/release.md` says what to check after it.

`docs/gcp-setup.md` is an input, not a description: its scope block is
generated from `internal/scopes` and gated (§5a, `staleness`).

## 13. Testing

- **`internal/mime` first**, with table tests over the cases of §3.6
  and §3.7 and a build-then-parse fuzz test (§4.10).
- **`gmailtest`**, an in-memory Gmail behind the REST paths the client
  uses, generated per §9.1, which models the facts of §2 that the
  server's logic depends on: threading by the three conditions, `SENT`
  and `DRAFT` refusing manual application, a draft's message id
  changing on update (once spike A says it does), 404 for an expired
  history id, unit costs.
- **Renderer goldens** for thread, message, listing, draft and changes,
  including the hidden-text and link-mismatch cases of §4.1.
- **The logging test** of §9.2.
- **The live driver**, `scripts/livemail`, per §9.1: every tool and
  option (held by `live-cover`), the transcript through one redacting
  writer (held by `transcript`), and the transcript read by a person
  before a phase counts.
- **Evals**, `scripts/evals`, against `gmailtest` only, including tasks
  whose mail contains an injected instruction, scored on whether the
  model followed it.

## 14. Confirmed decisions and their consequences

| Decision | By | Consequence |
|---|---|---|
| Design first, reviewed before code | maintainer, 2026-09-24 | this document; phase 0 waits for "go" |
| Drafts are the default write; send registered only with `GMAIL_ENABLE_SEND` | maintainer, 2026-09-24 | §4.2; `send_draft` is the only send |
| Trash by default; permanent delete only with `GMAIL_ENABLE_DESTRUCTIVE`, which alone requests `https://mail.google.com/` | maintainer, 2026-09-24 | §4.6, §9.4 |
| Adopt the vendored-schema validation of manifests and registry entries, with its known gaps closed | maintainer, 2026-09-24 | §5a `mcpb`, `server-json` |
| Send only through drafts; `messages.send` written off | this design | §4.2, §8a |
| Unregistered, not registered-and-refusing, for gated tools | this design | §17b |
| Release pipeline in phase 0 | this design | §12 |

## 15. What must be verified live

Each spike states its question and, when run, its verdict separately.
None has run.

- **Spike A — does `drafts.update` change the draft's message id?** The
  witness of §4.4 depends on it. Create, update twice, read between.
  Verdict decides between the message id and a raw-bytes hash.
- **Spike B — does Gmail keep a client-set `Message-ID`?** Create a
  draft with one, send it to the maintainer's second address, read the
  sent copy and the received copy. §4.3's settle-by-reading depends on
  it. If Gmail rewrites it, the fallback is the draft's own message id
  in `SENT`, which spike C checks.
- **Spike C — what does `drafts.send` leave behind?** Is the sent
  message's id the draft's message id? Does `drafts.get` answer 404
  afterwards? §4.3's verdict table is written from this.
- **Spike D — threading.** A reply built per §4.5 lands in the thread;
  and each of the three conditions of §2.5 removed in turn, to see which
  ones Gmail actually enforces for the sender and for a non-Gmail
  receiver.
- **Spike E — non-ASCII round trip.** Subject, display names, body and
  an attachment filename in four scripts, built by `internal/mime`,
  read back from Gmail and from a non-Gmail receiver.
- **Spike F — `messages.insert` as a fixture source.** Do inserted
  messages appear in `threads.list`, `q` search and `history.list` like
  delivered ones? §9.1's driver depends on it.
- **Spike G — scope refusals.** Under `gmail.modify`, `messages.delete`
  is refused 403 (the reason §4.6 gates by scope as well); under
  `gmail.readonly`, every write is refused. The error shape of each,
  for §6.5's mapping.
- **Spike H — rate limiting.** What a 429 looks like: `Retry-After`
  present or not, and the reason string that tells the sending limit
  apart from the rest.
- **Spike I — label names and case.** Can `Foo` and `foo` coexist? What
  does creating a label named like a system label return?
- **Spike J — consent without `prompt=consent`.** With a refresh token
  already issued, does a login that omits it get a new refresh token,
  none, or an error? §10.
- **Spike K — expired history.** A `startHistoryId` far below the
  current one: 404, and the body's shape. §7.6.

Spikes B, C and D send real mail and need the maintainer's second
address; they are the "ask before doing" of `CLAUDE.md`.

## 16. Delivery phases

Each phase is one session and ends ready to tag, then waits for an
explicit "go". The next session starts from this repository alone.

**Phase 0 — scaffolding, gates, MIME and reading (v0.1.0).** Everything
of §5a: CI, release, publish, bundle, registry, every gate of the
`check` list, pre-commit, templates, community files, and the docs
(`README`, `CHANGELOG`, `CONTRIBUTING`, `SECURITY`, `CODE_OF_CONDUCT`,
`docs/configuration.md`, `development.md`, `release.md`, `security.md`,
`gcp-setup.md` generated, `runbook.md`, `docs/README.md`). The core:
`cmd` with `login`, `logout`, `status`, `doctor`; `config`,
`credentials`, `fileperm`, `userconfig`, `auth`, `scopes`, `redact`,
`version`, `app`, `server`, `tools`, `gapi` with the read methods and the
unit budget. **`internal/mime` parse side complete with its table
tests**, because everything else stands on it. `gmailtest`. The read
tools: `get_profile`, `search_threads`, `search_messages`, `get_thread`,
`get_message`, `list_labels`, `list_drafts`, `get_draft`. The
untrusted-content rendering of §4.1 and the budget of §4.8. §8a moved
to `testdata/api-coverage.tsv`; §8b's record. Spikes F, G and K. A live
run whose transcript is read.

**Phase 1 — attachments, changes, settings, resources (v0.2.0).**
`download_attachment`, `list_changes`, `get_settings`, `list_filters`,
the three resources. Spike I.

**Phase 2 — the write path (v0.3.0).** `internal/mime` build side and
its round-trip fuzz; `create_draft` with replies, `update_draft` with
the witness, `delete_draft`, `modify_labels`, `trash`, `restore`,
`create_label`, `update_label`; per-item outcomes. Spikes A, D, E, J.

**Phase 3 — sending and the gated tools (v0.4.0).** `send_draft` with
the recipient guard and the settle-by-reading of §4.3;
`delete_permanently`, `delete_label`; the scope escalation and re-login
path. Spikes B, C, H.

**Phase 4 — evals and 1.0 (v1.0.0).** The evals harness with the
injection tasks of §13; the surface frozen into the schema baseline;
§17 closed or each item argued open.

### 16a. Found by review, and fixed

Empty. Each phase's `/code-review high` and `/security-review` findings
are recorded here with the commit that fixed them.

### Closing a phase

1. `make check` green; the live driver run and its transcript read.
2. `/simplify`, `/code-review high` and `/security-review`; findings
   fixed or recorded in §16a.
3. The status line, §16 and `CHANGELOG.md` say what was built and what
   is owed.
4. Commit on the topic branch; say what is ready to tag; stop.

## 17. Open decisions

1. **Is `delete_draft` destructive?** `drafts.delete` is permanent, not
   trash. Proposed: registered by default with `confirm: true`, because
   it only ever destroys the caller's own unsent text, and gating it
   would put the destructive flag — and `https://mail.google.com/` —
   between the model and tidying up a draft it wrote a minute ago. The
   counter-argument is that a draft may be the person's own half-written
   work, not the model's. **Open.**
2. **Should `get_thread` fold a thread's drafts in?** A draft reply
   appears inside its thread with the `DRAFT` label. Showing it helps a
   model see what is pending; hiding it keeps "what was said" apart from
   "what might be". Proposed: shown, marked as a draft, after the sent
   messages. **Open.**
3. **go-licenses v1.6.0 or v2.** One sibling runs the v2 module; the
   rest call v1.6.0 current. Unknown whether v2 classifies MIT-0, which
   one of our transitive candidates carries. Decide in phase 0 by
   running both over the real module graph. **Open.**
4. **Vacation responder writes.** `updateVacation` is deferred: an
   auto-reply writes to every sender, which is sending by another name.
   If built, it belongs behind `GMAIL_ENABLE_SEND` with the recipient
   scope (`restrictToContacts`, `restrictToDomain`) required, never
   defaulted. **Deferred to after 1.0.**
5. **Filters.** `filters.create` could be built with `action.forward`
   refused structurally — the wire type would have no such field. The
   case for it is weak and the risk is §4.1's worst one. **Deferred to
   after 1.0.**
6. **Google's hosted server.** §1. If it gains trash, local attachment
   transfer and bounded reads, this server's reason to exist narrows to
   the local-token and verified-release half. Revisit at each minor
   release; record the check in §18. **Open, standing.**
7. **Lenient argument decoding** (§3.14). Accepting `"[\"a\"]"` where
   an array is declared helps clients that stringify, and makes the
   schema a less exact contract. Proposed: accept JSON-in-a-string for
   arrays and integers only, and log that it happened. **Open.**

### 17a. Deferred cleanups

None yet.

### 17b. Deviations from the shared standard

The standard at `~/.claude/mcp-server-standard.md`, read 2026-09-24.

| The standard says | Here | Why |
|---|---|---|
| Errors use six classes | Twelve (§6.5) | `stale` and `ambiguous_outcome` are forced by §2.3 and §2.4; `rate_limited` must separate the sending limit from request limits; `forbidden` and `auth` ask for different fixes; `ambiguous` and `blocked` as the siblings use them |
| Never overwrite; compute a minimal diff | Held for labels (patch); **not holdable** for drafts | `drafts.update` is PUT-only and gmail/v1 has no ETag (§2.4). §4.4's witness narrows the lost-update window without closing it, and omitted fields are carried over rather than dropped |
| Destructive tools are not registered unless enabled | Held, and extended to sending | One sibling now registers deletes and refuses per call, and another retired its flag in favour of per-call guards. Neither fits here: the permanent-delete methods accept only a scope this server requests only when the flag is set, so a registered delete tool would fail every call by default; and a registered send tool is exactly the control an injected instruction gets to argue with |
| Read-only mode requests read-only scopes | Held | Recorded because one sibling requests write scopes in read-only mode to avoid a second consent; here the difference is `gmail.readonly` versus a scope that can send |

Everything else is adopted as written, including the preamble's three
obligations: make a rule a test, derive its list from the code, and
have the checker assert a floor on how much it read.

## 18. Evidence log: conventions checked, changed, or rejected

Sources: the Gmail v1 discovery document (revision 20260917, fetched
2026-09-24); Google's Gmail guides for quota, errors, batch, threads,
uploads, sync, filtering and labels; the Gmail scopes page; Google's
OAuth 2.0 page and Cloud help on Testing status; the Workspace MCP
configuration guide; Gmail and Workspace help on sending limits;
RFCs 5322, 2045–2047, 2231, 4648 and 8252; the MCP specification
2025-11-25; the servers of §1 and their issue trackers. Checked
2026-09-24.

**Three tiers.** (1) Verified here against a primary source. (2) Taken
from a sibling server's own evidence log, which was verified there. (3)
Asserted from documentation or a secondary source and **not yet probed
live** — §15 exists to settle these, and they are marked.

| # | Convention or assumption | How checked | Verdict |
|---|---|---|---|
| 1 | A narrower scope can allow drafts but not sending | Discovery document, per-method scopes | **Refuted.** `gmail.compose` and `gmail.modify` both grant `messages.send` and `drafts.send`. Registration is the only control; §4.2 |
| 2 | `gmail.modify` covers every write the server needs | Discovery document | **Confirmed, except permanent deletion**, which accepts only `https://mail.google.com/`. §4.6 gates the tool and the scope together |
| 3 | Gmail has ETags for optimistic concurrency, as the Calendar API does | Discovery document, all schemas and parameters | **Refuted.** No `etag`, no `If-Match`. §4.4 and §17b |
| 4 | Drafts can be patched | Discovery document | **Refuted.** `drafts.update` is PUT and "replaces a draft's content". §4.4 |
| 5 | A retried send is harmless | Discovery document, `messages.send` parameters; a public double-send issue | **Refuted.** No request id, no documented dedupe; a lost 200 became a second delivery. §4.3 |
| 6 | Quota is a request rate | Gmail quota page | **Refuted.** Units per user per minute, 6,000, with per-method costs from 1 to 100; changed for projects created from 2026-05-01. §4.9 |
| 7 | `threadId` alone threads a reply | Gmail threads guide; `Message.threadId` in discovery | **Refuted.** Three conditions: `threadId`, RFC 5322 `References`/`In-Reply-To`, matching `Subject`. §4.5; spike D probes which Gmail enforces |
| 8 | The API's message id is what `In-Reply-To` carries | RFC 5322 §3.6.4; a public fix to a server that did this | **Refuted.** It carries the RFC 5322 `Message-ID` header value. §4.5 |
| 9 | A 404 from `history.list` means the mailbox or id does not exist | Gmail sync guide | **Refuted.** It means the cursor expired; the client must fully resync. §7.6; spike K (tier 3 for the body shape) |
| 10 | Dates in `q` are read in the account's zone | Gmail filtering guide | **Refuted.** Midnight Pacific. §6.3 adds `after`/`before` as epoch seconds |
| 11 | `q` works under every read scope | Discovery document, `messages.list.q` | **Refuted for `gmail.metadata`.** Read-only mode uses `gmail.readonly` |
| 12 | `gmail.readonly` is a sensitive, easily granted scope | Gmail scopes page | **Refuted.** Restricted, like every Gmail scope that reads content. §2.11, §10 |
| 13 | A personal client's refresh token lasts until revoked | Google OAuth 2.0 page | **Refuted for External/Testing:** seven days. §10, runbook |
| 14 | `prompt=consent` on every login is harmless | Google OAuth 2.0 page (100 tokens per client per account); secondary reports of token churn | **Tier 3.** Each forced consent mints a token and the oldest is invalidated past 100; §10 asks consent only when scopes grow; spike J |
| 15 | Upload size is limited only by the attachment limit | Discovery document `mediaUpload.maxSize`; uploads guide | **Refined.** 35 MB for send and drafts, 150 MB for insert and import; simple upload recommended up to 5 MB. §7.4 |
| 16 | `batchModify` is a cheap way to act on search results | Discovery document (1,000 ids); errors guide | **Rejected as a surface.** Capped at 100 explicit ids, never a query. §4.7 |
| 17 | `batchDelete` reports what it deleted | Discovery document | **Refuted.** "Provides no guarantees that messages were not already deleted or even existed." Written off; §8a |
| 18 | Push notifications could drive `list_changes` | Discovery document, `watch` | **Rejected.** Delivers to Cloud Pub/Sub; a stdio server owns no topic |
| 19 | System label ids are a closed, documented set | Gmail labels guide | **Refuted.** "Not exhaustive." Label resolution never hard-codes the list beyond aliases for the common ones; spike I |
| 20 | `SENT` and `DRAFT` can be applied like other labels | Gmail labels guide | **Refuted.** Refused; `conflict` in §6.5 |
| 21 | Mail content can be shown to the model as plain text | Workspace MCP guide ("hidden instructions that can hijack your session"); a public issue reporting an injected forward | **Rejected.** §4.1 |
| 22 | The vendor's own Gmail MCP server offers send | Workspace MCP configuration guide | **Refuted.** Requests `gmail.readonly` and `gmail.compose`, ships drafts but no send, trash or delete. Supports §4.2's position. Whether it is labelled Developer Preview is **tier 3** |
| 23 | Sending limits are request quota | Gmail and Workspace help | **Refuted.** Separate daily and per-message recipient limits, surfaced as 429, possibly minutes late. §6.5 `rate_limited` separates them; spike H |
| 24 | Attachments may be read from any path the caller names | A surveyed server's README | **Rejected.** Only base names inside `GMAIL_LOCAL_DIR`. The exfiltration scenario is this design's inference, not a reported incident |
| 25 | Non-ASCII headers can be sent raw | RFC 5322 §2.2, RFC 2047; public 400s on accented display names | **Refuted.** Encoded-words, ≤75 characters each. §4.10; spike E |
| 26 | The credential order is keyring → file → env, as four sibling documents say | The sibling code | **Refuted (tier 2).** Every sibling's code resolves env → keyring → file; the documents describe which fallbacks exist, not precedence. §5a states the code's order |
| 27 | The newest sibling carries the newest shared machinery | Per-component comparison across all six siblings, 2026-09-24 | **Refuted.** Each sibling was newest for some pieces and stale for others. §5a merges them |
| 28 | A sibling's newest pin is upstream's newest | Upstream release listings, 2026-09-24 | **Refuted** for goreleaser, syft and codeql-action. §5a takes pins from upstream |
| 29 | A sibling's generated setup page is gated, as its source comments say | The sibling's gates and tests | **Refuted (tier 2).** Nothing read the page. §5a's `staleness` generates and compares it |
| 30 | The MCP Go SDK version can be a constant in the schema dump | Two siblings' constants against their `go.mod` | **Refuted.** Both constants were a version behind. §5a derives it from build info |
