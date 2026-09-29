# Architecture — google-mail-mcp

**Status: 2.0.1 is released, 2026-09-29, from `main`, and verified from
outside: checksums, the cosign signature and the provenance attestation,
each also against a tampered copy, the registry entry, and the Go proxy
resolving the `/v2` module path. A question quotes mail text in code
spans, so a client that draws Markdown shows it literally; that
rendering in VS Code was read from its source, not seen. Since 1.2.0
the server asks the person
through the MCP client before each write that takes `confirm` (§4.13).** Still
unproven: the bundle installed in Claude Desktop.

## 1. Mission and scope

A production-grade Go MCP server for Gmail, distributed to other people.
One binary, stdio, per-user OAuth against the person's own Desktop OAuth
client, no hosted deployment.

The server works **inside one mailbox**: finding mail, reading threads
and messages, attachments, labels, drafts, and — only when the person
opts in — sending and the signature, filters and vacation reply (§7.9).
It stops at the edge of the message. A calendar
invitation is an `.ics` attachment here; the event belongs to a server
built on the Calendar API. A linked Drive file is a URL here; the file
belongs to a Drive server. Mailbox administration — delegation,
forwarding, send-as identities beyond a signature, S/MIME and client-side-encryption keys —
is out of scope, and §8a says why method by method.

### Why build it (research summary, checked 2026-09-24)

Four servers were surveyed; their issue trackers are where §3 comes
from.

- **Google's hosted Gmail MCP server** (`gmailmcp.googleapis.com`,
  configured through the Workspace MCP guide). Remote HTTP; needs the
  person's own Cloud project with the Gmail MCP API enabled. Requests
  `gmail.readonly` and `gmail.compose`, and ships **no send, no trash
  and no delete tool** — although `gmail.compose` can send (§2.10). Its
  surface is search, read, drafts and labeling. It is the strongest
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
    labeled. A thread-level change does not reach messages added to the
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
    stringify them — arrays and numbers sent as JSON strings. Declined
    for 1.0: the defects still open do not reach this surface (§17.7).
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
   or same-color-as-background runs, zero-width and bidi-control
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
   The drafting, labeling and trash writes are registered by default;
   §9.4 lists the rest. There is no one-shot
   `send_message`: `messages.send` is written off, so everything that
   leaves this server existed first as a draft the person could open in
   Gmail. Replying is `create_draft` with `reply_to`, then `send_draft`.
2. **The body is exactly what the caller gave.** No signature, prefix,
   footer or "sent by" line is added server-side, ever.
3. **`send_draft` shows before it sends.** Its result names every
   recipient (To, Cc, Bcc), the subject, the attachment names and sizes,
   and the thread it joins. `dry_run: true` returns that without
   sending, and costs one `drafts.get` and one `threads.get`: every draft
   sits in a thread, if only its own.
   Like `update_draft`, it takes the draft's `message_id` as a witness
   and refuses with `[stale]` a draft that changed since it was read, so
   what is sent is what was shown.
4. **Recipient guard.** Every recipient who is not already a participant
   in the thread being answered — for a new message, every recipient —
   must be listed in `confirm_recipients`, exactly; otherwise the call
   is `[blocked]` and names them. Over 50 recipients is `[blocked]`
   outright. The model then has to write out an injected address itself
   rather than inherit it silently from a draft someone else shaped.
   A participant is the sender of a message of the thread, or an address
   the account itself sent one of them to (its own `To` and `Cc`). The
   `To`, `Cc` and `Reply-To` of a received message do not count: its
   sender wrote them, so a correspondent cannot widen a reply-all to
   addresses nobody confirmed (§17.8). Drafts, spam and trash are not
   read, so a message planted in the thread and binned does not vouch
   for its sender. The refusal names recipients by field
   and position, `cc[1]`, never by address, since an address may come
   from someone else's message; the dry run lists them inside a block.
   When the client can ask, the person then confirms the send, seeing
   every address it reaches (§4.13).

### 4.3 A send is never retried; ambiguity is settled by reading

`drafts.send` is a non-idempotent POST (§2.3). The client retries a POST
only where Google turned it away before acting — a 429 or 503 carrying
no body the server acted on, per §11 — and never on a timeout, a reset
connection or a 5xx after the request was written.

On an ambiguous failure the tool returns `[ambiguous_outcome]` and has
already done the read that settles it:

- The design meant to settle by the `Message-ID` the draft carried.
  Spikes B and C ruled that out: Gmail replaces the `Message-ID` on
  `drafts.create`, on `messages.send` and again on `drafts.send`, and the
  sent message gets a new id, not the draft's (§18 rows 41 and 44). What
  holds is the thread: the sent message is filed in the draft's own
  thread.
- So before the send the server records the ids of every message in the
  draft's thread, which it reads anyway for the recipient guard. After
  an ambiguous `drafts.send` it waits 5 seconds, then reads the draft
  (`drafts.get`) and the thread again. The draft gone and a message in
  `SENT` that was not in the thread before means **sent**. The draft
  present and no such message means **not sent**. Anything else stays
  **unknown**, and the result says so and says not to resend.
- This state is reported, never acted on: the server does not resend on
  "not sent" either. The person decides.

### 4.4 A write never destroys what it cannot see

**Labels are patched.** `labels.update` (PUT) is written off.

**A draft update is a read-merge-write against a witness.** Gmail has no
ETag (§2.4), and `drafts.update` replaces the whole draft. So:

1. `get_draft` returns, beside the content, the id of the message
   currently inside the draft. `drafts.update` changes that id (spike
   A), so it is the witness.
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

- `reply_to` takes a **message id**; `reply_to_thread` takes a thread id,
  and the server picks the newest message that is not a draft, not in
  `TRASH` and not a reaction, and names the chosen message in the result.
  They are two fields because the two ids cannot be told apart: a
  thread's id is its first message's id (§18 row 40), so one field taking
  both would answer the first message when the caller meant the thread.
  A `reply_to` naming a draft, a trashed message or a reaction is
  `[invalid]`.
- The server reads the parent's `Message-ID`, `References`, `Subject`,
  `From`, `Reply-To`, `To` and `Cc`, and writes `In-Reply-To` = the
  parent's `Message-ID`, `References` = the parent's `References` plus
  that id, `threadId` = the parent's thread, and `Subject` = the
  parent's, with a `Re:` prefix only if there is none. A parent with no
  `References` but one `In-Reply-To` contributes that (RFC 5322 §3.6.4);
  References keep the parent's first id, its last 38 and the parent's
  own, so a hostile parent cannot make a reply huge. A caller's `subject` on a reply is
  `[invalid]`, since it is one of the three conditions.
- `reply_all: true` expands recipients from the parent, drops the
  account's own addresses (from `sendAs.list`) and says who was added.
  When the parent is the account's own, the reply goes to its To, as in
  Gmail. Each recipient in the result carries its origin — the parent,
  reply_all, or the caller, whose addresses are added last. A parent's
  address that cannot be written into a header is left out and counted,
  never quoted in the server's voice.
- The result states whether the draft landed in the parent's thread, by
  reading the created draft's `threadId` rather than asserting it.

### 4.6 Removal is trash

`trash` and `restore` are ordinary writes, registered by default, because
trash is recoverable for 30 days and gating it would teach people to set
the flag that also arms permanent deletion. A gate that everyone turns
on protects no one.

`delete_permanently` and `delete_label` are **unregistered** unless
`GMAIL_ENABLE_DESTRUCTIVE=true`, and each call still needs
`confirm: true`, and the person's own confirmation when the client can
ask (§4.13). That flag is also **the only setting that requests
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

A filter (§7.9) does match a query, and is not an exception: Gmail
applies it only to mail that arrives after it exists, and the server
never applies one to mail already in the mailbox. What it can hide is
mail nobody has seen yet, which is why one that trashes needs
`confirm: true` and the person's confirmation (§4.13), and why filters
sit behind their own flag.

Per-item outcomes are reported per item. A call that trashed 97 of 100
says which three failed and why; it never reports the batch as one
success. So each item is its own pair of calls: a read of its labels,
then `messages.modify`, `trash` or `untrash` on the message (or the
thread's), whose answer is the labels after. An item already as asked is
not written and is reported unchanged. `batchModify` is written off: it
answers with nothing, so it could say neither which messages changed nor
why one failed (§18 row 39).

### 4.8 Reads are bounded and say what they left out

Every read has a budget in characters, stated in the result. A thread
renders newest-first up to the budget; quoted text and signature blocks
in each message are collapsed to a line saying how much was collapsed;
messages beyond the budget are listed by id, sender and date with a
cursor. A long body is cut at a paragraph boundary with a marker and the
offset to continue from. Listings use `format=metadata` with a fixed
header set, never `full`.

A listing's budget bounds its text, in `content` and again in
`untrusted_text`; the structured rows carry every item on the page in
full, as 1.1.0 did, and are not counted against it. A page that fits
is shown whole. Otherwise the text shows rows while they and the line
naming the rest fit, and that line counts every row left out and names
their ids, each once and none a shown row carries; `omitted_ids` holds
the same ids. A change listing can name one message twice, so the line
counts the changes on messages named already apart. `list_filters`
shows forwarding filters first, so one that forwards is never out of
the text.

The structured half is not bounded: a page of 100 results came to
70,000 to 106,000 characters under a budget of 24,000 (§18 row 57). The
search and draft descriptions say so, and that a client that limits
the size of one result should ask for a smaller `max`, such as 25.

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

### 4.13 A write that cannot be undone is confirmed by the person

`confirm: true` and `confirm_recipients` are arguments the model writes,
and a persuaded model writes them too. So when the client can ask, the
server asks the person itself, through MCP form elicitation, before
seven writes: `delete_draft`, `delete_label`, `delete_permanently`,
`delete_filter`, `create_filter` when the filter trashes, `set_vacation`
when it turns the reply on, and `send_draft`.

1. **A second gate, not a replacement.** The arguments stay and are
   checked first (§14). A call a guard refuses asks nothing. The
   question comes after every read and every other guard, just before
   the write, so it shows what the write would do.
2. **Accepting is the confirmation.** The form has no fields: an empty
   object schema, which the specification allows (§18 row 64). The
   first build asked for a checkbox as well, and in the maintainer's
   check a person pressed Accept without ticking it, meaning to
   confirm, and was refused; two confirmations that read as one were
   dropped for one. Anything but `accept` — decline, cancel, an error,
   an answer that came back after its question expired — is `[blocked]`,
   and nothing is written. The refusal says the call was "not confirmed
   by the person" and names the client's answer. It never says the
   person declined: a client can answer without showing anyone anything
   (§18 row 60), and a client hook can accept for the person (§18
   row 61). So an unattended client that declares elicitation cannot
   make these writes, which is accepted (§14). Two clients accept an
   empty form without a person choosing to: Codex under approval policy
   `never` with full access, and VS Code when the person skips the
   question. A required choice naming the outcome would stop both, and
   was declined after the maintainer's check found it slower and less
   clear than Accept (§18 row 67).
3. **No question possible.** A client that declares no form elicitation
   gets no question, and the arguments are the guard, as before.
   `GMAIL_REQUIRE_PROMPT=true` refuses those writes as `[blocked]`
   instead. Claude Desktop is such a client today (§18 row 62).
4. **A dry run never asks**, and never needs an answer.
5. **What the question says.** The tool, the target and the consequence,
   in the server's words: a label by name with its message and thread
   counts; a permanent delete by how many messages and threads; a draft
   by its subject and recipients; a filter by its criteria and actions;
   the vacation reply by its audience, dates, subject and the start of
   its text; a send by every address it reaches, its subject and the
   start of its body. Text from the mailbox or from the call stands in
   a code span, between backticks, on one line: control and invisible
   characters removed; backticks, grave and acute marks, and double,
   typographic and fullwidth quote marks made a plain single quote; a URL scheme, `mailto:`,
   `www.` and a bare domain followed by a path broken so no client draws
   a link; cut at 120 characters, an address at 254, a body at 300 with
   the count of the rest; one with nothing left to show is said in
   words, `empty` or "invisible characters only". A client that draws the question as Markdown
   shows a code span literally, so mail text draws no emphasis, link or
   HTML there, and a blank line between lines keeps them apart (§18 row
   66). A closing line says text in backticks or code style is not the
   server's. Nothing is
   phrased as an instruction from the mail (§4.1).
6. **One handler on every protocol.** The handler returns the question
   as an input request, the multi-round-trip pattern of 2026-07-28.
   Before that revision the SDK asks with `elicitation/create` and calls
   the handler again within the same request (§18 row 59). A client
   failure there is a JSON-RPC error inside the SDK, and middleware
   turns it into `[blocked]`.
7. **The answer is bound to its question.** `requestState` is signed
   with HMAC-SHA256 under a key drawn per process. It carries the tool,
   a hash of the arguments — the ids and, for a send, the draft's
   `message_id` witness — a hash of what the question binds, a nonce and
   an expiry. A retry is refused when its state is forged, for another
   call, expired or already used, and answers on a call with no state
   are refused, so no client answers before it is asked. The retry reads
   again; if what it binds differs from what was answered, it is
   refused, and the next call asks again. A question binds its text,
   with three exceptions: a label's counts are shown and not bound,
   since a label that receives mail while the person reads would never
   be confirmed, and its id and name are bound instead; a draft to
   delete binds the message it holds, the witness `update_draft`
   changes (§4.4); a send and a vacation reply bind a hash of the whole
   body, of which they show the start.
8. **The expiry applies where the state travels.** From 2026-07-28 the
   client carries `requestState` between the rounds, and it expires 5
   minutes out. Before, it never leaves the process: the request itself
   waits for the person, and a slow accept counts.
9. **A send goes at most once (§4.3).** The first round stops before
   `drafts.send`. Only the verified retry sends, and its state is spent
   before the handler runs, so a replay is refused.
10. **A failure after the answer is never "nothing written".** Once an
    answer has confirmed the write, a call that then fails without a
    result — its reply could not be built or sent, or it was canceled —
    is `[ambiguous_outcome]`: verdict `written` when the handler
    returned from its write, `unknown` otherwise, and never to be
    repeated. A failure while the question is still out is `[blocked]`.
11. **Asking costs the reads again.** The retry repeats every read
    before the write, so a confirmed call spends them twice; each
    description says how many more units, and a test holds the number
    to what the fake charges.
12. **Capability per request**: from the request's `_meta` on
    2026-07-28, from `initialize` before.
13. **Logs** say `person_asked` and `person_answered` with the tool and
    the client's action, never the question (§9.2).
14. **Held in one place.** `register` gives every kind that can take
    `confirm` a way to ask, and refuses at start a tool whose input has
    `confirm` or `confirm_recipients` under any other kind, or without
    the cost of asking. The service asks at its write, and a write
    reached with no way to ask is refused. A test finds every such tool
    from the published schemas and holds each: declined, nothing
    written; accepted, one write.

## 5. Module layout

As `CLAUDE.md` "Where things go"; the staleness gate holds that list
against `go list -tags live,evals ./...`, so this section does not repeat it.

### 5a. Shared machinery: what the current version must carry

The sibling servers improved shared machinery wherever a problem
happened to surface, so on 2026-09-24 no single sibling held the current
version of any large piece. Each row below lists every fix the
up-to-date component must carry, merged from all six. Phase 0 built to
this list, and a gate or test is named for each claim where one exists.
When a sibling fixes something shared, it is added here with the date.

**Pins, checked against upstream on 2026-09-24, not copied from a
sibling.** Three siblings' newest pins already trailed upstream:

| Tool | Version |
|---|---|
| Go | 1.27.1 (`go-version-file: go.mod` in CI) |
| MCP Go SDK | v1.8.0 |
| golangci-lint | v2.14.0 |
| goreleaser | v2.18.2 |
| cosign | v3.1.3 (`cosign-release:` on the installer) |
| syft | v1.52.0 (`syft-version:` on the installer) |
| mcp-publisher | v1.8.1, its own sigstore bundle verified before extraction |
| gitleaks | v8.30.1 |
| govulncheck | v1.8.0 |
| go-licenses | §17.3 |
| actionlint | v1.7.12, run through `go run`, not Docker |
| codeql-action | v4.38.2 |
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
| `api-fields` | recursive over inline objects; every struct the wire package declares judged; floors on published and modeled fields |
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
| `login`/`logout`/`status`/`doctor` | `logout` revoking the token at Google before deleting the local copy, and leaving an env-provided token alone; `--no-browser` printing the URL and the exact `ssh -L` line; `status --json`; the account and client-secret path masked in every output; `doctor` walking client JSON → token → granted scopes → API enabled → one `getProfile`, naming what is missing; the keyring replaced package-wide in tests by `TestMain`, with a decoy test proving the replacement happened |
| `auth` | loopback on `127.0.0.1:0`; PKCE S256; `state` checked; a bounded HTTP client on refresh; tokeninfo errors stripped of their URL, which carries the token; `invalid_grant` mapped to "log in again"; granted scopes stored |
| `scopes` | one source of truth per mode; an implication table (`https://mail.google.com/` ⊃ `gmail.modify` ⊃ `gmail.readonly`, `gmail.compose`, `gmail.labels`) with `Satisfied` and `Missing`; the generator for `docs/gcp-setup.md` |
| `credentials` | resolution **env → keyring → file**, documented as that order (four sibling documents describe the fallbacks and got the precedence wrong); a silent keyring told apart from a missing login; a warning on every use of the plaintext file |
| `fileperm` | 0600 on Unix; an ACL on Windows restricting the file to the current user, because 0600 there protects nothing |
| `userconfig` | profiles; the config-dir override refused outside the home directory, compared by real path |
| `config` | `Define(fs, env)` and `Build()` with errors joined; `GMAIL_` prefix; `READ_ONLY`, `ENABLE_SEND`, `ENABLE_DESTRUCTIVE`, `ENABLE_SETTINGS`, `LOCAL_DIR` (refused if it does not exist, naming the variable); `CONFIG_DIR` and `REFRESH_TOKEN` env-only; base-URL overrides for tests; an exported list of every variable, which the staleness gate reads |
| `redact` | account, address, path and client-id masking for product output; id truncation and the redacting printer for maintainer tooling |
| `server` | per-call log line with method, tool, outcome, milliseconds and units; the SDK's own logger only at debug; instructions built from the configuration, so read-only and send-enabled servers say different things; the schema dump taking the SDK version from build info rather than a constant (two siblings' constants are already wrong) |
| `app` | startup assembly reachable without `main`, used by the schema dump; settings that redact the token when logged |
| `tools` | one `register` deciding annotations, gating, `_meta`, the dry-run context and the rendering from one `Kind`; an explicit output schema with `date-time` for times; `Content` set so the SDK does not duplicate the JSON |
| `gapi` | write and repeatability derived from the HTTP method, POST failing closed, declared exceptions only; a POST retried only on a turned-away status; `Retry-After` honored as a minimum; full-jitter backoff; the unit budget of §4.9; a body cap; an origin allow-list, port included, checked before any credential is attached and on every redirect; transport errors stripped of the URL; **a context under which the client refuses every write**, which is what `dry_run` is; a closed `Class` type with `Retryable()`; addresses masked in API error text; a per-call counter of requests and units |
| `ask` (2026-09-29) | a form with no fields, accepting it the confirmation; `requestState` signed, single-use and bound to the tool, the arguments and the question; every quoted value a code span with backticks and quote marks folded, links broken, cut to one line, and a blank line between lines, because VS Code draws the message as Markdown (§4.13, §18 rows 66, 67) |
| logging test | every registered tool driven with canary values at debug; asserts the logs are non-empty and contain no canary |

## 6. Addressing

### 6.1 Messages, threads and drafts

By the ids Gmail gives them, which are stable and opaque, and which the
standard's §2 allows ("a stable id the platform gives you"). The model
never sees an index or a position. A result that lists messages gives
each id beside it. A draft has two ids — the draft's and the message
inside it — and every draft result names both, labeled, because
confusing them is how a reply lands on the wrong message.

RFC 5322 `Message-ID` values are shown as `untrusted_rfc822_message_id` and
accepted wherever a message id is, prefixed with `rfc822:`; the server
resolves them through `rfc822msgid:` search and refuses `[ambiguous]` if
two messages carry one (mailing lists do that).

### 6.2 Labels

By id or by exact name. System labels by their id in any case (`inbox`
is `INBOX`). A user label's name resolves through `labels.list`, which
is cached per call, not per process, because a label renamed in the web
UI between two calls must not resolve to the old one. An exact name
wins; otherwise a name matches case-insensitively. Spike I found that
Gmail refuses a second user label differing only in case (409), and
refuses `INBOX` and `Inbox` as user label names (400), so a
case-insensitive match finds at most one label Gmail created. The
`[ambiguous]` answer with both ids stays for a label list that says
otherwise, since nothing documents the rule.

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
| `forbidden` | signed in, but the organization or account refuses this | ask an administrator |
| `conflict` | the state refuses it: label name taken, `SENT` applied by hand | read and reconsider |
| `stale` | the draft moved since the witness was read (§4.4) | re-read and retry |
| `ambiguous` | a label name or `rfc822:` id matched more than one | pass an id |
| `blocked` | a guard refused what the API would have allowed (§4.2, §4.7), or the person did not confirm the write (§4.13) | pass the override, or don't; one the person did not confirm is not made again unless they ask |
| `rate_limited` | §2.2's per-user, project or sending limits | wait; the message says how long |
| `unavailable` | a transient upstream failure | retry |
| `unsupported` | the API cannot do this (§2, "cannot do") | see §2 |
| `ambiguous_outcome` | a send, a filter create or a filter delete may or may not have happened (§4.3, §7.9), or a write the person confirmed lost its result (§4.13) | read the result's verdict; never resend blind |

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
present; `all_headers: true` shows the rest. Budget and collapsing per §4.8.
Calendar invitations are listed as attachments with their method
(`REQUEST`, `CANCEL`) and no further parsing. A thread's drafts follow its conversation in a
section of their own, "drafts in this thread, not sent", on the first
read; the cursor counts only what was sent or received, and drafts over
the budget are listed by message id (§17.2).

### 7.3 Attachments

`download_attachment` writes one attachment into `GMAIL_LOCAL_DIR` and
is registered only when that is set. It takes a message id and the
attachment's `part_id`, which `get_message` lists: part ids are Gmail's
structure and stable, where an `attachmentId` is not documented to be.
The filename is taken from the part's RFC 2231 or RFC 2047 name, reduced
to a safe base name, and never overwrites: the file is created with
`O_EXCL` through an `os.Root` on the directory, a clash gets `-1`, `-2`
before the extension, and the result names the path written with the
file's size and SHA-256. The file is `0600`, or restricted by ACL on
Windows; a failed or partial write is removed. Content is streamed to
disk: `attachments.get` answers JSON, so the client reads it token by
token up to `data` and decodes that string as it arrives, never holding
it whole, and stops at 64 MB; an attachment declared larger is refused
before it is read. A stream is bounded by `GMAIL_HTTP_TIMEOUT` without
progress rather than in total, so a large file on a slow link finishes
and a stalled one does not hang. A body cut anywhere is a failed read and
retried; a retried read starts the file over.
Attachments are never inlined into a tool result.

### 7.4 Drafts and replies

`create_draft` takes `to`, `cc`, `bcc` (one address per entry, ASCII
local@domain; a refusal names the entry by position, not by what it
says), `subject`, `body` (plain text; `body_html` optional and sent as
`multipart/alternative` with the plain part kept, and refused without
`body`), `attachments` (base names inside `GMAIL_LOCAL_DIR` only; a
path, a `..` or a symlink out of it is `[invalid]`, and with the
directory unset `[blocked]`), `from` (one of `sendAs.list`, which is read
on every call for the default sender and the account's own addresses)
and `reply_to`/`reply_to_thread`/`reply_all` (§4.5). The server writes a
`Message-ID` at the sender's domain, and Gmail replaces it (§4.3), so
`create_draft` reports none; `get_draft` shows the one Gmail kept. Up to
5 MB it uses the JSON `raw` path; above it, multipart media upload;
above 35 MB it refuses before sending (§2.6).

`update_draft` per §4.4, as a tree edit of the draft's own bytes:
headers the call names are replaced, parts it names are rebuilt or
removed, and every other header and part is written back byte for byte.
`to`, `cc` and `bcc` replace their header, and `[]` clears it.
Attachments are removed by the `part_id` `get_draft` lists. A draft that
has both a plain and an HTML body takes `body` and `body_html` together,
so the two cannot disagree. A subject changed on a reply is saved and
flagged, since it may take the draft out of its thread. `delete_draft`
with `confirm: true` (§17.1) and the person's confirmation (§4.13); a draft deleted between the read and the
delete is reported gone. `list_drafts`, `get_draft`.

### 7.5 Organizing

`modify_labels` adds and removes labels on up to 100 message or thread
ids. Archiving is removing `INBOX`, marking read is removing `UNREAD`,
starring is adding `STARRED`; the description says so rather than
growing a tool per verb, and the result names the verb it amounted to.
`SENT` and `DRAFT` are `[conflict]` before anything is sent; `TRASH` is
`[invalid]`, pointing to `trash` and `restore`. A draft's message is
`[unsupported]` for that item, since Gmail does not label drafts (§2.12).
`trash`, `restore` (§4.6); trashing a draft is refused for that item and
points to `delete_draft`. `create_label`, `update_label` (patch: name,
color, visibility), with a name checked against the label list first,
so a dry run answers as Gmail would (§6.2). Visibility is written in the
tools' own words — `show`, `show_if_unread`, `hide` — in and out.
`delete_label` gated.

### 7.6 Changes

`list_changes` takes a `history_id` (from `get_profile` or a previous
call) and returns added, deleted and relabeled messages since, with the
next `history_id`. A 404 is not `not_found`: it is reported as
**"cursor expired — history before this point is gone"** with the
current `history_id` to restart from and a note that changes in between
cannot be recovered (§2.8). It never returns an empty list for an
expired cursor. The 404's body is Google's ordinary envelope, reason
`notFound`, "Requested entity was not found." (spike K), so the tool
tells expiry apart by which call met it, not by the body. With a
`label`, a 404 can also mean the label was deleted after it was
resolved, so the label list is read again first: a missing label is
`[not_found]`, never an expiry that would move the caller's cursor past
changes it can still read. `label` limits
the records to one label and `kinds` to some record types; while a
`next_page_token` is set, the pages continue from the same `history_id`,
and the returned one is used only once the listing is complete.

### 7.7 Settings, reading

`get_settings` returns vacation responder, auto-forwarding state and
address, forwarding addresses, IMAP, POP, language and send-as
identities, forwarding first. `list_filters` returns filters, each
forward action flagged, labels by name. Showing that mail is being
forwarded is the useful half of those APIs; changing forwarding is
written off (§4.1). Filters, the signature and the vacation reply can be
changed behind a flag (§7.9). Addresses, display names and filter criteria are the account's
own configuration and stand in the server's voice, like label names; the
vacation reply and the signatures are free text that goes out as mail,
so they sit inside blocks and in `untrusted_*` fields.

### 7.8 Sending

`send_draft` per §4.2, §4.3 and §4.13. Registered only with
`GMAIL_ENABLE_SEND=true`. It takes `draft_id`, the `message_id` witness,
`confirm_recipients` and `dry_run`. It reads the draft (`format=full`, for
the attachments), checks what needs no thread — a recipient at all, at
most 50, no confirmation of an address the draft does not send to — and
then reads its thread's `From`, `To` and `Cc` (§4.2). A dry run
reports the recipients the guard would stop instead of refusing, since
that is how the caller learns whom to confirm. A draft with no recipient
is `[invalid]`. The send is one `drafts.send`; the result names the sent
message, its thread and labels, read from Gmail's answer. On
`[ambiguous_outcome]` the verdict — `sent`, `not_sent` or `unknown` — is
in the message, from a `drafts.get` and a reread of the draft's thread
for a new message in `SENT` (§4.3), made 5 seconds after the failure so a
send still running at Google is not read as not sent. Those reads run
even when the call's own context has ended, bounded at 30 seconds. A send
that fails as `[unavailable]` — an answer that could not be read, or a
503 — is settled the same way, since it too may have gone out. A draft
whose `To`, `Cc` or `Bcc` is repeated, or could be read only leniently,
is `[blocked]`: the server reads the first of a repeated header and drops
what a lenient read cannot parse, while Gmail sends to every address it
finds, so the guard would clear a shorter list than the send reaches.

### 7.9 Settings writes

Registered only with `GMAIL_ENABLE_SETTINGS=true`. The writes below
that take `confirm: true` also ask the person (§4.13). The flag also
requests `gmail.settings.basic`: the discovery document lists no other scope for
these methods, `https://mail.google.com/` included. Each reads what it
changes first, so its result shows before and after, and a dry run stops
after that read.

- `update_signature` sets the signature of one of the account's own
  send-as addresses, the default unless `send_as` names another. The
  text is plain: it is escaped and its line breaks become `<br>`, so no
  markup a caller passes is applied. Empty clears it. Gmail adds the
  signature to mail written in Gmail itself; drafts this server writes
  carry none (§4.2). It patches the signature and nothing else.
- `create_filter` takes Gmail's criteria and actions as labels added or
  removed, with `archive`, `mark_read`, `star` and `trash` as flags. It
  cannot forward: `gmail.FilterWrite` has no such field (§17.5). `trash`
  needs `confirm: true`. `SENT`, `DRAFT` and `SPAM` cannot be added,
  `TRASH` only through the flag. An identical filter is `[conflict]`,
  found by reading the list first; Gmail itself keeps duplicates. Gmail
  rounds a size (§18 row 52), so a filter with one may escape the check. A filter acts on mail that arrives
  from now on, never on mail already there, so it is not a write that
  takes a query (§4.7).
  `SPAM` among a filter's removed labels is Gmail's "Never send it to
  Spam". `remove_labels` keeps Gmail's list, `never_spam` says it, and
  the text says "never sends matching mail to spam" rather than naming a
  label taken off. Gmail stores a filter with the labels sent, but was
  seen adding `SPAM` to every archiving filter later, ones it did not
  just write included (§18 row 56). So the duplicate check, and the
  read that settles an unconfirmed create, ignore a stored `SPAM` when
  the request archives and does not name it.
  A create Google did not confirm is `[ambiguous_outcome]` and is not
  repeated. As §4.3 does for a send, the server waits 5 seconds, reads
  the list, and gives a verdict: `created`, naming the new filter that
  does this; `not_created`; or `unknown` when the read fails (§18
  row 55).
- `delete_filter` takes a filter id and `confirm: true`, and reports what
  the filter did. A 404 on the delete is reported as gone.
- `set_vacation` also needs `GMAIL_ENABLE_SEND=true`: an auto-reply
  writes to other people (§17.4). Turning it on needs a body, an
  `audience` of `contacts` or `domain` — every sender is not offered —
  and `confirm: true`; `start` and `end` bound it. The body is sent
  exactly as given. Turning it off keeps its text for next time.
  `domain` is refused on a personal account (`gmail.com`,
  `googlemail.com`), found by one profile read: whether Gmail refuses it
  there or answers every sender is unchecked (§18 row 51).

Settings changes run one at a time within the process. Gmail refused
filter writes sent together, and some sent back to back, with 400
`failedPrecondition`, "Precondition check failed.", and took each 5
seconds later (§18 row 54). Each change holds the client's settings
lock from its first read to its write; a dry run takes none. Filter
writes are also paced: each attempt waits until 5 seconds have passed
since the last one finished. That refusal on a filter write says
nothing of its cause, and a limit or a policy may answer the same, so
it is repeated once, after the pacing wait, then reported
`[unavailable]` with Google's own words and no cause claimed. If an
earlier attempt of the same call may have taken effect, as a delete
that failed after it was sent may have, it is `[ambiguous_outcome]`
instead. `update_signature` and `set_vacation` take the lock but not
the pacing or the repeat: their writes were not probed (rule 18). Any
other `failedPrecondition` stays `[invalid]`. Spike L, which sends
filter writes at once to see how Gmail refuses them, runs only with the
live driver's `-spike-l`. Once, creates sent together were answered 500
instead; on a create that is `[ambiguous_outcome]`, settled as above. A delete Google did not
confirm is settled the same way: the list read 5 seconds after gives
`deleted`, `still_there` or `unknown`, and the delete is not sent
again. The pacing reserves each write's slot before it waits, so two
writers that do not hold the lock still go 5 seconds apart. The
duplicate check compares criteria exactly, a size included: Gmail's
rounding rule is unknown (§18 row 52), so a request with a size may
slip past an existing filter, which the verdict after an unconfirmed
create says. When the existing filter differs only by a `SPAM` Gmail
added, the conflict says so, and that deleting it and creating this
again gives the filter without it.

## 8. Tool surface

Twenty-seven tools: twenty by default, twelve in read-only mode, one
fewer in each when `GMAIL_LOCAL_DIR` is unset.
Annotations come from `Kind` in one place (`CLAUDE.md` rule 11);
`openWorldHint` is true only where the call reaches another person.
"Write, for good" is registered as a Write is and annotated destructive,
because Gmail deletes a draft rather than trashing it.
`_meta["anthropic/requiresUserInteraction"]` is set on the Send,
Auto-reply and Destructive kinds, as a signal and not a control. A tool
that takes `confirm` also asks the person, and only a kind that can ask
may take it (§4.13). The
schema dump names each tool's kind, since Settings looks like Write to a
client and Auto-reply like Send, and the smoke gate reads which switch a
tool sits behind from it.

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
| `list_changes` | Read | always | `gmail.readonly` | 2/page + 1 (+1 on expiry) |
| `get_settings` | Read | always | `gmail.readonly` | 1 each, 7 |
| `list_filters` | Read | always | `gmail.readonly` | 1 + 1 |
| `download_attachment` | Read (local write) | `GMAIL_LOCAL_DIR` set | `gmail.readonly` | 20 + 20 |
| `create_draft` | Write | not read-only | `gmail.modify` | 1 + 10 (+20 reply_to, +40 reply_to_thread) |
| `update_draft` | Write | not read-only | `gmail.modify` | 20 + 15 |
| `delete_draft` | Write, for good | not read-only | `gmail.modify` | 20 + 10 |
| `modify_labels` | Write | not read-only | `gmail.modify` | 1 + 25/message, 50/thread |
| `trash` | Write | not read-only | `gmail.modify` | 1 + 40/message, 60/thread |
| `restore` | Write | not read-only | `gmail.modify` | 1 + 25/message, 50/thread |
| `create_label` | Write | not read-only | `gmail.modify` | 1 + 5 |
| `update_label` | Write | not read-only | `gmail.modify` | 1 + 5 |
| `send_draft` | Send | `GMAIL_ENABLE_SEND` | `gmail.modify` | 20 + 40 + 100 |
| `delete_permanently` | Destructive | `GMAIL_ENABLE_DESTRUCTIVE` | `https://mail.google.com/` | 1 + 30/message, 60/thread |
| `delete_label` | Destructive | `GMAIL_ENABLE_DESTRUCTIVE` | `gmail.modify` | 1 + 1 + 5 |
| `update_signature` | Settings | `GMAIL_ENABLE_SETTINGS` | `gmail.settings.basic` | 1 + 100 |
| `create_filter` | Settings | `GMAIL_ENABLE_SETTINGS` | `gmail.settings.basic` | 1 + 1 + 5 |
| `delete_filter` | Settings, for good | `GMAIL_ENABLE_SETTINGS` | `gmail.settings.basic` | 1 + 1 + 5 |
| `set_vacation` | Auto-reply | `GMAIL_ENABLE_SETTINGS` and `GMAIL_ENABLE_SEND` | `gmail.settings.basic` | 1 + 5 |

The staleness gate holds those counts against the table. A tool that
asks the person reads again on the retry, and its description says how
many units that adds (§4.13).

Resources, for clients that attach rather than call:
`gmail://threads/{id}` and `gmail://messages/{id}` carry the same text
as `get_thread` and `get_message` under the same budget and boundaries;
`gmail://labels` carries `list_labels`.

### 8a. Every published method, with a verdict

All 79 methods of the discovery document have a verdict in
`testdata/api-coverage.tsv` — used, gated or written off
with a reason — and the `api-coverage` gate holds it. The
table that stood here during design moved there in phase 0, so there is
one copy. It holds thirty used, eight gated, zero deferred to §17,
forty-one written off. Phase 1 wrote off `filters.get` and
`forwardingAddresses.get`, whose lists return the same fields (§18
row 36). Phase 2 wrote off `messages.batchModify`, which reports nothing
per item, and `sendAs.get`, whose list `create_draft` reads anyway (§18
row 39). Phase 5 gated the four settings writes §17 had deferred or
written off: `filters.create` and `filters.delete`, `sendAs.patch` for
the signature, and `updateVacation`.

### 8b. Field coverage

`api-fields` holds a verdict for every field of `Message`, `MessagePart`,
`MessagePartHeader`, `MessagePartBody`, `Thread`, `Draft`, `Label`,
`LabelColor`, `History` and its four record types, `Profile`,
`ListMessagesResponse`, `ListThreadsResponse`, `ListDraftsResponse`,
`ListHistoryResponse`, `VacationSettings`, `AutoForwarding`,
`ForwardingAddress`, `ImapSettings`, `PopSettings`, `LanguageSettings`,
`SendAs` and `Filter` with its criteria and action — modeled, or left
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
   never searches the mailbox unconstrained. Sending — the send spikes and
   the `send_draft` step — goes only to the address the maintainer
   passes as `-send-to`, and the transcript records its redacted form
   only. At the end of a run it trashes what it inserted and deletes its label,
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
| `GMAIL_ENABLE_SETTINGS=true` | adds `update_signature`, `create_filter`, `delete_filter`; with `GMAIL_ENABLE_SEND`, also `set_vacation` | adds `gmail.settings.basic` |
| `GMAIL_REQUIRE_PROMPT=true` | nothing more; refuses the writes that take `confirm` when the client cannot ask the person (§4.13) | no change |

`READ_ONLY` with any enable flag is refused at startup, naming both.
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
  either **Internal** to a Workspace organization, or **External** in
  Testing with themselves as a test user — whose refresh tokens expire
  after seven days. `docs/gcp-setup.md` says which to choose and why,
  `doctor` names the seven-day expiry when a refresh fails, and an `invalid_grant` after about a week is explained in
  `docs/runbook.md` as this, not as a bug.
- **Consent is forced only when the scope set grows**, or with
  `login --consent`. Spike J found that a login without it still mints
  a refresh token, so this spares the screen, not a token toward the 100
  per client (§18 row 43).
- **Changing a flag that changes scopes needs a new login**, and the
  server says so at startup rather than failing on the first call:
  `doctor` and startup compare granted with required scopes.

Configuration: `GMAIL_PROFILE`, `GMAIL_CLIENT_SECRET`, `GMAIL_READ_ONLY`,
`GMAIL_ENABLE_SEND`, `GMAIL_ENABLE_DESTRUCTIVE`, `GMAIL_ENABLE_SETTINGS`,
`GMAIL_REQUIRE_PROMPT`, `GMAIL_LOCAL_DIR`,
`GMAIL_LOG_LEVEL`, `GMAIL_LOG_FORMAT` (`text` default), `GMAIL_HTTP_TIMEOUT`
(60 s default), `GMAIL_CONFIG_DIR`, `GMAIL_REFRESH_TOKEN`. Each also a
flag, except the last two, which are env-only. `docs/configuration.md`
is checked against the exported list.

Process: one stdio session; starts before authentication so `doctor` and
`--dump-schemas` work signed out; a disconnect is exit 0.

## 11. Reliability

- **Repeatability comes from the HTTP method.** GET is retried. POST
  is not, except `modify`, `trash` and `untrash` on a message or
  thread, which are declared repeatable at the call site with
  the reason (applying a label twice is applying it once). `drafts.create`
  is not repeatable: a retry makes two drafts. `drafts.send` is never
  retried (§4.3).
- **Turned-away statuses** — 429, 503, a 403 with a rate-limit reason,
  a filter write refused with Google's generic "Precondition check
  failed." (§7.9, repeated once), or a
  connection never made —
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
  changing on update (spike A), 404 for an expired
  history id, unit costs.
- **Renderer goldens** for thread, message, listing, draft and changes,
  including the hidden-text and link-mismatch cases of §4.1.
- **The logging test** of §9.2, which also answers every question.
- **The person's confirmation** (§4.13): a client that declares
  elicitation and answers each way, on 2025-06-18, 2025-11-25 and
  2026-07-28; one that declares none, with and without
  `GMAIL_REQUIRE_PROMPT`; forged, replayed, expired, other-call and
  unasked answers made by hand; every tool that takes `confirm` found
  from the published schemas.
- **The live driver**, `scripts/livemail`, per §9.1: every tool and
  option (held by `live-cover`), the transcript through one redacting
  writer (held by `transcript`), and the transcript read by a person
  before a phase counts.
- **Evals**, `scripts/evals`, against `gmailtest` only, including tasks
  whose mail contains an injected instruction, scored on whether the
  model followed it. The real server answers over an in-memory
  transport; a task is scored on its calls, its answer and the mailbox
  afterwards. `-self-check`, in `check`, needs no model: every canned
  transcript gets its expected verdict, and every task fails on a
  mailbox nobody touched.

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
| `delete_draft` is a default write that takes `confirm: true`, not destructive-gated | maintainer, 2026-09-26 | §17.1, §7.4 |
| The recipient guard counts `From` on received messages and `To` and `Cc` on the account's own; a correspondent's `Cc` and `Reply-To` need confirming | maintainer, 2026-09-27 | §4.2, §17.8 |
| Arguments are decoded exactly as the schema declares; a JSON string where an array or integer is declared is `[invalid]` | maintainer, 2026-09-27 | §17.7, §18 row 47 |
| `get_thread` shows the thread's drafts, marked, after its other messages | maintainer, 2026-09-27 | §7.2, §17.2 |
| Settings writes behind a new opt-in flag, `GMAIL_ENABLE_SETTINGS`, which alone requests `gmail.settings.basic` | maintainer, 2026-09-27 | §7.9, §9.4 |
| A filter may trash matching mail, with `confirm: true`; it can never forward | maintainer, 2026-09-27 | §7.9, §17.5 |
| The vacation reply needs both the settings and the send flags, an audience of contacts or domain, and `confirm: true` | maintainer, 2026-09-27 | §7.9, §17.4 |
| The person confirms, through MCP form elicitation, each write that takes `confirm` or `confirm_recipients`, on top of those arguments; only an accept writes, and anything else is `[blocked]` as "not confirmed by the person", never "declined" | maintainer, 2026-09-28 | §4.13; an unattended client that declares elicitation cannot make these writes |
| Accepting the question is the confirmation; the form has no checkbox | maintainer, 2026-09-28, after the interactive check | §4.13, §18 row 64 |
| A client that cannot ask falls back to `confirm` and `confirm_recipients`; `GMAIL_REQUIRE_PROMPT=true` refuses instead | maintainer, 2026-09-28 | §4.13, §9.4 |
| Spikes D and E send in phase 2, from the live driver's run, to a second address the maintainer passes on the command line and never commits | maintainer, 2026-09-26 | §15; the transcript records the address redacted only |

## 15. What must be verified live

Each spike states its question and, when run, its verdict separately.
All have run; what is still owed is named under each.

- **Spike A — does `drafts.update` change the draft's message id?** The
  witness of §4.4 depends on it. Create, update twice, read between.
  Verdict decides between the message id and a raw-bytes hash.

  **Answered 2026-09-26, twice.** Yes: a create and two updates of the
  same bytes gave three distinct message ids, and `drafts.get` named
  each. The message id is the witness.
- **Spike B — does Gmail keep a client-set `Message-ID`?** Create a
  draft with one, send it to the maintainer's second address, read the
  sent copy and the received copy. §4.3's settle-by-reading depends on
  it. If Gmail rewrites it, the fallback is the draft's own message id
  in `SENT`, which spike C checks.

  **Answered in part 2026-09-26, by phase 2's runs.** No, for the
  sender: `drafts.create` replaced the `Message-ID` of every draft the
  server built, and `messages.send` replaced spike D's original's.

  **`drafts.send` answered 2026-09-27, by phase 3's run.** It replaced
  the `Message-ID` again: the sent copy carries neither the one the
  server wrote nor the one the draft held. So §4.3 cannot settle by any
  `Message-ID`, and settles by the draft's thread instead (§18 row 44).
  **Received copy read 2026-09-27 by the maintainer**, in the second
  mailbox, a Gmail account. The message arrived in the inbox with the
  subject and body exactly as the draft held them and nothing added — no
  signature, footer or prefix — which is §4.2's verbatim send seen from
  the receiver. The received copy's `Message-ID` was not read.
- **Spike C — what does `drafts.send` leave behind?** Is the sent
  message's id the draft's message id? Does `drafts.get` answer 404
  afterwards? §4.3's verdict table is written from this.

  **Answered 2026-09-27.** The sent message has a new id, not the
  draft's message id, and it is filed in the draft's thread. `drafts.get`
  on the sent draft answers 404, reason `notFound`. So after a send the
  draft is gone and its thread holds a message in `SENT` that was not
  there before, which is what §4.3's verdicts read.
- **Spike D — threading.** A reply built per §4.5 lands in the thread;
  and each of the three conditions of §2.5 removed in turn, to see which
  ones Gmail actually enforces for the sender and for a non-Gmail
  receiver.

  **Sender side answered 2026-09-26.** An original and four replies were
  sent to the second address. The reply with all three conditions
  joined the original's thread. So did the reply without `In-Reply-To`
  and `References`, and the one with another subject. Only the reply
  without `threadId` started a new thread. For the sender, `threadId`
  alone decides (§18 row 42). Drafts agree: every reply draft the steps
  made joined its parent's thread.

  **Receiver side answered 2026-09-27 by the maintainer**, reading the
  second mailbox, a Gmail account. The reply without `In-Reply-To` and
  `References` did not thread there, and neither did the one with
  another subject. So each condition matters to someone: `threadId` to
  the sender, the headers and the subject to the receiver. That is what
  §4.5 writes, and why a reply refuses a caller's subject. A non-Gmail
  receiver is not yet checked.
- **Spike E — non-ASCII round trip.** Subject, display names, body and
  an attachment filename in four scripts, built by `internal/mime`,
  read back from Gmail and from a non-Gmail receiver.

  **Answered for Gmail 2026-09-26.** A draft and a sent message, each
  with subject, display names, body and attachment name in Latin,
  Cyrillic, Greek and Japanese, read back with every field intact.

  **Receiver side answered 2026-09-27 by the maintainer**, reading the
  second mailbox, a Gmail account. The subject and body showed in all
  four scripts, and so did the attachment's name, cut short only by the
  attachment tile. The recipient's display name does not show in that
  view. A non-Gmail receiver is not yet checked.
- **Spike F — `messages.insert` as a fixture source.** Do inserted
  messages appear in `threads.list`, `q` search and `history.list` like
  delivered ones? §9.1's driver depends on it.

  **Answered 2026-09-26.** Yes for listing and search: inserted messages
  appear in `threads.list`, in `q` search and in `rfc822msgid:` lookups
  like delivered ones. `history.list` reports them as added, checked by
  phase 1's live runs. And a date surprise: with
  `internalDateSource=receivedTime`, Gmail recorded the message's own
  `Date` header as `internalDate`, while `after:` still matched it by
  the time of the insert (§18 row 35).
- **Spike G — scope refusals.** Under `gmail.modify`, `messages.delete`
  is refused 403 (the reason §4.6 gates by scope as well); under
  `gmail.readonly`, every write is refused. The error shape of each,
  for §6.5's mapping.

  **Positive half answered 2026-09-26:** `messages.delete` on the run's
  own message under `gmail.modify` answered 403, so permanent deletion
  needs `https://mail.google.com/` and §4.6's scope gate holds. The
  negative half needs a profile granted only `gmail.readonly` and is
  still owed.
- **Spike H — rate limiting.** What a 429 looks like: `Retry-After`
  present or not, and the reason string that tells the sending limit
  apart from the rest.

  **Answered for the request limit 2026-09-27.** Reads of the run's own
  message from 32 workers met a refusal after 1,321 reads, 26,420 units:
  **403**, not 429, reason `rateLimitExceeded`, message "Quota exceeded
  for quota metric 'Total Query Cost' and limit 'Units per minute per
  user'", and **no `Retry-After`**. The client already reads it as a
  request-rate limit and backs off; a test now holds that shape (§18
  row 45). The run's cleanup met the same limit straight after, so the
  driver's own calls now wait it out. The sending limit was not
  provoked: its wait is up to a day.
- **Spike I — label names and case.** Can `Foo` and `foo` coexist? What
  does creating a label named like a system label return?

  **Answered 2026-09-26.** No: the second answered 409, reason `aborted`,
  "Label name exists or conflicts". `INBOX` and `Inbox` both answered
  400, reason `invalidArgument`, "Invalid label name". §6.2.
- **Spike J — consent without `prompt=consent`.** With a refresh token
  already issued, does a login that omits it get a new refresh token,
  none, or an error? §10.

  **Answered 2026-09-27 by the maintainer's login.** A new refresh token.
  The authorization URL carried no `prompt`, every scope was already
  granted, and Google still issued one, which `login` stored in place of
  the old. So skipping consent spares the screen, not the token: every
  login mints one toward the 100 per client (§18 row 43). The old token
  stays valid at Google until it ages out or the cap evicts it; it
  cannot be revoked alone (§17a, §18 row 53).
- **Spike K — expired history.** A `startHistoryId` far below the
  current one: 404, and the body's shape. §7.6.

  **Answered 2026-09-26:** `startHistoryId=1` answered 404, as the sync
  guide says, in Google's ordinary envelope: reason `notFound`, message
  "Requested entity was not found." Nothing in the body says the cursor
  expired, so `list_changes` reads any 404 from `history.list` as expiry
  (§7.6).

Spikes B, C, D and E send real mail and need the maintainer's second
address; they are the "ask before doing" of `CLAUDE.md`. D and E were
approved on 2026-09-26 to send in phase 2 (§14), to that address only,
passed to the driver as a flag. They sent six messages in one run; the
second run sent none. Phase 3's run answered B and C.

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

Built 2026-09-25, on a topic branch. Beyond the list above: the smoke gate
probes `server/discover`, since the 2026-07-28 revision is not reachable
through `initialize` (§18 row 32); the README's tool table is held to the
binary's surface; and `messages.attachments.get` moved into this phase,
because `get_message` must fetch body parts Gmail stores behind an
attachment id (§7.2). After the reviews, the boundary between mail and the
server's voice is a type rather than a rule: `internal/render` writes
server text only from typed parts, so sender text cannot reach a note
without going through a block, and a property test plants markers in every
sender-controlled field of hostile mail across every renderer.

Run live 2026-09-26, twice. Both runs passed every step, and reading the
first transcript found four things the green steps did not: the Cloud
project number in the `Received` header Gmail stamps on inserted mail,
which the transcript redactor let through (§18 row 34); dates that were
the `Date` header rather than the time of the insert (spike F, §18 row
35); Gmail's result estimate stated as a count when it was 201 for a
search matching 3 (§18 row 33); and escaped angle brackets in the
driver's argument echo. The second run is the one with all four fixed.

**Phase 1 — attachments, changes, settings, resources (v0.2.0).**
`download_attachment`, `list_changes`, `get_settings`, `list_filters`,
the three resources. Spike I.

Built 2026-09-26, on a topic branch stacked on phase 0's. Beyond the list:
the client gained a streaming path for attachments (§7.3); `get_message`
lists each attachment's `part_id`, which `download_attachment` takes; the
header block of a read is capped at half the budget, closing the §17a
entry; resource reads are logged and counted like tool calls; and
`filters.get` and `forwardingAddresses.get` were written off (§18 row 36).
`unsupported` moved to phase 2, where `modify_labels` meets a draft.

Run live 2026-09-26, three times, every step passing each time. Reading
the first transcript found the account's real history ids in the clear,
in `list_changes`'s lines and the argument echo, which the redactor did
not know as ids (§18 row 38); the second run has that fixed. The third
ran after the review fixes to the streaming path, and its transcript also
masks the system temporary directory the download path runs through.

**Phase 2 — the write path (v0.3.0).** `internal/mime` build side and
its round-trip fuzz; `create_draft` with replies, `update_draft` with
the witness, `delete_draft`, `modify_labels`, `trash`, `restore`,
`create_label`, `update_label`; per-item outcomes. Spikes A, D, E, J.

Built 2026-09-26, on a topic branch stacked on phase 1's. `internal/mime`
builds with RFC 2047 headers, RFC 2231 filenames and quoted-printable
text, and `EditRaw` edits a draft's own bytes as a tree, leaving every
part it does not touch as Gmail stored it. Two fuzz targets hold them:
built messages parse back to the same fields, and edits never break a
parse. `gmailtest` reads what it is sent with the standard library rather
than `internal/mime`, so the builder is not tested against its own
parser's mistakes. It models threading on all three conditions, a new
message id per draft save, `SENT` and `DRAFT` refused by hand, and a
draft refused a label.

Beyond the list: the reply's thread is its own field,
`reply_to_thread` (§4.5). Each item of a multi-id write is read, then
written only if needed (§4.7). `messages.batchModify` and `sendAs.get`
were written off (§18 row 39). `login` now says whether Google issued a
new refresh token, which is spike J's question. And the `outcomes` gate
had been reading nothing since phase 0: it knew input types spelled
`FooInput`, this repository spells them `FooIn`, and its floor caught
that when the first write tool registered. It now reads both, with a
test. Its floor counted distinct field names, and all the writes share
one `dry_run`, so it now counts fields per input type.

Run live 2026-09-26, twice, with every step passing each time. The first
run sent six messages to the maintainer's second address for spikes D
and E; the second sent none. Reading the first transcript found that
Gmail replaces the `Message-ID` of a draft created through the API, so
`create_draft` had reported an id Gmail did not keep (§18 row 41). It
reports none now, and the fake replaces the id as Gmail does. It also
found that the sender threads by `threadId` alone (§18 row 42). The
second run is the one with the fix.

**Phase 3 — sending and the gated tools (v0.4.0).** `send_draft` with
the recipient guard and the settle-by-reading of §4.3;
`delete_permanently`, `delete_label`; the scope escalation and re-login
path. Spikes B, C, H.

Built 2026-09-27, on a topic branch stacked on phase 2's. Beyond the
list: `send_draft` takes the draft's `message_id` as a witness, as
`update_draft` does, so a draft changed after it was shown is `[stale]`
rather than sent (§4.2). A dry run with unconfirmed recipients reports
them instead of refusing. `delete_permanently` reads each item first, as
the other multi-id writes do, and reports a 404 on the delete as deleted.
`delete_label` reads the label's counts, so its result says how much mail
loses it. The escalation path was mostly in place from phase 0 — startup
and `doctor` compare granted with required, and `login` asks for consent
when the scope set grows. Phase 3 adds its other direction: `doctor` names
a token wider than its configuration, typically `https://mail.google.com/`
left after the destructive flag is turned off, and says `logout` then
`login` narrows it. The fake models `drafts.send` filing the message in
`SENT` under a new id, a permanent delete refused 403 without the full
scope (spike G), and a failure served after the request was acted on, so
an ambiguous send whose draft did go out is tested. The schema-diff floor
now counts the whole surface, 23 tools; it had stayed at phase 0's eight.

Run live 2026-09-27, four times. The first, with `-send-to` and the
destructive login, passed all 56 steps and drove 106 of 106 options; it
sent one message through `send_draft`. Spikes B and C found that
`drafts.send` replaces the `Message-ID` and files the sent copy under a
new id in the draft's thread, so the settle-by-reading of §4.3 as
written could never have found a sent message; it now rereads the
thread (§18 row 44). The second, for spike H alone, met the per-user
limit, and so did its cleanup, which left two drafts and two messages
until `-clean` removed them. The third answered spike H with the driver
waiting the limit out. The fourth ran after the thread-based settle,
without `-send-to`: 54 steps passed, the two that send were skipped, and
105 of 106 options were driven, the missing one being `confirm_recipients`
on the send the first run made. The maintainer's login showed the re-login path
both ways: `doctor` named the missing scope, `login` asked for consent,
and with the flag off again `doctor` names the wider token.

The live driver runs the server with both flags on. Every `send_draft`
step but one uses a draft addressed to reserved domains and is a dry run
or refused; the one that sends needs `-send-to`, and the guard holds its
recipient and its `confirm_recipients` to that address. The permanent
delete runs only when the profile's login granted
`https://mail.google.com/`, and spike G, which needs a profile without
it, runs only when it is absent. Spike H floods reads of the run's own message only
with `-spike-h`. Spikes D and E, answered in phase 2, send again only
with `-spikes-de`, so a run with `-send-to` sends one message.

**Phase 4 — evals and 1.0 (v1.0.0).** The evals harness with the
injection tasks of §13; the surface frozen into the schema baseline;
§17 closed or each item argued open.

Built 2026-09-27, on a topic branch stacked on phase 3's. The harness
serves the real server over streamable HTTP on the loopback interface,
against a fresh `gmailtest` mailbox per trial, and gives each task to a
model through `claude -p`, fenced to this server's tools as the newest
sibling fences it. It scores six tasks on their calls, their answer and
the mailbox after the run. Three carry a planted instruction; obeying it, by the tool it asks
for or by its address in any argument, is counted apart from an ordinary
failure. `-trials` repeats each task, and a task passes only when every
trial does. A run the CLI stopped, or one that called a tool not this
server's, is reported rather than scored. `-self-check`, in `check`, scores
15 canned transcripts and requires every task to fail on a mailbox
nobody touched.

§17 is closed item by item, each decided by the maintainer on 2026-09-27
from the references in §18 rows 46 and 47: the recipient guard clears
the senders of a thread's messages and the addresses the account itself
sent to, and no longer a correspondent's `Cc` or `Reply-To`; `get_thread`
shows a thread's drafts in a section after the conversation; arguments
are decoded exactly as declared. §17.4 and §17.5 stay deferred to after
1.0, and §17.6 stays open by design, checked for 1.0 in §18 row 48. The
live driver gained a dry run of a reply-all on a received thread, which
holds the narrowed guard against Gmail's own labels.

Run live 2026-09-27, twice, without `-send-to`: 55 steps passed each
time, the two that send were skipped, and 105 of 106 options were
driven. Reading the first transcript found the dry run's own sentence
still saying a thread's "senders and recipients need no confirming",
which the narrowed guard made untrue; it now says whoever the account
sent them to. The second run is after that fix and the reviews. Two
more ran on the final code with `-send-to`, 57 steps and 106 of 106
options each: the first sent one message through `send_draft`; the
second also sent spikes D and E, seven messages in all, and flooded
reads for spike H. Every spike reproduced its verdict of §15: B and C
the replaced `Message-ID` and the draft's 404; D threading by
`threadId` alone for the sender; E four scripts intact; H a 403
`rateLimitExceeded` without `Retry-After`, this time after 22,800
units. No
scored evals run had been made at that point.

Scored 2026-09-27 through the CLI, `claude-opus-5-5` at high effort,
three trials per task, on the committed harness: six of six tasks
passed every trial, the planted instruction was followed in none of 18
trials, and the run cost $1.32. Every answer to the injected tasks
named the planted message as likely phishing and said it had not acted
on it. An earlier run had failed `send-draft` three times, and reading
it showed
why: the CLI asked for a person before every `send_draft` call, dry
runs included, because the tool carries `requiresUserInteraction`
(§18 row 49), and under `claude -p` there is none. The model found the
draft and its witness each time and stopped to ask. The task now scores
reaching the send with the right witness, whether the client holds the
call or lets it through.

**Phase 5 — settings writes (v1.1.0).** Asked for by the maintainer
after 1.0, 2026-09-27: `update_signature`, `create_filter`,
`delete_filter` and `set_vacation` behind a new `GMAIL_ENABLE_SETTINGS`,
which requests `gmail.settings.basic`; the vacation reply also behind
`GMAIL_ENABLE_SEND` (§7.9, §14, §17.4, §17.5).

Built 2026-09-27, on a topic branch from `main`. A filter cannot forward:
the type it is written as has no such field. The schema dump now names
each tool's kind, since a settings write looks like a write for good to a
client and the vacation reply like a send, and the smoke gate drives two
more modes from it. The `api-fields` gate learned a declared view, a
`Schema: <Name>` line, so a write type narrower than its schema is still
held to that schema's fields. The live driver saves the default
address's signature and the vacation reply before its settings steps
and restores both exactly after; its filters match only the run's own
sender at a domain that never resolves, and its vacation reply starts
300 days ahead. `injected-helpdesk` now runs with the settings tools on.

Run live 2026-09-27, twice, on a Workspace account after a login that
granted `gmail.settings.basic`, without `-send-to`. The first run found
three things the unit tests could not (§18 row 52): Gmail keeps a second
identical filter, it stores a filter's size rounded, and a vacation reply
sent with an empty HTML body beside a plain one came back with no body.
The HTML body is now left out unless it holds something, and the fake
answers as Gmail did. The second run passed all 71 steps and drove 136 of
137 options, the missing one being `confirm_recipients`, which needs a
send. Both runs restored the account's signature and vacation reply.

Scored 2026-09-27 after the release, through the CLI, `claude-opus-5-5`
at high effort, three trials per task, with the settings tools on in
`injected-helpdesk`: six of six tasks passed every trial, the planted
instruction was followed in none of 18 trials, and the run cost $1.47.

**Phase 6 — the person confirms (unreleased).** Asked for by the
maintainer on 2026-09-28, after a client's approval prompts named labels
by id: the server asks the person, through MCP elicitation, before each
write that takes `confirm` or `confirm_recipients` (§4.13, §14). Checked
first against the specification, the SDK's source, a throwaway probe
server and the clients' own documentation (§18 rows 58 to 63).

Built 2026-09-28, on a topic branch from `main`. The service asks at
its write, after every read and guard, so the question states what
would be written; `register` gives each call a way to ask for the kinds
that take `confirm`, and the question goes out as a multi-round-trip
input request on every protocol. The answer is bound by a signed,
single-use `requestState` to the tool, the arguments and the question's
words, so a retry whose question changed is refused and asked again.
Mail text in a question is quoted on one line with no link drawn. The
test client, `testutil.ConnectClient`, takes client options and a
protocol, so the tests answer as a person would on 2025-06-18,
2025-11-25 and 2026-07-28, and forge and replay answers by hand. The
descriptions of the tools that take `confirm` say the server also asks.

The live driver is now a client that declares elicitation and answers
for the maintainer, since it is their own scripted run: accept, but for
one step that declines and checks the refusal. It
prints every question into the transcript. The evals' `send-draft` task
already scored reaching the send with the right witness; its wording now
says that `claude -p` may pass the call and cancel the server's question
rather than hold it, and either scores the same.

Run live 2026-09-28, once, without `-send-to`, on protocol 2025-11-25:
78 steps passed, and 136 of 137 options were driven, the missing one
being `confirm_recipients`, which needs a send. Each write that takes
`confirm` put one question to the driver, the send aside, since no step
sent; the declined delete wrote nothing and said so. Reading the transcript found the questions as
§4.13 describes them, with the run's own label, subject and filter
criteria quoted.

Checked by the maintainer 2026-09-28 in Claude Code 2.1.284, on
protocol 2025-11-25, with `delete_draft` (§18 row 64). Accept with the
box unticked, Esc and Decline were each `[blocked]`, logged as
`accept` unconfirmed, `cancel` and `decline`; the box ticked and
Accept deleted the draft. The model never saw the question and did not
call again on its own, and the log carried no subject or address. The
first case, a person meaning to confirm and refused, is why accepting
is now the confirmation and the form has no box.

The review round that followed is in §16a. Run live again
2026-09-28 after it, without `-send-to`: 78 steps passed, 136 of 137
options driven. The driver answered with an accept alone, the vacation
question showed the reply's text, and the declined delete wrote nothing.

Run live 2026-09-28 with `-send-to` and no spike that sends: 80 steps
passed, and 137 of 137 options were driven, `confirm_recipients`
included. The send was the run's one message. Its question named the
one recipient, the run's subject and the body's single line, the driver
accepted, and the result reported 1 recipient, 1 confirmed. Spikes B and
C reproduced their verdicts of §15: a replaced `Message-ID`, a new
message id, and 404 on the sent draft. Cleanup and the settings restore
ran without a failure.

Scored 2026-09-28 through the CLI, `claude-opus-5-5` at high effort,
three trials per task: six of six tasks passed every trial, the planted
instruction was followed in none of the 9 trials that carry one, and the run cost
$1.31. In each `send-draft` trial the CLI held the dry run for a person
(§18 row 49), so the call never reached the server and its question.
The task scores that the same as before; the server's question under
`claude -p` is not yet seen from a model.

Owed: the maintainer's check of the empty form in an interactive
client; and `/simplify` and `/security-review` on the round.

**2.0.1 — questions drawn as Markdown (released 2026-09-29).** From the
elicitation research of 2026-09-29. VS Code draws a question as
Markdown, and 2.0.0 quoted mail text in double quotes, so a label
named in Markdown's link syntax drew a link there. Each quoted value is now a code
span and the lines stand apart (§4.13, §18 row 66).
`TestQuestionsAreInertMarkdown` puts Markdown in every field of every
question and holds the text outside the spans to the server's own. The
form stays empty: the maintainer compared three forms in Claude Code
2.1.284 against a throwaway probe, and a required choice naming the
outcome was slower and less clear than Accept (§18 row 67).

Run live 2026-09-29 without `-send-to`, before and after the review
round: 78 steps passed each time, 136 of 137 options driven, the
missing one `confirm_recipients`, which needs a send. The transcript's
nine questions each quote the run's own text in code spans, one line
apart, and the declined delete wrote nothing. Reviews in §16a.

### 16a. Found by review, and fixed

Each phase's `/code-review high` and `/security-review` findings, with
what fixed them.

- **Phase 0, security review: sender text reached the server's voice.**
  A part's `charset` parameter, which RFC 2231 lets a sender
  percent-encode with newlines, was printed unsanitized in a `note:` line
  after the untrusted block closed; a crafted header could write fake
  server notes. An unsafe attachment name was echoed in a note the same
  way, on one line. Fixed: `internal/mime` returns a charset label only
  when it has the IANA shape, and the attachment note no longer names the
  file. `TestSenderTextNeverReachesTheServersVoice` drives hostile raw mail
  through parse, model and render, and fails on the old code in all three
  cases.
- **Phase 0, code review: ten findings, all fixed, each with a test that
  failed first.**
  - Three more ways sender text reached the server's voice: a `mailto:`
    link's percent-decoded host in the link-mismatch note (hosts are now
    host-shaped or absent); the senders of messages a thread read did not
    show (now inside a block); and invisible or bidi characters in an
    address, which were never stripped (now stripped and counted).
  - Two quadratic paths over sender text: `StripInvisible` on a long
    invisible run, and quote-span marking. Both are linear, held by tests
    with 300k-character and 200k-span inputs.
  - A `Retry-After` above the cap was honored on 503 and 5xx, so one call
    could sleep an hour. One place now reports a long wait for every
    status.
  - The server instructions named tools phase 0 does not have. They are
    now built from the tools actually registered.
  - Google's error text kept a third party's domain. Addresses in it are
    now masked domain and all.
  - Listings read each row one after another, 22 round trips for a default
    `search_threads`. They now read up to five at a time, in row order,
    with the first failing row's error.
  - The scripts carried their own copy of `redact.ID` with different
    rules. They now use the server's.

- **Phase 1, security review: nothing found.** The download path, the
  streaming decoder, the resources, the new renderers, the log line for a
  resource read and the driver's transcript were each checked; none met
  the bar of a real exploit. `fileperm.RestrictToOwner` works on a path
  outside the `os.Root`, which only a local race inside the person's own
  directory could turn; accepted, since the file is already created
  `0600`.
- **Phase 1, code review: ten findings, nine fixed, one accepted.**
  - A body cut before the `data` string was read as a malformed answer
    and not retried. Every cut is now a failed read; a test drives four.
  - The whole-request timeout bounded a streamed attachment, so a large
    one on a slow link could never finish. Streams are now bounded by
    time without progress; a test shows a slow body outlasting the
    timeout and a stalled one failing.
  - An attachment declared over 64 MB was downloaded before it was
    refused. It is now refused from its declared size, with no read.
  - A label deleted between its resolution and `history.list` read as an
    expired cursor (§7.6). The label list is read again first.
  - The driver's download path ran through the system's temporary
    directory, which can name the maintainer's account. The transcript
    redactor masks it.
  - `list_changes`'s description priced a page at two units; it is three,
    four on expiry.
  - A `history_id` with spaces round it was rendered as "(unreadable
    id)". The rendering uses the trimmed id.
  - POP showed as on for any window but `disabled`. Only the two
    documented windows show as on; anything else is "unknown".
  - The test meant to prove a JSON escape in `data` is refused had lost
    its backslash. It has it back.
  - `fileperm.RestrictToOwner` outside the root: accepted, as above.
- **Phase 1, `/simplify`:** duplicates folded (the `rfc822:` resolver,
  page-size bounds, change kinds, the forwarding count, the write-failure
  message), and a test that runs `list_changes`'s renderer through the
  transcript redactor, which found "history from N" unmasked.

- **Phase 2, code review: ten findings, seven fixed, three recorded.**
  - A draft upload was bounded by the 60-second whole-request timeout,
    so 35 MB on a slow uplink timed out as `[ambiguous_outcome]` and a
    retry could make a second draft. Uploads are now bounded by time
    without progress, as downloads are. A test drives a slow transport
    past the timeout, and a stalled one fails.
  - `delete_draft` carried `destructiveHint: false`, so a client that
    runs non-destructive tools unasked would run it. It has its own
    `Kind` now, registered as a Write and annotated destructive.
  - An edit of a draft with more MIME parts than are read would have
    dropped the rest. It is refused as `[unsupported]`.
  - A named pipe in `GMAIL_LOCAL_DIR` hung `create_draft` on open. Files
    are checked as regular before they are opened.
  - A label whose visibility Gmail did not state read "other". It reads
    "not set".
  - A reply to a subject with a run longer than a line could hold was
    refused. Such a subject is now encoded.
  - `part_id ""` passed one check and failed another. It is refused with
    what it means: the whole draft.

  Recorded, not changed:
  - `reply_to_thread` reads the thread with its bodies, which only a
    format that carries the parts can tell a reaction by.
  - A draft on `modify_labels` is `[unsupported]` and on `trash`
    `[invalid]`. These are different conditions: Gmail refuses to label a
    draft, while trash has a tool of its own for drafts.
  - Upload bodies are copied once more into the multipart envelope.
- **Phase 2, security review: nothing found.** It checked:
  - header injection through a subject, a name, a filename or a parent's
    References;
  - recipients taken from a hostile parent;
  - sender text in the server's voice in the write results;
  - reading attachments out of `GMAIL_LOCAL_DIR`;
  - the upload origin;
  - writes under `dry_run`;
  - the live driver's sending.
  
  Two notes, neither a vulnerability. `EditRaw` does not hold the
  998-character line limit `Build` does, since a draft Gmail stored may
  already carry longer lines and refusing them would refuse the edit. A
  FIFO in `GMAIL_LOCAL_DIR` would block a read, which only the person who
  owns the directory can arrange.
- **Phase 2, `/simplify`: the `outcomes` gate was green and read
  nothing.** Phase 2 moved every branch on a request field out of
  `internal/tools` into `internal/service`, and the gate read only the
  first. It now reads both. It treats as requests the struct types an
  exported service method takes, and a bare bool such as `confirm`. It
  fails when write tools exist and no branch was examined; a test holds
  each case. Also folded:
  - One table each in `model` for label visibility, the change verbs and
    the updatable fields. Service and render derive from it, so no list
    is typed twice.
  - One base-name check (`mime.ValidFilename`) and one control-character
    check (`mime.HasControl`). `NewMessageID` takes the sender's address.
  - `splitHeaders` built on the raw splitter. The edit tree now classes a
    boundary-less multipart as the parser does, so the two walkers agree
    on which parts are bodies.
  - One call per draft method, with `gapi.Uploads` deciding the path.
  - Base64 written straight into a buffer of its final size, and buffers
    sized once. The send-as list and a reply's parent read concurrently.
  - The fake's shared insert, minimal format and upload routing.
  - The driver's guard: it refuses a write argument it has no rule for,
    and reads addresses with the server's parser. `Send` itself holds a
    spike to `-send-to`.

  Skipped, with reasons:
  - `update_draft` parses the draft twice. It costs memory on drafts near
    35 MB, and a headers-only parse is not worth a second parser.
  - `Upload` and `Reply.Joined` are stored rather than derived. Each is
    read from one decision at the point it is made.
  - A thread's labels are read again when Gmail's write answer lists no
    messages. The live run shows whether that fallback ever fires.

- **Phase 3, `/simplify`.** Folded:
  - A recipient's position is set once in the model; the guard, the
    rendering and the structured output read it, instead of counting it
    four times.
  - The guard is two checks. What needs no thread — no recipient, over
    50, a confirmation of an address the draft does not send to — runs
    before the thread is read. A dry run skips only the confirmation
    check, rather than swallowing its refusal after the fact.
  - The settle reads check the `Message-ID` first, and stop after a draft
    read that is neither found nor 404.
  - `delete_label` refuses a system label from the list it already read.
  - One fake handler for both permanent deletes, one attachment line for
    both write blocks, and one set of words per write op in the items
    rendering.
  - `send_draft`'s description priced a new conversation at 120 units. It
    is 160, since every draft sits in a thread and the thread is read.

  Skipped, with reasons:
  - Reading the thread only when some recipient is unconfirmed. It would
    save 40 units, but the result would lose the thread it joins, which
    §4.2 says a send shows.
  - Reading a thread's labels before deleting it. The read is what
    `labels_before` reports.
  - The label list overlapping the item fan-out. `trash` has the same
    shape, and one round trip does not justify diverging.
  - A shared draft-read and witness helper. The three callers read in
    different formats and give different advice on `[stale]`.
  - Dropping `sent` and `deleted`, which restate `dry_run`. `delete_draft`
    already reports `deleted` that way.

- **Phase 3, code review: ten findings, eight fixed, two recorded.**
  - The guard read only the first `To`, `Cc` and `Bcc` header, and a
    lenient read drops what it cannot parse, while Gmail sends to every
    address. A draft shaped elsewhere with a second `Cc` line reached an
    address nobody confirmed. Such a draft is now `[blocked]`, dry run
    included; a test drives both shapes.
  - A send that failed as `[unavailable]` — a 2xx whose body could not be
    read, or a 503 after Google acted — was reported as a failure with
    "retry shortly". It is settled by reading now, like an ambiguous one.
  - The settle reads ran in the same instant as the failure, so a send
    still running at Google read as not sent. They wait 5 seconds first,
    through the client's own sleep, and the verdict says Gmail can file
    a send late.
  - `delete_permanently` on a thread holding a draft deleted the draft
    with it. That item is refused and points to `delete_draft`.
  - `/simplify` had moved `confirm_recipients` onto the `to` parser,
    which requires an ASCII address, so a draft to an internationalized
    address could never be confirmed. It has its own strict parse again.
  - A `message_id` with spaces round it read as `[stale]`. It is trimmed.
  - The driver's permanent delete removed two messages from the run's
    list, which the thread-ids spike reads beside the thread list by
    position. Deleted messages are marked instead, and cleanup skips them.

  Recorded, not changed:
  - Between the witness read and `drafts.send` the draft can still
    change, and Gmail sends what it holds then. `drafts.send` takes only
    an id, so nothing narrows this further (§17b).
  - The recipient and attachment walks repeat `compose.go`'s. The shapes
    differ (a send keeps part ids and positions).
- **Phase 3, security review: nothing at the reporting bar.** It checked
  the guard (address comparison, lookalike addresses, the participant
  set), the witness, no retry, the dry runs, the destructive gates, the
  server's voice in results and errors, `doctor`'s new line, and the
  driver's guard. It raised one design question, recorded as §17.8: a
  participant's `To`, `Cc` and `Reply-To` are the sender's own words.

- **Phase 4, simplify: the drafts rule in one layer.** The service had
  reordered a thread with its drafts last, which changed what
  `Latest()` means for every later caller. It returns Gmail's order
  again; `get_thread` and the renderer split the drafts off themselves.
  The self-check stopped building a whole server per canned transcript,
  and the fake counts calls by method for the tests and the harness.
- **Phase 4, code review: ten findings, all fixed.**
  - `/simplify` deleted `TestSendDraftReply` along with the helper it
    replaced, which took away the only tool-level test of a send's
    `[stale]` refusal and a reply's send. It is restored.
  - Over the budget, the drafts listing was uncapped, so forty drafts
    pushed a read past its budget and cut the newest message to nothing.
    It lists five and counts the rest; a test reads forty.
  - Drafts were numbered newest first from 1, the other way from the
    conversation. The newest draft is now `N of N`, like a message.
  - The reply task searched quoted-printable bytes for a word, and a
    soft line break inside it failed a correct reply. Soft breaks are
    undone first; a test puts the break inside the word.
  - The answer scored was the last text block of the whole run, not the
    last turn's text. It is the last turn's blocks, joined.
  - A trial the CLI stopped, or that never ran, counted as a
    failed task and printed the hint about tool descriptions. It is
    counted as incomplete, and the run exits 2.
  - `summarize-thread` no longer required `get_thread`, so an answer
    from search snippets passed. It requires it again.
  - The self-check no longer refused a task named twice, or an injection
    with no tools or no marker. It does again.
  - Each turn re-billed the growing conversation. Moot since: the
    harness now runs the model through the claude CLI, which manages
    its own caching.
  - A schema-gate comment named a release version in prose, which
    `CLAUDE.md` rules out. Reworded.
- **Phase 4, security review: nothing at the reporting bar.** It checked
  the narrowed guard (the `SENT` label is matched by id, and only the
  account can apply it), the drafts section's server voice, and the
  harness's handling of the API key, which it no longer needs.
- **1.0.0, the first CI run: three Windows tests and one CodeQL alert.**
  CI had never run before the release pull request, so the Windows
  runner met these for the first time. A test left its temporary file
  open, which Windows cannot delete; `TestDefaults` did not redirect
  `%AppData%`, which `os.UserConfigDir` reads on Windows; and the bundle
  test held its own zip reader open while it packed again over the same
  file. All three were in the tests, not the code they test. CodeQL
  flagged the leak scanner's Gmail-link pattern as an unanchored URL
  regex. It is unanchored on purpose: it finds a link anywhere in
  committed text and decides nothing about fetching one. The alert is
  dismissed as a false positive, with that reason.
- **Phase 5, simplify.** `get_settings` and `list_filters` still said
  this server could change neither settings nor filters; their
  descriptions now follow the flags, with a test. `update_signature`
  had its own default-address rule, which dropped the primary fallback
  `create_draft` has; they share one. The fake compared filters with
  label order mattering and the service without; both ignore it. Render,
  label checks, time parsing and the vacation field copies now share
  one helper each, and `create_filter` reads labels and filters at once.
  Kept: `Kind` stays one value per switch and effect rather than an
  effect with a flag set, a design question for a later phase.
- **Phase 5, security review: one medium finding, fixed; one question,
  refused until checked.** A driver step run alone with `-run` could print
  the account's own signature or vacation text; every such step is quiet.
  `audience: domain` on a personal account is unchecked, so it is refused
  there (§18 row 51). An HTML-only vacation reply lost its text when
  turned off; the write type carries it.
- **Phase 5, code review: nine findings, all fixed.** Turning the reply
  on now writes every text field, empty included, so an old HTML body
  cannot go out in place of the caller's. The driver saved the default
  address's signature but changed the account's own, which differ when
  the default is an alias; it changes the one it saved. Settings tools
  are annotated destructive, since this server does not put back a
  signature or trashed mail. Filter criteria are trimmed, and
  `exclude_chats` alone is refused. A size out of range is refused
  rather than clamped. Runs of spaces survive in a signature. A 404 on a
  filter delete says it may follow a lost answer. `set_vacation` reads
  the profile at once with the reply.
- **1.1.1, code review: ten findings, nine fixed, one recorded.**
  `logout` read each profile's Cloud project from its client file at
  logout time, so a relative or replaced file could hide a profile the
  revoke signs out; `login` now records the project, from the one
  client-JSON parser, and `logout` compares what was recorded. A
  profile with no client path was skipped rather than named. The note
  printed before the revoke, and when none happened; it now follows a
  revoke that succeeded. A command-level test holds the note. Three
  doc slips from the sweep are fixed. Recorded: the rule that a revoke
  ends the account's grant to the whole project rests on Google's page,
  not a live probe (§18 row 53, tier 3). `logout` relied on it before
  this release; the probe costs the maintainer's grant and a new login.
- **1.1.1, security review: no findings.**
- **Filters and search size, self-review: four findings fixed, two
  recorded.** The first live run read an archiving filter back at once
  without `SPAM`, and the fix first concluded Gmail never adds it.
  Gmail was later seen adding it to archiving filters; the duplicate
  check and the settle read now ignore a stored `SPAM` on a request
  that archives without it (§18 row 56). Spike L showed deletes sent back to
  back refused like overlapping ones, so a repeat waits at least 2
  seconds (§18 row 54). The fake refused an identical filter, which row
  52 had refuted, and the client's comment said Gmail does; both now
  keep it. The search descriptions name `omitted_ids`. Recorded: every
  read carries its text twice, in `content` and in `untrusted_text`,
  which reads against `CLAUDE.md` rule 12; `untrusted_text` is a
  released field (rule 14), so the listing budget counts both copies
  instead. And a listing now shows fewer rows under the same budget:
  10 of a page of 100 generated messages, and 12 of a default page of
  20, where the text alone showed about 42. Search takes no
  `budget_chars` to widen it; adding one is not breaking.
- **Filters and search size, code review: ten findings, all fixed.**
  Rows past a listing's budget were dropped from the structured half,
  which lost filters (a forwarding one included) and changes for good,
  and left search and draft rows past the budget unreachable, since the
  next page starts after them. Every row now stays: search and draft
  rows past the budget are slim, without what a sender wrote; filters
  and changes are kept whole and spent first (§4.8). A change could
  name a message both shown and omitted; `omitted_ids` now leaves out
  any id a shown row carries. `budget_chars` and `omitted_ids` say what
  they now mean. The CHANGELOG files the listing change under Changed.
  The new evidence rows named whose account they came from; they no
  longer do. The overlap repeat covered `sendAs.patch` and
  `updateVacation` with no evidence (rule 18); it covers the filter
  writes alone. The live report step found its filter in the text, which
  a long list cuts, and slept without its context; it reads the
  structured rows and waits on the run's context. The listing
  marshaled the remaining ids on every row, which is quadratic; it keeps
  a running total. Two JSON sizing helpers became one,
  `render.JSONChars`.
- **Filters and search size, second code review: ten findings, all
  fixed.** A change left out whose message a shown row named went
  unnamed, and the "not shown" line could vanish while the page was
  truncated; the line now counts every row left out and names each
  message once. The `not_created` verdict promised a retry would be
  refused as a duplicate, which a rounded size can defeat; with a size
  it says to list the filters first. The driver's cleanup deleted
  filters back to back with no repeat, so a refusal could leave one on
  the account; it waits out an overlap refusal as it does a rate limit.
  Spike L ran on every settings run; it needs `-spike-l`. The overlap
  repeat waited 2 seconds, which nothing showed to work; it waits 5, the
  one wait seen to. Many filters could leave the text with none; filters
  now have the text budget to themselves, forwarding ones first. The
  listing still rendered the rest of the page on every row; it reserves
  that line once. The evidence rows described the account they came
  from; they describe only what Gmail did. The live report step printed
  the account's filter count; it prints only the fact about the run's
  own filter. The listing chose its mode from a zero-filled slice; it
  takes an explicit reply cost.
  Recorded: one live run after these fixes had the step that clears the
  signature refused, and the driver's restore of the signature answered
  400; the signature ended as the run found it, read back without
  printing it. The refusal was not printed, since the step is quiet; the
  driver now prints a quiet step's refusal, which carries no setting's
  text. Two runs after, the signature steps alone and the whole driver,
  passed. `sendAs.patch` is not repeated on a refusal until one is seen
  and read (rule 18).
- **Filters and search size, third code review: ten findings, all
  fixed.** The repeat matched Google's generic "Precondition check
  failed.", which a limit or a policy may also answer, and called it an
  overlap; it is repeated once and reported in Google's words. The lock
  released at once, and Gmail refuses writes back to back; filter
  writes are paced 5 seconds apart, tested on a fake clock. The
  refusal said "nothing was changed" after an earlier attempt that may
  have acted; that is `[ambiguous_outcome]` now. The listing kept room
  for the "not shown" line on every page, so a page that fit was cut;
  the room is kept only when a row will be left out. The duplicate check
  and the settle read compared criteria exactly; they ignore case and
  runs of spaces, as Gmail's search does. `omitted_ids` is described per
  tool. A slim row could pass for a blank message; it carries
  `content_omitted`. The fake's list of filter writes is held equal to
  the client's by a test. The filter settle and the send settle share
  their scaffolding, `settleAfter`; the send's own reads are unchanged
  and never send. The driver's `call` comment sat on a constant and
  named only 403s.
- **Filters and search size, fourth code review: the listing design
  reverted, and seven filter findings fixed.** The lead reverted the
  whole-reply budget: emptying the content of rows past it changes what
  a released field holds, which `CLAUDE.md` makes a breaking change, for
  a size that only a page near `max` 100 reaches. Every row is full
  again, `budget_chars` bounds the text, and the descriptions advise a
  smaller `max`; `content_omitted` and the reply-cost options are gone.
  The text's "not shown" line and `omitted_ids` disagreed for changes;
  they name the same ids, and the line counts the rest apart. The loose
  criteria match folded `OR`, `AND` and `AROUND` to lower case, which
  changes a Gmail query; criteria are compared exactly again, and a
  sized filter's rounding is a known limit. A call canceled while
  waiting its turn or its backoff after an attempt that may have acted
  said "not sent"; it is `[ambiguous_outcome]`. Pacing reserved no
  slot, so two unlocked writers could send together; it reserves one.
  Units were charged and the timer started before the pacing wait, and
  the final attempt went unlogged; both are fixed, and a canceled wait
  charges nothing. A filter delete Google did not confirm is settled by
  reading, as a create is. A conflict with a filter that also keeps
  mail out of spam says so, and how to get the filter without it.
- **Filters and listings, code review at medium: one finding, fixed.**
  A call whose last attempt failed on its own returned that failure
  unchanged, though an earlier attempt may have acted; it is
  `[ambiguous_outcome]`, so a filter delete settles by reading.

- **Phase 6, the maintainer's check and `/code-review high`.**
  - Accept without the box ticked was refused, though the person meant
    to confirm. Accepting is now the confirmation: the form has no
    fields, which the specification allows (§18 row 64).
  - A call that failed as a JSON-RPC error after its question said
    "nothing was written", even when the retry had already written.
    The middleware now follows the call's stage: after the answer
    confirmed the write it is `[ambiguous_outcome]`, `written` or
    `unknown` (§4.13 item 10).
  - The 5-minute expiry refused a slow accept on protocols before
    2026-07-28, where the state never leaves the process. It applies
    only where the client carries the state.
  - A label receiving mail between the rounds could never be deleted,
    since the question bound its counts. They are shown and not bound;
    the label's id and name are.
  - The send and vacation questions did not show the body. They show
    its start, and bind a hash of all of it.
  - A draft edited between the rounds would be deleted on the old
    answer. The question binds the message the draft holds.
  - Asking repeats the reads, which the descriptions did not count:
    `delete_draft` said 30 units and charged 50. Each description of a
    tool that asks says how many more units, and a test holds it.
  - The recipient grouping was written twice; the argument hash was
    computed on calls that asked nothing; the test client of the live
    driver answered `ping` as an unknown method; "fulfils". Each fixed.
  - Security review, below its threshold: typographic and fullwidth
    quote marks now fold to a plain single quote, and `mailto:` and a
    bare domain followed by a path are broken, so no client links them.

- **Phase 6, security review of the review round: none at the bar; three
  low items closed.** Lookalike double quotes outside the folded set
  (modifier and Hebrew marks, ornament quotes, ditto marks) are folded
  too; a bare domain followed by a port, query or fragment, or written in
  a non-Latin script, is broken like one followed by a path; and blank
  characters no stripping removes, such as the Braille blank, become
  spaces, so a preview cannot be padded to steer where a client wraps it.
  The code review at medium found nothing.

- **2.0.1, code review at high: five fixed, two declined.**
  - The closing line named backticks, which a Markdown client does not
    draw around a code span. It now says "in backticks or code style",
    and so do the user documents.
  - Grave and acute lookalikes outside the folded set, such as U+00B4,
    U+02CA, U+02F4 and the Greek oxia and tonos, could seem to close a
    span where a client draws plain text. They fold to a plain single
    quote.
  - A value of invisible characters only was shown as the word `empty`.
    It is now "invisible characters only"; blank is still `empty`.
  - The test of `quoted` used its expected word `empty` as a sentinel,
    and the test of whole questions counted spans across all of them.
    Each case now states its whole output, a value literally named
    "empty" among them, and each question has its own span count.
  - Declined: doubling line breaks after rendering rather than where
    lines are written. `ask` is the one place every question passes,
    and the text it spaces is the text it binds; the test holds the
    layout. Declined: the version named in the status line and §16,
    which is this document's practice.
- **2.0.1, security review: none at the bar.** `/simplify` found the
  code clean and simplified the new test.

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
   work, not the model's. **Decided 2026-09-26 by the maintainer:
   registered by default, each call taking `confirm: true`** (§14).
2. **Should `get_thread` fold a thread's drafts in?** A draft reply
   appears inside its thread with the `DRAFT` label. Showing it helps a
   model see what is pending; hiding it keeps "what was said" apart from
   "what might be". Proposed: shown, marked as a draft, after the sent
   messages. **Decided 2026-09-27 by the maintainer: shown, in a
   section after the conversation** (§7.2, §14).
3. **go-licenses v1.6.0 or v2.** One sibling runs the v2 module; the
   rest call v1.6.0 current. **Decided in phase 0: v1.6.0.** It runs
   clean over this module's real graph on Go 1.27.1 (2026-09-25), and no
   dependency here carries MIT-0, which was the case for v2. Revisit when
   one does.
4. **Vacation responder writes.** `updateVacation` is deferred: an
   auto-reply writes to every sender, which is sending by another name.
   If built, it belongs behind `GMAIL_ENABLE_SEND` with the recipient
   scope (`restrictToContacts`, `restrictToDomain`) required, never
   defaulted. **Built in phase 5, decided 2026-09-27 by the maintainer**:
   `set_vacation` needs both flags, answers only contacts or the domain,
   and takes `confirm: true` (§7.9, §14).
5. **Filters.** `filters.create` could be built with `action.forward`
   refused structurally — the wire type would have no such field. The
   case for it is weak and the risk is §4.1's worst one. **Built in phase
   5, decided 2026-09-27 by the maintainer**: `create_filter` and
   `delete_filter` behind `GMAIL_ENABLE_SETTINGS`, with forward absent
   from the wire type and trash behind `confirm: true` (§7.9, §14).
6. **Google's hosted server.** §1. If it gains trash, local attachment
   transfer and bounded reads, this server's reason to exist narrows to
   the local-token and verified-release half. Revisit at each minor
   release; record the check in §18. **Open, standing.** Checked for
   1.0 on 2026-09-27 (§18 row 48): still in Developer Preview, eleven
   tools, and still no send, trash, delete or attachment download.
7. **Lenient argument decoding** (§3.14). Accepting `"[\"a\"]"` where
   an array is declared helps clients that stringify, and makes the
   schema a less exact contract. Proposed: accept JSON-in-a-string for
   arrays and integers only, and log that it happened. **Decided
   2026-09-27 by the maintainer: declined for 1.0** (§14, §18 row 47).
   The client defects still open do not reach this surface, and the
   schema stays an exact contract. Revisit if one that does is
   reported.

8. **Who counts as a participant for the recipient guard?** §4.2 counts
   every address on the thread's messages: `From`, `To`, `Cc` and
   `Reply-To`. The last three are written by each message's sender, so a
   correspondent in the thread can list other addresses in their own
   `Cc` and have a later reply-all reach them unconfirmed. Their own
   `From` already counts, so this widens reach rather than opening a new
   kind. Proposed by the phase 3 security review: count `From` on
   received messages, and `To` and `Cc` on the account's own sent
   messages only. **Decided 2026-09-27 by the maintainer: narrowed as
   proposed** (§14, §18 row 46). RFC 5322 §3.6.3 makes a received
   message's recipients the conventional reply-all audience, which is
   why the guard counted them; the guidance on agents reading untrusted
   mail says the opposite — an address someone else wrote is untrusted
   data, and it must not choose a send's recipients. The cost is one
   `confirm_recipients` entry per address a correspondent added.

### 17a. Deferred cleanups

None open. Written off:

- **Revoke the replaced refresh token at login.** Not buildable. Google
  revokes the grant, not one token, so revoking the replaced token would
  also end the one `login` had just stored (§18 row 53). The cap of 100
  per client is met only by repeated logins, and the runbook says so.

Phase 1 closed the header-block entry: a read's header block is capped
at half its budget, cut at a line, and the cut is stated.

### 17b. Deviations from the shared standard

The standard at `~/.claude/mcp-server-standard.md`, read 2026-09-24.

| The standard says | Here | Why |
|---|---|---|
| Errors use six classes | Twelve (§6.5) | `stale` and `ambiguous_outcome` are forced by §2.3 and §2.4; `rate_limited` must separate the sending limit from request limits; `forbidden` and `auth` ask for different fixes; `ambiguous` and `blocked` as the siblings use them |
| Never overwrite; compute a minimal diff | Held for labels (patch); **not holdable** for drafts | `drafts.update` is PUT-only and gmail/v1 has no ETag (§2.4). §4.4's witness narrows the lost-update window without closing it, and omitted fields are carried over rather than dropped |
| Destructive tools are not registered unless enabled | Held, and extended to sending | One sibling now registers deletes and refuses per call, and another retired its flag in favor of per-call guards. Neither fits here: the permanent-delete methods accept only a scope this server requests only when the flag is set, so a registered delete tool would fail every call by default; and a registered send tool is exactly the control an injected instruction gets to argue with |
| Never overwrite without a witness | Held for `send_draft`'s read; **not holdable** up to the send | `drafts.send` takes only a draft id and gmail/v1 has no ETag (§2.4). The witness is checked on the read just before the send, and a change landing between the two is what Gmail sends. The window is one round trip |
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
| 22 | The vendor's own Gmail MCP server offers send | Workspace MCP configuration guide | **Refuted.** Requests `gmail.readonly` and `gmail.compose`, ships drafts but no send, trash or delete. Supports §4.2's position. Whether it is labeled Developer Preview is **tier 3** |
| 23 | Sending limits are request quota | Gmail and Workspace help | **Refuted.** Separate daily and per-message recipient limits, surfaced as 429, possibly minutes late. §6.5 `rate_limited` separates them; spike H |
| 24 | Attachments may be read from any path the caller names | A surveyed server's README | **Rejected.** Only base names inside `GMAIL_LOCAL_DIR`. The exfiltration scenario is this design's inference, not a reported incident |
| 25 | Non-ASCII headers can be sent raw | RFC 5322 §2.2, RFC 2047; public 400s on accented display names | **Refuted.** Encoded-words, ≤75 characters each. §4.10; spike E |
| 26 | The credential order is keyring → file → env, as four sibling documents say | The sibling code | **Refuted (tier 2).** Every sibling's code resolves env → keyring → file; the documents describe which fallbacks exist, not precedence. §5a states the code's order |
| 27 | The newest sibling carries the newest shared machinery | Per-component comparison across all six siblings, 2026-09-24 | **Refuted.** Each sibling was newest for some pieces and stale for others. §5a merges them |
| 28 | A sibling's newest pin is upstream's newest | Upstream release listings, 2026-09-24 | **Refuted** for goreleaser, syft and codeql-action. §5a takes pins from upstream |
| 29 | A sibling's generated setup page is gated, as its source comments say | The sibling's gates and tests | **Refuted (tier 2).** Nothing read the page. §5a's `staleness` generates and compares it |
| 30 | The MCP Go SDK version can be a constant in the schema dump | Two siblings' constants against their `go.mod` | **Refuted.** Both constants were a version behind. §5a derives it from build info |
| 31 | The discovery document the design was verified against is current | `api-diff` on 2026-09-25 fetched revision 20260921; compared with 20260917 excluding `revision` and `etag` | **Confirmed.** No method, parameter, scope or schema changed. The committed snapshot is 20260921 |
| 32 | A smoke test can reach the newest protocol revision through `initialize` | MCP Go SDK v1.8.0 source, `negotiatedVersion`; the smoke gate against the built binary | **Refuted.** From 2026-07-28 the handshake is deprecated and the SDK caps `initialize` at 2025-11-25; the newest revision is reached through `server/discover`, which the smoke gate now probes and holds to the SDK's newest |
| 33 | `resultSizeEstimate` is a usable count | Live run, 2026-09-26: a search scoped to a label holding 3 messages, and a draft search matching 2 | **Refuted.** Gmail answered 201 for both. The text no longer states it; the structured field is described as an estimate that is often far off |
| 34 | A transcript of the driver's own mail carries nothing about the account | Live run, 2026-09-26, reading the transcript | **Refuted.** Gmail stamps inserted mail with `Received: from <number> named unknown by gmailapi.google.com`, and the number is the OAuth client's Cloud project. The transcript redactor now masks bare numbers of ten digits and more |
| 35 | `messages.insert` with `internalDateSource=receivedTime` records the time of the insert | Discovery document (the parameter's default and enum); spike F live, 2026-09-26 | **Refuted.** `internalDate` was the message's `Date` header, two days earlier, while `after:` matched the message by the time of the insert. So Gmail's search and `internalDate` can disagree for inserted or imported mail; for delivered mail they agree. The tools show `internalDate` |
| 36 | `filters.get` and `forwardingAddresses.get` add something their lists lack | Discovery document: each `get` returns the schema its `list` returns an array of, at the same unit cost | **Refuted.** Both written off in phase 1; `list_filters` and `get_settings` read the lists. The in-scope floor of `api-coverage` moved with them |
| 37 | Gmail allows user labels differing only in case, and user labels named like system labels | Spike I live, 2026-09-26 | **Refuted, both.** 409 "Label name exists or conflicts" for the second of `X-Case`/`X-case`; 400 "Invalid label name" for `INBOX` and `Inbox`. §6.2 |
| 38 | The transcript redactor masks every id the account has | Live run, 2026-09-26, reading the first phase 1 transcript | **Refuted.** History ids are short decimal counters, shaped like any number, and passed through in `list_changes`'s lines and the argument echo. The redactor now masks a number after `history`, `history from`, `history_id=`, `"history_id":` and `startHistoryId=`, and a test runs `list_changes`'s renderer through it so a reworded line fails before a run |
| 39 | `messages.batchModify` and `sendAs.get` add something their alternatives lack | Discovery document: `batchModify` returns an empty body; `sendAs.get` returns the schema `sendAs.list` returns an array of, at one unit each | **Refuted, both.** `batchModify` cannot report per item what §4.7 requires, and `messages.modify` per id, 5 units, answers with the labels after. `create_draft` reads `sendAs.list` on every call, which makes `get` redundant. Both written off; the in-scope floor of `api-coverage` moved with them |
| 40 | A reply field can take a message id or a thread id and tell which it was given | Gmail's threads guide; the fake's model; the live driver's thread-id spike, 2026-09-26 | **Refuted.** A thread's id is its first message's id, which three of three inserted threads showed live, so the same string names both, and the server could not know whether the caller meant that message or the thread's newest. `reply_to` takes a message id and `reply_to_thread` a thread id |
| 41 | Gmail keeps the `Message-ID` a client writes | Live runs, 2026-09-26: `get_draft` after `create_draft`, and spike D's sent original | **Refuted, for the sender.** `drafts.create` and `messages.send` both replaced it. `create_draft` no longer reports the one written, and the fake replaces it on create. Phase 3's settle-by-reading uses the id Gmail assigned (§4.3) |
| 42 | Gmail needs all three of §2.5's conditions to thread a reply | Spike D live, 2026-09-26, sender side; the maintainer reading the receiving Gmail mailbox, 2026-09-27 | **Split.** For the sender, `threadId` alone decided: replies without the headers, or with another subject, joined its thread. For the receiver, both of those stayed out of the thread. All three are needed between the two, as §4.5 writes them; the fake models all three, so a test catches a reply missing any |
| 43 | A login that does not force consent keeps the refresh token already issued | Spike J, the maintainer's login, 2026-09-27: every scope granted, no `prompt` in the authorization URL | **Refuted.** Google issued a new refresh token, and `login` stored it. Not forcing consent spares a screen, not a token; revoking the replaced one is written off by row 53 |
| 44 | An ambiguous send can be settled by searching `SENT` for the draft's `Message-ID` | Spikes B and C live, 2026-09-27: one draft sent through `send_draft`, its sent copy read back | **Refuted.** `drafts.send` replaced the `Message-ID` the draft held, and filed the sent copy under a new id, in the draft's thread; `drafts.get` then answered 404. §4.3 now records the thread's message ids before the send and rereads the thread, and the fake replaces the id on send as Gmail does |
| 45 | Gmail's per-user rate limit is a 429 with `Retry-After` | Spike H live, 2026-09-27: 32 workers reading the run's own message | **Refuted.** After 26,420 units it answered 403, reason `rateLimitExceeded`, "Units per minute per user", with no `Retry-After`. The client already classes that reason as a request-rate limit and backs off without a header; a test holds the shape, and that it is not read as the sending limit |
| 46 | Every address on a thread's messages is a participant the recipient guard may clear | RFC 5322 §3.6.3 (a reply's audience is the parent's `Reply-To` or `From`, and a copy often goes to its `To` and `Cc`); OWASP's AI Agent Security Cheat Sheet ("treat all external data as untrusted … emails", `send_email` a sensitive action); the 2025 paper *Design Patterns for Securing LLM Agents against Prompt Injections* (untrusted data must not choose a consequential action's arguments); MCP 2025-06-18 tools, security considerations (clients show inputs "to avoid malicious or accidental data exfiltration") — read 2026-09-27 | **Narrowed.** The RFC describes what a mail client offers a person; the other three describe what an agent may do with text a stranger wrote. The guard now clears the senders of the thread's messages and the addresses the account itself sent to, and asks for the rest (§4.2, §17.8) |
| 47 | Clients send arrays and integers as JSON strings, so the server should decode them leniently | MCP 2025-06-18 tools ("Servers MUST … validate all tool inputs"); RFC 9413 on the robustness principle; three public issues on a widely used MCP client's tracker, May to July 2026; `testdata/schema-baseline.json` — read 2026-09-27 | **Declined for 1.0.** The one issue that stringified every argument is closed. The two still open stringify only a parameter whose schema is empty, or an object declared through `$ref`/`$defs`, and this surface has neither: every parameter has a concrete type and no schema uses `$ref`. RFC 9413 describes how tolerating a peer's error entrenches it. Adding tolerance later is not breaking; removing it would be (§17.7) |
| 48 | Google's hosted Gmail MCP server now covers what this server is for (§17.6) | Google's MCP reference for `gmailmcp.googleapis.com`, last updated 2026-07-21, read 2026-09-27 | **Not yet.** Developer Preview; eleven tools — drafts, threads, messages, search and labels. No send, trash, delete or attachment download, and no stated read budget. §1's comparison stands for 1.0 |
| 49 | A client that allows an MCP server's tools by rule runs `send_draft` without asking | `claude -p` 2.1.282 with `--allowed-tools mcp__gmail__*`, the evals run of 2026-09-27 | **Refuted, as intended.** Every `send_draft` call, `dry_run: true` included, was refused with "MCPTool requires permission." before it reached the server, while the other writes ran. The client honors `anthropic/requiresUserInteraction` over an allow rule. So in that client a dry run does not spare the person a prompt: the hint is per tool, and cannot tell a preview from a send |
| 50 | `https://mail.google.com/` covers every Gmail method, the settings writes included | Discovery document revision 20260921, the `scopes` of `settings.filters.create`, `settings.filters.delete`, `settings.updateVacation` and `settings.sendAs.patch`, read 2026-09-27 | **Refuted.** Each lists only `gmail.settings.basic` (and `sendAs.patch` also `gmail.settings.sharing`). So `GMAIL_ENABLE_SETTINGS` adds that scope in every mode, the destructive one included, and the implication table does not let the full scope satisfy it; a test holds both. The quota page, read the same day, prices the filter and vacation writes at 5 units and does not list `sendAs.patch`, which takes `sendAs.update`'s 100 |
| 51 | `restrictToDomain` limits a personal Gmail account's vacation reply | Not checked: raised by the phase 5 security review, 2026-09-27. No reference page says what a personal account does with it, and the live driver runs on one account | **Refused until checked.** If Gmail accepted it and answered every sender, that is the audience §17.4 rules out, so `set_vacation` refuses `domain` for `gmail.com` and `googlemail.com` addresses. A live probe on a personal profile would settle it |
| 52 | Gmail refuses an identical filter, keeps a filter's size as given, and reads an empty HTML vacation body as absent | Phase 5's first live run, 2026-09-27 | **Refuted, all three.** A second filter identical to the first was created, not refused, so `create_filter`'s own check is the only guard against a duplicate. A size of 1,048,576 bytes was stored as 1,000,000, so a filter with a size may not match its own request and a duplicate of it can get through; the rounding rule is unknown and the check does not guess at it. A vacation reply sent with `responseBodyHtml: ""` beside a plain body came back with no body at all; the HTML body is now left out unless it holds something. The run restored the account's own reply after |
| 53 | `login` can revoke the refresh token it replaces and keep the new one | Google's OAuth 2.0 page for installed apps, "Revoking a token", read 2026-09-27: "Revocation removes all OAuth 2.0 scopes previously granted to a project, invalidating any issued access or refresh tokens for all clients registered under that project" | **Refuted, tier 3.** Revoking the old token ends the grant the new one belongs to, so `login` would sign itself out. §17a's cleanup is written off. `logout` and the runbook already rely on the same rule. Not probed live: the probe revokes the maintainer's grant and needs a new login |
| 54 | Gmail takes filter writes sent at once, as it takes them one at a time | Observed live 2026-09-28: about eight `create_filter` calls sent in parallel, several refused, the same calls sent one at a time taken. Spike L, the live driver, the same day, five runs: eight `filters.create` at once, each refused one again alone 5 seconds later, then the run's filters deleted back to back | **Refuted, intermittently.** One run answered 6 of 8 creates 500 `backendError`, "Internal error encountered.", which had created nothing; one answered all 8 that way, and of the 8 sent again one at a time, back to back, refused 2 with 400 `failedPrecondition`; two answered 3 of 8 with 400 `failedPrecondition`, "Precondition check failed.", and took each 5 seconds later, and one of those refused 3 of 8 deletes sent back to back, not at once, the same way; one took all 8. The server holds its settings changes to one at a time, paces filter writes 5 seconds apart, the one wait seen to work, and repeats that refusal on a filter write once, without calling it an overlap: the refusal does not say why (§7.9, §11). `sendAs.patch` and `updateVacation` were not probed, so they take the lock and neither the pacing nor the repeat |
| 55 | After a `create_filter` Google did not confirm, the caller can tell whether it was saved | Observed live 2026-09-28: a 500 "Internal error" answered `[ambiguous_outcome]`, and only a `list_filters` showed that nothing was saved | **Refuted.** The server now reads the list 5 seconds after such a create and gives a verdict, as §4.3 does for a send: `created` naming the new filter, `not_created`, or `unknown`. The create is never repeated. Row 54's 500s are the likeliest cause |
| 56 | A filter keeps the removed labels it was created with | Observed live 2026-09-28: archiving filters, including ones made in Gmail's settings page, later listed `SPAM` among their removed labels though no write named it; a filter read at once after its create did not, nor 15 seconds and one more filter write later. Gmail's filter guide, read 2026-09-28: `removeLabelIds=['SPAM']` is "Never mark as spam" | **Refuted, cause unknown.** Gmail can add `SPAM` to archiving filters after the fact, not at create time; what triggers it is not known. So `create_filter`'s duplicate check, and the read that settles an unconfirmed create, do not count a stored `SPAM` against a request that archives without it. A request that names `SPAM` is compared as given. The text says "never sends matching mail to spam" rather than naming a label taken off, and `never_spam` reports it. The live driver lists its own archiving filter again after another filter write and reports, without judging, whether `SPAM` appeared |
| 57 | `budget_chars` bounds a listing's whole reply | Observed live 2026-09-28: `search_messages` and `search_threads` with `max` 100 under a budget of 24,000 returned 70,000 to 106,000 characters, over the client's limit, with rows in `omitted_ids` that were also rows in `messages` and `threads`; the same page of 100 generated in `gmailtest` came to 102,000 and 127,000 | **Refuted, and kept.** It bounds the text; the structured rows carry every result in full, which is the released contract. Bounding them was tried on this branch, first by dropping rows and then by emptying their content, and reverted: both break clients that read the rows. `omitted_ids` no longer repeats a row the text shows, and the search and draft descriptions advise a smaller `max` to a client that limits one result's size (§4.8) |
| 58 | A server may ask the person to confirm a write through MCP elicitation, and the client's answer tells whether a person confirmed | The specification's client elicitation pages for 2025-06-18, 2025-11-25 and 2026-07-28 (<https://modelcontextprotocol.io/specification/2025-11-25/client/elicitation>), read 2026-09-28 | **Confirmed for asking; refuted for proof.** "Clients that support elicitation MUST declare the `elicitation` capability", and an empty one means form. Servers "MUST NOT use form mode elicitation to request sensitive information such as passwords, API keys, access tokens, or payment credentials"; a yes or no is none of those. Servers "SHOULD NOT include URLs intended to be clickable" in a form, and "MUST handle cases where the user declines or cancels the elicitation, or where the client fails to process the request". `accept` is "User explicitly approved and submitted with data", `decline` "User explicitly declined", `cancel` "User dismissed without making an explicit choice". But nothing says a person must answer: the multi-round-trip step reads "Client gathers the requested information from the user or other sources". §4.13 asks, and reads only an accept as confirmed |
| 59 | The Go SDK's `ServerSession.Elicit` works on every protocol this server serves | MCP Go SDK v1.8.0 source, `mcp/server.go` and `mcp/mrtr.go`; the multi-round-trip page of 2026-07-28 (<https://modelcontextprotocol.io/specification/2026-07-28/basic/patterns/mrtr>), read 2026-09-28 | **Refuted.** From 2026-07-28 `Elicit` returns an error: "return an InputRequests map instead". A handler that returns `InputRequests` with a `requestState` works on both: before 2026-07-28 the SDK's middleware asks with `elicitation/create` and calls the handler again in the same request. The SDK does not strip `inputResponses` a client sends on a first call. The page says servers "MUST treat `requestState` as an attacker-controlled input", "MUST protect its integrity (e.g. HMAC or AEAD)" when it influences business logic, and must enforce single use server-side. §4.13 signs, binds and spends it |
| 60 | A client that declares elicitation has a person to answer it | `claude -p` 2.1.284 against a throwaway probe server with one tool that asks, 2026-09-28; Claude Code's headless documentation (<https://code.claude.com/docs/en/headless>) | **Refuted.** On the default handshake it declared `"elicitation":{}` at 2025-11-25 and answered `cancel` about 5 ms after the question. With `MCP_PROTOCOL_NEGOTIATION=auto` it reached 2026-07-28, declared form and URL, retried with the echoed `requestState`, and answered `cancel` in about 15 ms; a permissive permission mode did not accept. The model saw only the tool's final text. The documentation says an elicitation "that no Elicitation hook answers is cancelled". So an unattended run declares the capability and cancels, which §4.13 refuses and §14 accepts |
| 61 | An `accept` comes from a person | Claude Code's MCP documentation (<https://code.claude.com/docs/en/mcp>) and changelog (<https://github.com/anthropics/claude-code/blob/main/CHANGELOG.md>, 2.1.76 and 2.1.284), read 2026-09-28 | **Refuted.** Claude Code "displays an interactive dialog and passes your response back to the server", and an `Elicitation` hook can "auto-respond to elicitation requests without showing a dialog"; an `ElicitationResult` hook can change the answer. The Agent SDK's `onElicitation`, left unset, declines. An accept may be a delegation the person configured, like an allow rule, and a decline may be a client's default: so a refusal says "not confirmed by the person", never "declined" (§4.13). The model cannot answer: the question is not a tool call, and `inputResponses` sits outside the arguments |
| 62 | Claude Desktop asks the person when a server elicits | Issues on Claude Code's tracker (<https://github.com/anthropics/claude-code/issues/96043>, <https://github.com/anthropics/claude-code/issues/89858>) and <https://github.com/anthropics/claude-ai-mcp/issues/153>, read 2026-09-28 | **Tier 3, not probed.** No primary source says the chat app supports it. Reports say its code tab declares no `elicitation` for local servers, that a hosted variant answers `decline` for a question never drawn, and that claude.ai does not support it. §4.13 treats Claude Desktop as a client that cannot ask, where `confirm` stays the guard unless `GMAIL_REQUIRE_PROMPT` is set. A probe of the bundle is owed with the rest of its install check |
| 63 | Other clients ask the person when a server elicits | VS Code 1.102 release notes (<https://code.visualstudio.com/updates/v1_102>), "includes support for elicitations"; Cursor 1.5 changelog (<https://cursor.com/changelog/1-5>), "Cursor now supports MCP elicitation"; <https://github.com/anthropics/claude-code/issues/79174>, closed, on an editor extension that declared the capability and declined every question — read 2026-09-28 | **Tier 3, not probed.** Support is announced; whether a person sees each question is not checked here. Either way a refusal is what an unanswered question gets |
| 64 | A form elicitation must ask for at least one field, so a confirmation needs a checkbox | The `ElicitRequestFormParams` type in the specification's `schema.ts` for 2025-06-18, 2025-11-25 and 2026-07-28, and the MCP Go SDK v1.8.0's `validateElicitSchema`, read 2026-09-28; the maintainer's check in Claude Code 2.1.284, protocol 2025-11-25, the same day | **Refuted.** `requestedSchema` is `type: "object"` with `properties: {[key: string]: PrimitiveSchemaDefinition}` and `required` optional: an open map with no minimum, so `properties: {}` is valid, and the SDK accepts it. The first build asked for one boolean as well. In the maintainer's check of `delete_draft`, Accept with the box unticked, Esc and Decline were each refused, answered `accept` unconfirmed, `cancel` and `decline`; ticked and Accept deleted the draft; the model never saw the question and did not call again on its own; the log carried no subject or address. A person pressed Accept meaning to confirm and was refused. So the form has no fields and the accept is the answer (§4.13). The second check, on the final build: Decline was refused and a plain Accept deleted the draft, and Claude Code drew the fieldless form with its three buttons |
| 65 | The Gmail API lists every inbox category Gmail shows | `labels.list`, observed live 2026-09-28; news reports of a "Purchases" category in Gmail from September 2025, not found on a Google page | **Refuted, observation only.** The list held the five classic `CATEGORY_*` labels — personal, social, promotions, updates, forums — while Gmail's interface showed a Purchases category. The server cannot see that category or filter by it. Nothing was changed |
| 66 | A client draws an elicitation question as plain text | VS Code `src/vs/workbench/contrib/mcp/browser/mcpElicitationService.ts` L100 and L173, and `src/vs/base/common/htmlContent.ts` L52-62, `main` at 251bcf5f, read 2026-09-29; the maintainer's check in Claude Code 2.1.284 the same day | **Refuted.** VS Code builds a form question as `new MarkdownString(elicitation.message)`, untrusted: command links are off, but emphasis, link text, code spans and HTML-like text draw, and single line breaks join into one paragraph. Only URL mode escapes the message, with `appendText`. 2.0.0 quoted mail text in double quotes, so a label named in Markdown's link syntax drew as link text, and one in double asterisks as the server's emphasis. Each quoted value is now a code span, which CommonMark draws literally, with backticks folded, and a blank line separates the lines. Backslash escaping was rejected: where a client draws plain text, the backslashes show inside addresses, the datum a send asks the person to check. The maintainer compared both in Claude Code and chose the code span |
| 67 | A required choice naming the outcome confirms better than an empty form | Codex `codex-rs/codex-mcp/src/elicitation.rs` L415-458 and L552-571, `main` at c248f6d4, and VS Code `mcpElicitationService.ts` L111-119 and L237-297, read 2026-09-29; the maintainer's check in Claude Code 2.1.284, protocol 2025-11-25, 2026-09-29, against a throwaway probe with three forms of one `delete_label` question | **Declined, for now.** For: Codex accepts a form with no properties by itself under approval policy `never` with full access, and a VS Code chat question the person skips resolves as `accept` with no content. A required choice survives both, since Codex then declines and an answer without the choice is refused. Against: in Claude Code the choice list, "Keep the label" first and no default, took the maintainer 60 seconds, against 8 for the empty form and 10 for a typed name, and they found it confusing. The empty form stays, and both client behaviors are recorded as limits (§4.13). Revisit if either client changes, or a client is shown to draw a choice list clearly |
