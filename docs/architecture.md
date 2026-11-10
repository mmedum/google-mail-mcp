# Architecture

How `google-mail-mcp` is put together, and the decisions a contributor
should not undo without a better reason than the one that put them
here. Part 1 is the overview. Part 2, from [Decisions](#decisions), is
one entry per decision, cited elsewhere by its D-ID. What was verified
about Gmail lives in the comment at the code that depends on it, with
the source linked and the live step that checks it named.

## Context

`google-mail-mcp` is a Model Context Protocol server for Gmail. One Go
binary runs as a subprocess of an MCP client and speaks MCP over stdio.
It holds one person's OAuth credentials and calls the Gmail API v1 over
raw REST, with wire types of its own (D-01, D-06). Nothing is hosted.
Each person brings their own Google Cloud project and Desktop OAuth
client, and nothing deployer-specific is built into the code, the
repository or a release (D-04).

The parties around the server:

- **The person** signs in once with `google-mail-mcp login` and owns the
  mailbox. Every call acts as them, with their own access.
- **The correspondents** wrote every subject, body, header and
  attachment the server reads. Some of them write to steer an assistant,
  so their text is data and never instructions (D-09).
- **The operator** decides what the server may do, through `GMAIL_*`
  settings the client passes at start: read-only mode, the send,
  destructive and settings switches, the local directory and the
  signature default (D-48, D-52, D-55, D-25, D-36). The operator is
  usually the person, setting up their own client.
- **The client** draws the approval prompt for each call from the tool
  annotations and its own rules. The server asks the person nothing
  itself (D-65).
- **The model** chooses the calls and their arguments. It can be wrong,
  or persuaded by mail somebody else wrote, so nothing it sends widens
  what the operator allowed (D-69, D-47).

The server talks to Google through:

- the Gmail API v1, for messages, threads, drafts, labels, history,
  attachments and settings;
- Google's OAuth endpoints, for login, refresh and revocation, and
  `tokeninfo`, for the scopes a token carries.

Nothing else is reached, and a credential goes only to Google's
origins (D-81).

It works inside one mailbox (D-08). An invitation is an attachment
here, read for the UID and times a calendar server finds its event by
(D-20). A linked Drive file is a URL. A name is never resolved to an
address (D-102).

It is not a push receiver (D-98), a migration tool (D-100) or an
administrator's tool (D-101), and it has no write over a query (D-99).

It exists so that mail is read without steering the assistant, so that
sending and permanent deletion stay absent unless the operator enables
them, and so that replies thread every time.

## Quality goals

In priority order:

1. **Mail never steers the assistant through the server.** Content is
   marked, hidden text is counted, nothing is fetched, and the writes
   that would redirect mail do not exist (D-09, D-10, D-11, D-12,
   D-13).
2. **Nothing reaches another person, or is lost for good, unless the
   operator enabled it.** Sending, permanent deletion and settings
   writes are unregistered by default, removal is trash, a send is never
   retried, and every recipient is named (D-48, D-49, D-50, D-51, D-52,
   D-55, D-64).
3. **A write changes only what was read and named.** Writes take ids,
   never a query; a draft is replaced under a witness; each item is
   reported on its own; every write takes `dry_run` (D-42, D-43, D-44,
   D-45, D-46, D-47).
4. **A read is bounded and says what it left out.** Every read has a
   character budget, and every omission has a continuation (D-14,
   D-15).
5. **The stream, the logs and the repository stay clean, and the
   surface is a contract.** Stdout carries frames only, logs carry no
   mail, the repository holds nothing real, and a released name or
   field keeps working (D-02, D-03, D-04, D-84).

## Codemap

- `cmd/google-mail-mcp` is the program. With no subcommand it serves;
  `login`, `logout`, `status`, `doctor`, `--dump-schemas` and
  `--version` are the rest. It names stdout and stderr once and passes
  them down (D-02). It imports `time/tzdata`, and prints errors through
  the redactor.
- `internal/app` is the startup assembly every entry point shares:
  configuration, credentials, the client and the server. It starts
  without credentials (D-75).
- `internal/auth` runs Google's desktop OAuth flow, a loopback redirect
  with PKCE and `state`, and holds the token source and revocation
  (D-71, D-72, D-76).
- `internal/config` reads the `GMAIL_*` environment and the bound
  flags, validates them once at start, refuses a removed setting, and
  keeps the registry of every variable the server reads (D-74).
- `internal/credentials` stores and finds the refresh token: the
  environment, then the OS keyring, then an owner-only file (D-71).
- `internal/fileperm` restricts a file to the account that wrote it,
  with a protected access list on Windows (D-71).
- `internal/gapi` is the raw REST client: the unit budget, the origin
  allow-list, retries by method, the context under which every write is
  refused, progress timeouts, streaming and the error classes (D-43,
  D-77, D-78, D-79, D-80, D-81).
- `internal/gapi/gmailtest` is the in-memory Gmail the tests and the
  evals run against, generated and never recorded, modeling what Gmail
  was seen to do (D-05).
- `internal/gmail` holds the wire types, written by hand for the fields
  used (D-06, D-07). A filter's write type has no forward field (D-56).
- `internal/mime` parses and builds RFC 5322 and MIME with no network:
  headers, addresses, charsets, HTML to text with hidden text counted,
  the made HTML version, signatures, quotes, forwards, draft edits as a
  tree, invitations and `List-Unsubscribe` (D-11, D-24, D-27 to D-38).
- `internal/model` is the server's view of a message, a thread, a
  draft, a label and a change, with the tables `service` and `render`
  derive from (D-17, D-83).
- `internal/redact` masks the account, addresses, paths and client ids
  in what `status`, `doctor` and errors print, and truncates ids (D-03).
- `internal/render` writes the readable half of every result, with the
  boundary blocks the server's own text is typed apart from, held by
  golden files (D-09, D-10, D-14).
- `internal/scopes` holds the scope set for each mode, which granted
  scope covers which, and what each tool needs. It generates the scope
  block of [gcp-setup.md](gcp-setup.md) (D-55, D-70, D-73).
- `internal/server` wires the MCP SDK: the instructions built from the
  configuration, the resources, the per-call log line and the schema
  dump (D-03, D-85, D-86).
- `internal/server/testutil` connects an in-memory client to the server
  in tests. Its client fails any call that asks the person (D-65).
- `internal/service` holds orchestration and policy: label resolution,
  budgets and paging, threading, the recipient guard, witnesses, settle
  reads and filter pacing. It is the only place a Google failure becomes
  a `[class]` tool error (D-23, D-39, D-47, D-49, D-50, D-58, D-59).
- `internal/tools` defines the MCP tools, each added through
  `register`, whose `Kind` decides the annotations, registration,
  `confirm`, the dry-run context and the rendered reply (D-63 to D-68).
- `internal/userconfig` stores non-secret per-profile state: the client
  secret's path, the token store and the granted scopes. It refuses a
  config directory outside the home directory.
- `internal/version` holds the version the build stamps in, with the
  module's build info as a fallback.

Beside the packages, `scripts/gates` is every check the repository runs
on itself, written in Go behind one registry (D-91, D-92).
`scripts/livemail` is the live driver (D-87, D-88, D-89).
`scripts/evals` scores a model driving the tools (D-90).
`scripts/internal` holds what the gates and drivers share: the stdio
client, the transcript writer, the redactor and the live-cover reader.
`packaging/mcpb` holds the Claude Desktop bundle's manifest (D-96,
D-97). `testdata` holds the released surface, the API snapshot and its
verdicts, the field verdicts, the live waivers and the renderer goldens
(D-07, D-84).

## Request flow

A tool call crosses the layers in one direction:

```
MCP client
   │  JSON-RPC over stdio; stdout carries nothing else
   ▼
internal/server     SDK session, tools, resources, one log line per call
   ▼
internal/tools      decode the input; register applies the Kind:
   │                annotations, gating, confirm, the dry-run context,
   │                both halves of the reply
   ▼
internal/service    labels, budgets, threading, guards, witnesses,
   │                settle reads, [class] errors
   ├──► internal/mime    parse and build, no network
   ├──► internal/model   the server's view of mail
   └──► internal/render  the readable half and its boundaries
   ▼
internal/gapi       one request: unit budget, origin allow-list, retry
   │                by method, no write under a dry run, classify
   ▼
internal/auth       refresh token → access token
   ▼
Gmail API v1, Google OAuth
```

### Start

`main` loads the configuration and refuses a bad one before it
announces itself: read-only mode with an enable switch, a removed
setting turned on, or a local directory that does not exist (D-70,
D-74). The server serves and lists its tools with no credentials; each
call then answers `[auth]` naming `login` (D-75). Startup and `doctor`
compare the granted scopes with the required ones and say what differs
(D-73). `tools.Register` adds every tool through `register`. A gated
tool is left out, and a kind that cannot be undone without `confirm`
panics at start, so the mistake fails in every configuration (D-63,
D-64, D-66).

### A read

A client calls `get_thread` on a long thread. The client spends its
units against the per-user budget, and the result says how many (D-79).

The service walks the whole MIME tree. The body is the plain part
unless it is a placeholder, else the HTML converted to text. Every
charset is decoded, and body parts Gmail stored behind an
`attachmentId` are fetched (D-27). Text a reader would not see is
removed and counted (D-11).

The thread renders newest first within the character budget. Quotes
and signatures collapse to a line, the drafts follow the conversation
in a section of their own, and a cursor names what did not fit (D-14,
D-18). Each message sits in a block whose boundary token is drawn per
call. The structured half carries the same text in `untrusted_*` fields
(D-09, D-82). The server's own notes are a separate type, so sender
text cannot reach them (D-10).

### A reply, then a send

A client calls `create_draft` with `reply_to`. The service reads the
parent and writes `In-Reply-To`, `References`, `threadId` and the
subject (D-39, D-40). The body goes as given, quoted-printable with each
paragraph on one line. An HTML version made from it goes beside it
unless `plain_only` asks otherwise (D-33, D-28, D-29, D-31). A
signature or a quote is added only when the call or the operator asks
(D-34, D-36). The result reads back whether Gmail filed the draft in the
parent's thread (D-46).

`send_draft` with `dry_run: true` reads the draft and its thread. It
names every recipient, the subject, the attachments and the thread the
send joins, and writes nothing (D-43).

`send_draft` for real carries the draft's `message_id`, `subject` and
`first_line`. Each is compared with what Gmail holds and refused as
`[stale]` when it differs (D-47). Every recipient must be in
`confirm_recipients`, at most 50. A repeated or unreadable recipient
header is `[blocked]` (D-49).

The send is one `drafts.send`, never retried (D-50, D-78). On an
ambiguous failure the service waits, reads the draft and its thread
again, and reports sent, not sent or unknown. It never sends again
(D-50). The tool exists only under `GMAIL_ENABLE_SEND`, and the client
prompts for it by its annotation (D-48, D-67).

### Login and refresh

`google-mail-mcp login` opens Google's consent page and listens on a
loopback port, with PKCE. `--no-browser` prints the URL and the
`ssh -L` line to forward it (D-71). Consent is forced only when the
scope set grows (D-72).

The refresh token goes to the OS keyring, or to an owner-only file with
a warning on every use. The environment overrides both (D-71). At serve
time the token source trades the refresh token for an access token. A
revoked or expired grant reaches the caller as `[auth]`, naming `login`.
An External client in Testing expires its token after seven days, which
`doctor` and [troubleshooting.md](troubleshooting.md) name (D-01).
`logout` revokes at Google, which ends the account's whole grant to the
project (D-76).

## Invariants

- Stdout carries only MCP JSON-RPC frames (D-02).
- No log line carries an address, a subject, a snippet, a body, a
  filename, a label name or a query (D-03).
- Nothing in the repository identifies a real mailbox, account or
  person (D-04).
- Mail content is returned only inside a boundary, and sender text
  never reaches the server's voice (D-09, D-10).
- Nothing a message references is fetched (D-12).
- No tool sets forwarding, delegation, a send-as identity or a filter
  that forwards (D-13, D-56).
- A send reaches only addresses named in `confirm_recipients`, and is
  never retried (D-49, D-50).
- The body is what the caller gave, plus only what the call or the
  operator named (D-33).
- A write names its ids, at most 100, and never takes a query (D-44).
- A draft is replaced only under the witness of the read that planned
  it (D-42).
- A dry run writes nothing (D-43).
- Sending, permanent deletion and settings writes are registered only
  under their switches, and the full-mail scope is asked for only with
  the destructive switch (D-48, D-52, D-55, D-64).
- Every write that cannot be undone takes `confirm` and repeats what it
  acts on (D-47, D-66).
- The server never asks the person a question (D-65).
- No tool changes the server's own settings (D-69).
- Every reply carries `structuredContent` and a text rendering (D-82).
- A read never silently returns less than it found (D-14).
- A released tool name, output field or resource URI changes only in an
  acknowledged major release (D-84).
- A default live run reads only its own mail and reaches nobody (D-87).

## Cross-cutting concerns

**Configuration.** Every setting is a `GMAIL_*` variable with a flag
bound to it. A flag overrides the environment, and the environment
overrides the default. Every reader goes through `config.Lookup`, which
drops the spaces around a value. Validation runs once at start, and a
removed setting is refused with its replacement (D-74). The full list
is in [configuration.md](configuration.md).

**Untrusted content.** Mail is shown inside a block whose boundary is
drawn per call, and the structured half names the same text
`untrusted_*` (D-09). Hidden text is removed and counted, and nothing a
message references is fetched (D-11, D-12). The server's own notes are
written from typed parts, so a sender cannot write one (D-10).
[security.md](security.md) holds the threat model.

**Errors.** Every tool error is `[class] message`, and the class tells
the caller what to do next. The vocabulary is closed and declared in
`internal/gapi/errors.go`, where each class's comment says what it
means and what the caller should do (D-77).

**Quota and retries.** Gmail meters units per user per minute, and each
method has its cost. The client spends against that budget, waits
rather than earning a refusal, and every result reports the units it
spent (D-79). A read retries a transient failure. A POST is retried
only when Google turned it away before acting, and the sending limit is
never retried (D-78).

**Logging.** `slog` to stderr, as text or JSON. A line names the method,
the tool, the outcome, the milliseconds, the units and a truncated id,
and nothing from the mailbox. Transport errors are stripped of their
URL before they are logged, since a search travels in the query string
(D-03).

**Scopes.** Read-only mode asks for `gmail.readonly` and the default
for `gmail.modify`. The settings switch adds `gmail.settings.basic`, and
only the destructive switch asks for full mail access. A changed set
needs a new `login` (D-48, D-52, D-55, D-70, D-73). The list to paste
into the consent screen is in [gcp-setup.md](gcp-setup.md).

**Approval.** Registration, `confirm`, the witnesses, the guards and
`dry_run` hold on every client. The annotations and Claude Code's
`requiresUserInteraction` mark shape the client's prompt and hold
nothing on their own (D-63 to D-68).

**Testing.** The unit suite runs against `internal/gapi/gmailtest`,
which encodes what Gmail was seen to do (D-05). Fuzz targets hold the
MIME round trips, and golden files hold the renderers (D-27).
`TestLogsNeverCarryThePayload` drives every tool with canaries (D-03).
`make live` drives the shipped binary against a real account and is
the only check that can show a belief wrong, and `make live-cover`
fails on a tool option no live step drives (D-89). Evals score planted
instructions (D-90). How to run a live check is in
[CONTRIBUTING.md](../CONTRIBUTING.md#the-live-run).

**Documentation.** A decision lives once, in part 2 here. What was
verified about Gmail lives in the comment at the line that depends on
it: the behavior, the source link, where the source is wrong or silent,
and the live step that checks it. A refuted belief goes at that line or
in a decision's `Rejected:` line. The docs gate holds every D-ID
citation to an entry, every "Held by" cell to a gate or test that
exists, and the codemap to the tree. The staleness gate holds the
README, [configuration.md](configuration.md) and
[gcp-setup.md](gcp-setup.md) to the code.

## Decisions

| ID | Decision | Status | Held by |
| --- | --- | --- | --- |
| D-01 | One stdio binary per person, on their own OAuth client | accepted | smoke, TestServeSignedOutAndDisconnect, review |
| D-02 | Stdout carries only MCP JSON-RPC frames | accepted | forbidigo, smoke, TestDumpSchemas |
| D-03 | Logs record the call, never the payload | accepted | TestLogsNeverCarryThePayload, TestLogLineCarriesTheCallButNotItsPayload, TestTransportErrorsCarryNoURL, TestTheSDKLoggerOnlyAtDebug, TestInspectDoesNotLeakTheTokenOnATransportError |
| D-04 | Nothing in the repository identifies a real mailbox, account or person | accepted | leaks, leaks-history, secrets, TestLeaksRulesCatchPlantedShapes, TestLeaksAllowListHasReasons, TestLeaksHistoryReadsBlobsMessagesAndTags, TestLeaksTreeRefusesABinaryAndAShortScan |
| D-05 | Fixtures are generated, and the fake models what Gmail was seen to do | accepted | TestIDsAreGeneratedShape, TestScenarioContent, TestDraftsThreadByIDOrAsDocumented, TestFakeAndClientAgreeOnFilterWrites |
| D-06 | Own wire types over raw REST, and no code imported from another project | accepted | depguard, api-fields, TestTagsMatchDiscovery |
| D-07 | Every API method and field has a written verdict | accepted | api-coverage, api-fields, TestAPICoverageCommittedRecord, TestAPIDiffClosedPortWritesNothing |
| D-08 | The server works inside one mailbox | accepted | review |
| D-09 | Mail content is marked as data, inside a per-call boundary | accepted | TestInjectedTextStaysInsideItsBlock, TestTokenRedrawnWhenContentHoldsIt, TestUntrustedFieldsAreNamed, TestSearchThreadsMarksMailAsUntrusted |
| D-10 | Sender text never reaches the server's voice | accepted | TestSenderTextNeverReachesTheServersVoice, TestServerVoiceIsTyped, TestInstructionsNameOnlyRegisteredTools |
| D-11 | Text a reader would not see is removed and counted | accepted | TestHTMLToTextHidden, TestGetThreadCountsHiddenText, TestGetSettingsCountsHiddenText, TestInvisibleCharactersInAPartsTypeAreRemoved, FuzzHTMLToText |
| D-12 | Nothing a message references is fetched | accepted | TestHTMLLinks, TestUnsubscribeOnRowsAndReads, review |
| D-13 | The writes that redirect mail do not exist | accepted | TestAFilterCannotForward, TestListFiltersFlagsForwarding, api-coverage |
| D-14 | Every read has a character budget and names what it left out | accepted | TestBudgetIsHeld, TestCutBody, TestGetThreadContinuesWithCursor, TestNotesStayWithinTheBudget, TestAHeaderBlockLargerThanTheBudgetIsCut, TestEmptyPageWithTokenIsNotComplete |
| D-15 | A listing's budget bounds its text, never its structured rows | accepted | TestSearchKeepsEveryRowInFull, TestRowsOmittedCountsEveryRow, TestOmittedIDsIsDescribedPerTool, TestListingThatFitsExactlyIsWhole, TestListFiltersShowsForwardingFirst |
| D-16 | `q` passes through verbatim, with `after` and `before` as instants beside it | accepted | TestSearchResolvesLabelNamesAndAppendsEpochBounds, TestParseZone, TestInvalidSearchSendsNothing |
| D-17 | A thread row names its newest message that was sent or received | accepted | TestSearchThreadsRowPassesOverDraftsAndTrash, TestThreadLatestPassesOverWhatWasNotSaid |
| D-18 | `get_thread` shows a thread's drafts after the conversation | accepted | TestThreadDraftsFollowTheConversation |
| D-19 | `list_changes` reports an expired cursor, never an empty list | accepted | TestListChangesSaysACursorExpired, TestAnExpiredCursorIsReportedNotEmpty, TestALabelDeletedMidCallIsNotAnExpiredCursor |
| D-20 | An invitation is read for the UID and times a calendar server needs | accepted | TestParseInvitation, FuzzParseInvitation, FuzzInvitationRoundTrip, TestInvitationOnAttachments, TestAnInvitationFileIsReadBesideItsInlineCopy |
| D-21 | Mail is addressed by Gmail's ids, and a draft result names both of its ids | accepted | TestDrafts, review |
| D-22 | An `rfc822:` id is one Message-ID, never a search | accepted | TestRFC822TakesOneMessageID, TestRFC822WithASearchTermIsRefused, TestMessageByRFC822ID |
| D-23 | A label resolves by id or exact name, read on every call | accepted | TestSearchResolvesLabelNamesAndAppendsEpochBounds, TestLabelIndex, review |
| D-24 | Addresses are strict going out, lenient coming in, and shown so they read back | accepted | FuzzAddressStringReadsBack, FuzzLenientAddressIsOutsideQuotes, TestALenientFromShowsTheAddressOutsideTheName, TestARecipientShownCanBeGivenBack |
| D-25 | Files move only through `GMAIL_LOCAL_DIR`, never over an existing file | accepted | TestDownloadsAreRegisteredOnlyWithALocalDir, TestDownloadAttachmentWritesIntoTheLocalDir, TestDownloadAttachmentKeepsAnUnsafeNameInTheDirectory, TestCreateDraftAttachments, TestANamedPipeIsNotAttached |
| D-26 | `read_attachment` reads text types inside the boundaries and the budget | accepted | TestReadAttachmentReadsTextTypes, TestReadAttachmentConvertsHTML, TestReadAttachmentReadsAnAttachedMessage, TestReadAttachmentRefusals |
| D-27 | MIME is the server's own, and what it builds parses back the same | accepted | FuzzBuildRoundTrip, FuzzEditRaw, FuzzParseRaw, TestBuildRoundTripsFourScripts, live |
| D-28 | A paragraph goes out as one line, never wrapped to a width | accepted | TestBuildSendsAParagraphAsOneLine |
| D-29 | A draft without `body_html` carries an HTML version made from its text | accepted | TestADraftCarriesHTMLMadeFromItsBody, TestHTMLFromText, live |
| D-30 | The made HTML is recognized by reading it back, and a draft keeps its shape | accepted | TestEditRawKeepsTheHTMLMadeFromTheText, FuzzMadeHTMLIsRecognized, TestPlainOnlyKeepsHTMLWrittenByHand |
| D-31 | `plain_only` keeps a draft plain on request | accepted | TestPlainOnlyKeepsHTMLWrittenByHand |
| D-32 | A web address in the made HTML is a link whose text is the address | accepted | TestLineHTML, FuzzLineHTML |
| D-33 | The body is exactly what the caller gave, and the server adds only what is named | accepted | TestBuildPlainASCIIIsExact, live, review |
| D-34 | `quote` puts the parent's text, as a read shows it, below the body | accepted | TestCreateDraftQuotes, TestCreateDraftQuotesHTMLAsConverted, TestCreateDraftQuoteReadsTextStoredApart |
| D-35 | `forward` attaches the original as Gmail stored it, its `Bcc` left out | accepted | TestCreateDraftForwards, TestCreateDraftForwardLeavesOutBcc, FuzzForwardCopyLeavesOutBcc, TestAnAttachedMessageGoesAsWritten, TestCreateDraftForwardInANewThread |
| D-36 | A signature is added only when the call or the operator asks | accepted | TestCreateDraftSignsWhenAsked, TestTheServerSignsNewDraftsWhenSetTo, TestDraftSignatureIsASwitchThatStartsOff, live |
| D-37 | A signature goes in the composer's markup, its images as parts | accepted | TestCreateDraftSignsAboveTheQuote, TestTheSignaturesImageIsAPartOfItsOwn, TestBodiesMoveImagesIntoParts |
| D-38 | A draft never carries two signatures, and a new body keeps its own | accepted | TestUpdateDraftCarriesTheSignature, TestTheSignatureStaysInTheDraft, TestUpdateDraftWillNotDropASignatureUnsaid, FuzzSignatureIsCarried |
| D-39 | A reply is threaded by the server from its parent | accepted | TestCreateDraftReplyThreadingHeaders, TestAReplyKeepsItsThreadingHeaders, live |
| D-40 | `reply_to` takes a message id and `reply_to_thread` a thread id | accepted | TestCreateDraftReplies |
| D-41 | `reply_all` expands from the parent and names where each recipient came from | accepted | TestCreateDraftReplies, review |
| D-42 | A write never replaces what it did not read | accepted | TestUpdateDraft, TestEditRawWithNothingIsTheSameBytes, FuzzEditRaw, TestADraftUpdateReplacesItsMessage, TestCreateAndUpdateLabel |
| D-43 | Every write takes `dry_run`, and a dry run cannot write | accepted | TestDryRunEntersTheNoWritesContext, TestWithoutWritesRefusesEveryWriteBeforeTheNetwork, TestDryRunPredictsLabelsAfter, outcomes |
| D-44 | Writes take explicit ids, at most 100, never a query | accepted | TestModifyLabels, review |
| D-45 | Each item of a multi-id write is read, written only if needed, and reported alone | accepted | TestModifyLabels, TestTrashAndRestore, TestDeletePermanently, outcomes |
| D-46 | Results say what changed, read back from Gmail | accepted | outcomes, TestOutcomesPassesHonestBranches, TestCreateDraftReplies |
| D-47 | A call that cannot be undone repeats what it acts on | accepted | TestDeleteDraftNeedsItsWitness, TestSendDraftNeedsItsSubjectAndFirstLine, TestSendDraftReadsTheFirstLineWhole, TestDeleteFilterNeedsItsCriteria |
| D-48 | Sending exists only with `GMAIL_ENABLE_SEND`, and only through a draft | accepted | TestGatedToolsRegisterOnlyWithTheirFlag, TestRegistrationGates, TestModesSendChangesNothing |
| D-49 | Every recipient of a send is named in `confirm_recipients` | accepted | TestSendDraftRecipientGuard, TestSendDraftCountsOnlyWhatTheAccountChose, TestSendDraftRefusesRecipientsItCannotReadWhole |
| D-50 | A send is never retried, and an ambiguous one is settled by reading | accepted | TestASendIsNeverRepeated, TestAPostIsNotRepeatedAfterAnAmbiguousFailure, TestSendDraftAmbiguousOutcome, TestSendDraftUnavailableIsSettled, live |
| D-51 | Removal is trash, an ordinary write | accepted | TestTrashAndRestore |
| D-52 | Permanent deletion exists only with the flag that requests full mail access | accepted | TestGatedToolsRegisterOnlyWithTheirFlag, TestDeletePermanentlyNeedsTheFullScope, TestForMode, live |
| D-53 | `delete_draft` is a default write that takes `confirm` and its witness | accepted | TestDeleteDraftNeedsItsWitness, TestDeleteDraft, TestAKindThatCannotBeUndoneTakesConfirm |
| D-54 | Archiving, marking read and starring are label changes, not tools | accepted | TestModifyLabels, review |
| D-55 | Settings writes exist only with `GMAIL_ENABLE_SETTINGS`, which adds their one scope | accepted | TestSettingsToolsRegisterOnlyWithTheirFlags, TestForMode, TestSettingsDescriptionsFollowTheFlags |
| D-56 | A filter cannot forward | accepted | TestAFilterCannotForward |
| D-57 | A risky variant is its own tool | accepted | TestCreateTrashFilter, TestRegistrationGates |
| D-58 | A filter's duplicate check and an unconfirmed write are settled by reading the list | accepted | TestCreateFilterChecksItsCriteria, TestAmbiguousFilterCreateIsSettledByReading, TestAmbiguousFilterDeleteIsSettledByReading |
| D-59 | Settings changes run one at a time, and filter writes are paced | accepted | TestParallelFilterWritesDoNotOverlap, TestFilterWritesArePaced, TestPacingReservesTheSlot, TestGenericPreconditionOnFilterWriteIsRepeatedOnce, TestStoppedFilterWrites |
| D-60 | The vacation reply needs the send flag, a bounded audience and `confirm` | accepted | TestSetVacation, TestSettingsToolsRegisterOnlyWithTheirFlags |
| D-61 | `update_signature` writes plain text lines, never caller markup | accepted | TestUpdateSignature, TestUpdateSignatureKeepsSpacing, TestUpdateSignatureSaysWhetherASignatureWithNoTextIsSet |
| D-62 | `get_settings` leads with forwarding and says what is empty | accepted | TestGetSettingsLeadsWithForwarding, TestGetSettingsSaysWhatIsEmpty, TestGetSettingsShowsAReplyOfImagesAlone |
| D-63 | Every tool goes through one `register`, whose `Kind` decides the rest | accepted | TestAnnotationsAndMetaComeFromKind, TestRegistrationGates, TestSurfaceByMode, TestSmokeShapeFollowsRegister, TestSmokeModesCoverEveryKind |
| D-64 | A gated tool is unregistered, never registered and refusing | accepted | TestGatedToolsRegisterOnlyWithTheirFlag, TestSettingsToolsRegisterOnlyWithTheirFlags, TestDownloadsAreRegisteredOnlyWithALocalDir, smoke |
| D-65 | The server asks the person nothing; per-call approval is the client's | accepted | forbidigo, TestHarness, TestElicitationIsDeclaredAndDeclined, review |
| D-66 | `confirm` is a witness on every write that cannot be undone, never approval | accepted | TestAKindThatCannotBeUndoneTakesConfirm, TestAToolThatTakesConfirmIsAnnotatedDestructive |
| D-67 | `destructiveHint` is true on every kind but Read, Read (local write) and Adding | accepted | TestAnnotationsAndMetaComeFromKind, TestDumpDestructive, TestStalenessMustAsk, TestStalenessReadmePermissionRules |
| D-68 | Only `delete_permanently` and `delete_label` carry the mark | accepted | TestOnlyTheWidestDeletesCarryTheMark |
| D-69 | No tool widens the server's own reach | accepted | TestNoToolWritesTheServersOwnSettings |
| D-70 | Read-only mode registers the reads and asks for `gmail.readonly` | accepted | TestReadOnlyWithAnEnableFlagIsRefused, TestSurfaceByMode, TestForMode |
| D-71 | Login is a loopback flow with PKCE, and the token lives in the OS keyring | accepted | TestLoginUsesLoopbackLiteralAndPKCE, TestLoginRejectsAStateMismatch, TestEnvironmentWins, TestFileFallbackWarnsEveryTime, TestRestrictToOwnerSetsAProtectedACL, TestBaseDirRefusesOutsideHome |
| D-72 | Consent is forced only when the scope set grows | accepted | TestConsentOnlyWhenScopesGrow, TestNoRefreshTokenWithoutConsentKeepsTheStoredOne |
| D-73 | Scopes follow the flags, and a granted set that differs is named | accepted | TestScopeWarning, TestMissingAndCovered, TestExcess, TestForMode, TestStalenessScopeBlock |
| D-74 | Every setting is read through one reader, and a removed one is refused | accepted | TestASettingOfSpacesIsUnset, TestEveryVariableTheCodeNamesIsInEnvVars, TestTheNameScanReadsEveryChainInOrder, TestARemovedSettingIsRefusedUnlessOff, TestEveryProblemIsReported |
| D-75 | The server starts and lists its tools without credentials | accepted | TestServeSignedOutAndDisconnect, TestAssembleWithoutCredentialsStillServes, TestDoctorAndLoginSignedOut, smoke |
| D-76 | `logout` revokes at Google, which ends the whole grant | accepted | TestLoginStatusDoctorLogout, TestLogoutLeavesAnEnvironmentTokenAlone, TestInspectAndRevoke |
| D-77 | Every error is `[class] message` from a closed vocabulary | accepted | classes, TestClassesClean, TestErrorsAreClassedToolErrors, TestStatusClasses |
| D-78 | Repeatability comes from the HTTP method, and a POST is retried only when turned away | accepted | TestAPostIsRepeatedWhenTurnedAway, TestADeclaredRepeatablePostRetriesLikeARead, TestRetriesStopAtFourAttempts, TestRetryAfterIsAFloor, TestTheSendingLimitIsNotRetriedAndSaysSo, TestThePerUserUnitLimitIsRetried |
| D-79 | Quota is spent in units against a per-user budget, and each result reports it | accepted | TestTheUnitBudgetWaitsAndCanBeCanceled, TestTheUnitBudgetFollowsUnitsPerMinute, TestUnits, TestConcurrentCallsShareTheBudgetAndCounter, TestSearchThreadsDefaultsToTwentyAndReadsHeadersOnly |
| D-80 | Every request is bounded by progress and by size | accepted | TestResponsesAreBounded, TestAStreamOutlastsTheTimeoutWhileItProgresses, TestAnUploadIsBoundedByProgressNotTotal, TestStreamDataStopsAtTheCap, TestDownloadAttachmentRefusesAnOversizeAttachmentBeforeReadingIt |
| D-81 | A credential goes only to Google's origins, checked on every redirect | accepted | TestARedirectOffTheAllowlistIsRefused, TestARedirectWithinTheAllowlistIsFollowed, TestAnInjectedHTTPClientKeepsTheAllowlist, TestOriginKeyAndAllowlist |
| D-82 | A reply carries both halves, never the same bytes twice | accepted | TestAReplyCarriesBothHalves |
| D-83 | An unknown value is left out, never sent as a zero | accepted | TestAnUnknownDateIsLeftOut, TestAnInternalDateOfZeroIsNoDate |
| D-84 | The released tool surface is a contract held by a committed baseline | accepted | schema-diff, schema-ack, TestSchemaDiffFailsEachBreakingChange, TestSchemaDiffBaselineMustBeTheNewestRelease, TestSchemaAckNeedsEveryItemNamed |
| D-85 | Instructions and descriptions are built from the configuration | accepted | TestInstructionsNameOnlyRegisteredTools, TestInstructionsDescribeWritingOnlyWhereItExists, TestSettingsDescriptionsFollowTheFlags, TestTheDumpCarriesEverySentenceOfTheInstructions |
| D-86 | Resources carry the same text as their tools | accepted | TestResources, smoke |
| D-87 | The live driver reads only what it wrote and reaches nobody by default | accepted | TestTheGuardRefusesReadsOutsideTheRun, TestTheGuardHoldsWritesToTheRun, TestTheGuardHoldsSendsToSendTo, TestSendingSpikesNeedASecondAddress, TestTheSettingsAreKeptBeforeAnythingChanges, TestASignatureGmailWillNotTakeBackIsNeverChanged |
| D-88 | The live transcript goes through one redacting writer | accepted | transcript, TestEveryLinePrintedIsRedacted, TestTheCloudProjectNumberInAReceivedHeaderIsMasked, TestHistoryIDsAreMasked |
| D-89 | A write-path change counts only after a live run whose transcript is read | accepted | live-cover, TestLiveCoverReadsTheRealDriver, live |
| D-90 | Evals score the calls and the mailbox, with planted instructions | accepted | evals-check, TestScoreCatchesAnInjectionByToolAndByMarker, TestSelfCheckPasses, TestTheFenceIsOnTheCommandLine |
| D-91 | A rule is made to fail: a test, a list derived from the code, a floor | accepted | TestRegistryFloor, TestClassesFloors, TestAPICoverageFloors, TestSchemaDiffFloor, review |
| D-92 | `make check` and CI run the same set of gates | accepted | parity, checklist, TestParityRefuses, TestParityHoldsTheTreesTagsToAVetPass, TestParityHoldsCheckToWhatAPushRuns, TestParityReadsEveryRuleOfATarget, TestShellSwallowed, TestChecklist, TestEveryCommandDeclaresWhereItRuns |
| D-93 | A release is reproducible, signed, attested, and refused unless CI is green | accepted | release, goreleaser-check, TestReleaseRefuses, TestReleaseReadsTheJobsPermissions, live |
| D-94 | The registry entry's hash is read only after the checksum signature verifies | accepted | server-json, TestServerJSONCheckRefusesTheWorkflow, TestServerJSONPublish |
| D-95 | Every tool is pinned to one exact version everywhere it is named | accepted | pins, TestPinsRefuses, TestPinsNamesEachProblem, TestPinsPassesOnAPinnedRepository, TestPinsReadsAnInputInAnyCase, TestPinsPassesALocalCallsContext, TestReadWorkflowRefusesAStrayLineBreak |
| D-96 | The bundle defaults every switch off and passes settings through `env` alone | accepted | mcpb, TestMcpbRefusalsTurnASwitchOnWhateverItsDefault, TestMcpbAsksForEverySettingNotLeftOut, TestMcpbRefusesEachBreak |
| D-97 | The bundle manifest is held to its vendored schema and read once | accepted | mcpb, TestMcpbRefusesARepeatedKey, TestVendorManifestSchemaChosenByVersion, TestMcpbStampIsHeldToTheSchema, TestMcpbPackRunsTheRepositoryChecks |
| D-98 | `watch` pushes changes through Cloud Pub/Sub | rejected | — |
| D-99 | A write acts on every result of a query | rejected | — |
| D-100 | The server imports or inserts mail | rejected | — |
| D-101 | The server administers a Workspace through service accounts and delegation | rejected | — |
| D-102 | A tool resolves a name to an address | rejected | — |
| D-103 | `send_message` sends without a draft | rejected | — |
| D-104 | The server confirms a write by asking the person through elicitation | rejected | — |
| D-105 | Arguments a client stringified are decoded leniently | rejected | — |
| D-106 | Plain text goes out as `format=flowed` | rejected | — |
| D-107 | `login` revokes the refresh token it replaces | rejected | — |
| D-108 | `Kind` splits into an effect and a gate | rejected | — |

### D-01 One stdio binary per person, on their own OAuth client

The server is one Go binary that runs as a subprocess of the MCP client
and speaks MCP over stdio. It holds one person's OAuth credentials and
calls the Gmail API directly, as that person. Nothing is hosted, and
there is no shared deployment.

Each person creates their own Google Cloud project and a Desktop OAuth
client. Nothing deployer-specific is built into the code, the
repository or a release, so a copy of the binary carries no one's
credentials.

The reason is reach. A mailbox holds the person's mail and other
people's words, and per-user OAuth keeps every call inside the person's
own access. Every Gmail scope that reads content is restricted
([scopes](https://developers.google.com/workspace/gmail/api/auth/scopes)),
so a person's own client, Internal to their organization or External in
Testing, signs in without the verification and security assessment a
published app with restricted scopes needs.

Rejected: a hosted deployment, which this repository has nothing to
run; an OAuth client shipped with the release, which would put one
deployer's credentials in every copy.
Cost: an External client in Testing gets refresh tokens that expire
after seven days
([OAuth 2.0](https://developers.google.com/identity/protocols/oauth2#expiration)).
`doctor` names that expiry when a refresh fails, and
[troubleshooting.md](troubleshooting.md) explains it.

### D-02 Stdout carries only MCP JSON-RPC frames

MCP's stdio transport says the server "MUST NOT write anything to its
`stdout` that is not a valid MCP message", and "MAY write UTF-8 strings
to its standard error (`stderr`) for logging purposes"
([transports](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports)).
A stray print corrupts the JSON-RPC stream, and the client silently
stops working.

So logs go to stderr through `slog`. `main` names the process's streams
once and passes them down as `io.Writer`. forbidigo refuses
`fmt.Print*` and `os.Stdout` everywhere else, with `cmd/` and
`scripts/` excluded. Its rules carry their own messages, so a settings
block that fails to load shows up as forbidigo's default message. The
smoke gate drives the shipped binary over stdio and fails on any line
that is not a frame. `--dump-schemas` writes JSON to stdout because it
does not serve.

Rejected: a print to stdout anywhere but `main`, even behind a debug
setting, since one stray line breaks the stream at any level.
Cost: only `main` names the process's streams; everything else takes an
`io.Writer`.

### D-03 Logs record the call, never the payload

A call line carries the method, the tool, the outcome, the
milliseconds, the units spent and an id truncated to its first six
characters. It never carries an address, a subject, a snippet, a body,
a filename, a label name or a search query.

A mailbox is other people's words and addresses. A log somebody pastes
into a bug report has to be safe by construction rather than by their
care.

The subtle leak is the request URL. A search puts `q` in the query
string, and a transport failure renders the whole URL, so `gapi`
strips the URL from transport errors before anything logs them. The
token check in `internal/auth` does the same for the token in its
query. The SDK's own logger runs only at debug.

`TestLogsNeverCarryThePayload` drives every registered tool with
canaries in every string argument, at debug, and fails if one appears
in the log or if the log is empty.

Rejected: payloads at debug level, since a debug log is still the file
a person pastes into a bug report.
Cost: a transport error's text, which names the URL, is replaced with
the kind of failure it was, and a failure is diagnosed from the method,
the tool, the outcome and the class alone.

### D-04 Nothing in the repository identifies a real mailbox, account or person

The repository is public, and the server is built by running it against
a real mailbox. Nothing deployer-specific lands here: no addresses, no
names, no message, thread, label or filter ids, no Cloud project or
OAuth client ids, and no subject, body or signature from real mail.
That holds for code, docs, fixtures, goldens, transcripts, commit and
tag messages, pull requests and logs.

A mailbox is the worst payload there is. Every message carries third
parties' addresses, words they wrote to someone else, and a signature
with their phone number and employer.

The leak gate is an allow-list anchored on shapes the server's
generated fields cannot take. It flags an `@` before a dotted domain
outside the example and invalid set, a Gmail web URL, a keyword such as
`message` before a hex id outside the fixture range, and the Google
OAuth shapes. `make leaks-history` reads every blob, commit message and tag.
A tracked binary is a finding. gitleaks covers credentials in the tree
and in HEAD's history, through `make secrets`.

Rejected: a credential scanner alone, which passes identifiers
untouched.
Cost: the exemption list is asserted, so each entry carries a reason
and is an argued decision.

### D-05 Fixtures are generated, and the fake models what Gmail was seen to do

`internal/gapi/gmailtest` is an in-memory Gmail served at the REST paths
the client calls. Its mailbox is built from code: names from a fixed
synthetic list, addresses at `example.com`, `example.org` and
`.invalid`, bodies from templates, and ids from a counter formatted as
Gmail's hex. A fixture copied from a live response is itself the leak,
whatever a scanner says about it.

The fake behaves as Gmail was seen to behave where the server's logic
depends on it. A draft gets a new message id on every save, and its
`Message-ID` is replaced on create. `SENT` and `DRAFT` refuse manual
application, and a draft refuses a label. An expired history id
answers 404. `drafts.send` files the sent copy in `SENT` under a new id,
a permanent delete is refused without the full scope, and a failure can
arrive after the request was acted on. Every method charges its units.

The fake reads what it is sent with the standard library rather than
`internal/mime`, so the builder is never tested against its own
parser's mistakes.

Rejected: a fixture copied from a live response.
Cost: a wrong fake agrees with the code written from it. Only a live
run catches the shared mistake (D-89).

### D-06 Own wire types over raw REST, and no code imported from another project

`internal/gapi` calls the Gmail REST endpoints directly, and
`internal/gmail` holds wire types written by hand for the fields the
server uses. Nothing imports `google.golang.org/api`, and nothing
imports another project's server code.

That package drags in gRPC and telemetry for a set of JSON calls. Its
generated types would also decide which fields exist, where this
server needs an argued list. Machinery shared with other servers is
ported, verified here and owned here, so a fix elsewhere arrives as a
change someone read.

The client stays small enough to read in full. Each method it calls has
a verdict in `testdata/api-coverage.tsv`, and each field of the
resources it models a verdict in `testdata/api-fields.tsv` (D-07).
`TestTagsMatchDiscovery` holds the JSON tags to the discovery
document's names, and depguard refuses the import.

Rejected: `google.golang.org/api`, which drags in gRPC and telemetry;
depending on another server's code.
Cost: every field is hand-written and needs a verdict, and a field
Google adds stays unread until someone records it.

### D-07 Every API method and field has a written verdict

`scripts/gates api-diff` fetches Google's discovery document into
`testdata/api-surface.json`. `testdata/api-coverage.tsv` gives every
method published there a verdict: used, with the client function that
calls it; gated behind a switch; or written off with a reason.
`api-coverage` holds that in three directions, binding each row to its
method by the verb and path the client's code builds. `api-fields`
holds a verdict for every field of the resources the server models.

The discovery document is what Google publishes, and it is more
reliable than a reference page's prose. A list held against it turns
"the methods actually used" into an argued list rather than an
accident. It also records why a method is out of reach: some exist only
for a service account with domain-wide authority (D-101).

Rejected: a method table in this document, which would be a second
copy of the record.
Cost: `api-diff` is manual, so a method Google adds waits until
somebody runs it. The release checklist in [release.md](release.md)
says when.

### D-08 The server works inside one mailbox

The server finds mail, reads threads, messages and attachments, and
writes labels and drafts. Only when the operator opts in does it send,
delete for good, or change the signature, filters and vacation reply.
It stops at the edge of the message.

A calendar invitation is an `.ics` attachment here, read for the UID
and times a calendar server finds its event by (D-20). The event
belongs to a server built on the Calendar API. A linked Drive file is a
URL here; the file belongs to a Drive server. Mailbox administration,
such as delegation, forwarding, send-as identities beyond a signature,
S/MIME and client-side encryption keys, is out of scope (D-101).

The reason is reach. One mailbox, read and written as the person,
keeps the access the person grants as narrow as the job. Each of the
other systems has its own API, its own scopes and its own server.

Rejected: the event an invitation is about, a linked Drive file, and
mailbox administration.
Cost: a task that crosses into a calendar or Drive takes a second
server, handed the UID or URL this one reads.

### D-09 Mail content is marked as data, inside a per-call boundary

Every subject, body, header, snippet and attachment name was written by
someone other than the person using the server, and some of it is
written to steer an agent. Google's Workspace MCP guide warns that mail
"may contain hidden instructions that can hijack your session".

So rendered mail sits inside a delimited block naming its origin, the
sender's address and the message id. The block's boundary token is
drawn per call, and drawn again if the content holds it, so the content
cannot close the block itself. `structuredContent` carries the same
text in fields named `untrusted_*`. The tool descriptions and the server
instructions say mail content is untrusted, and no result phrases a
message's text as something to do.

A property test plants a marker in every sender-controlled field of
hostile mail and holds each one inside its block, across every
renderer.

Rejected: showing mail to the model as plain text.
Cost: the server cannot stop a model from being persuaded. It can only
make the persuasion visible, remove the tools that would make it
catastrophic (D-13), and never add its own voice to it (D-10).

### D-10 Sender text never reaches the server's voice

`internal/render` writes the server's own text, its notes and labels,
only from typed parts. Sender text reaches a result only through a
block (D-09), so it cannot write a line that reads as the server's.

The reason is that a rule kept by care breaks at the next field. A
part's `charset` parameter, which RFC 2231 lets a sender percent-encode
with newlines, can carry a fake server note. An attachment name or a
link's host can do the same. A type closes the whole class at once.

So a charset label is shown only when it has the IANA shape. An
attachment note does not name the file. A link-mismatch note names a
host only when it is host-shaped. Invisible characters in an address are
stripped and counted (D-11). The server instructions name only the tools
actually registered (D-85). `TestSenderTextNeverReachesTheServersVoice`
drives hostile raw mail through parse, model and render.

Rejected: a rule kept by care rather than a type.
Cost: a charset label is shown only in the IANA shape, and an
attachment note does not name the file.

### D-11 Text a reader would not see is removed and counted

The server converts HTML to text itself. Text a person would not see is
dropped: `display:none`, zero-size or same-color-as-background runs,
zero-width and bidi-control characters, and HTML comments. The result
says how many characters were dropped and why.

The count is the signal. Legitimate mail rarely hides much, and an
instruction written for an assistant often hides in exactly these
places. The same removal runs over headers, addresses, a part's type
and method, an attachment's text (D-26), and the signature and vacation
reply `get_settings` shows (D-62). A fuzz target holds the HTML
converter.

Rejected: passing hidden text through for the model to judge, which
hands it instructions the person never saw.
Cost: Gmail keeps hidden text in a signature or a vacation reply, so
the note there says the text holds it rather than that it was removed.

### D-12 Nothing a message references is fetched

No image, stylesheet, tracking pixel or link is fetched. Links render as
text with their host shown, and a link whose visible text names a
different host than its target says so.

A fetch tells the sender the mail was read, and sends a request where
the mail pointed. The server's job is to show mail as text, so it has no
reason to make either.

A message's row and its read say how the sender offers to unsubscribe.
`List-Unsubscribe` is read as
[RFC 2369 §2](https://www.rfc-editor.org/rfc/rfc2369#section-2) tells a
client to: bracketed URIs in the header's order, which is the sender's
order of preference. `one_click` is true when one is https and
`List-Unsubscribe-Post` reads exactly `List-Unsubscribe=One-Click`
([RFC 8058 §3.1](https://www.rfc-editor.org/rfc/rfc8058#section-3.1)).
The server never visits or writes to any of these addresses.

Rejected: rendering HTML.
Cost: `one_click` is the sender's claim. RFC 8058 §4 asks a receiver to
check a DKIM signature over both headers, and this server does not.

### D-13 The writes that redirect mail do not exist

Auto-forwarding, forwarding addresses, filters that forward, delegation
and send-as identities have no tool. They are written off in
`testdata/api-coverage.tsv`, not gated: absent in every configuration.

Each turns one persuaded call into a permanent leak of every future
message. A message saying "forward all mail to this address" is an
attack, and one was reported against a real server. A switch that
enables such a write is a switch somebody turns on.

The useful half of those APIs is showing that mail is being forwarded.
`get_settings` leads with forwarding (D-62), and `list_filters` flags
each forward action (D-56).

Rejected: gating these writes rather than leaving them out.
Cost: changing forwarding is written off; `get_settings` only shows it.

### D-14 Every read has a character budget and names what it left out

Every read has a budget in characters, stated in the result. A thread
renders newest first up to the budget. Quoted text and signature blocks
collapse to a line saying how much was collapsed. Messages past the
budget are listed by id, sender and date with a cursor. A long body is
cut at a paragraph boundary, with a marker and the offset to continue
from.

The rule is that a read never silently returns less than it found:
every omission is named and continuable. One thread from another server
returned 46k tokens to a client capped at 25k.

The parts around a body count too. A read's header block takes at most
half the budget and says where it was cut. Each list in the notes, such
as the links whose text names another site, takes at most an eighth,
and the rest are counted. Listings use `format=metadata` with a fixed
header set, never `full`. A page with a token is incomplete even when
it holds no rows.

Rejected: an unbounded read.
Cost: a long thread takes several calls, each continuing from the
cursor the last one gave.

### D-15 A listing's budget bounds its text, never its structured rows

`budget_chars` bounds a listing's text, in `content` and again in
`untrusted_text`. The structured rows carry every item on the page in
full and are not counted against it.

A page that fits is shown whole. Otherwise the text shows rows while
they and the line naming the rest fit. That line counts every row left
out and names their ids, each once, and `omitted_ids` holds the same
ids. `list_filters` shows forwarding filters first, so one that
forwards is never out of the text.

The rows are a released field, and emptying or dropping one changes
what it holds, which is a breaking change (D-84). A row dropped past the
budget is also unreachable, since the next page starts after it.

Rejected: bounding the structured rows, by dropping rows or emptying
their content.
Cost: a page of 100 came to 70,000 to 106,000 characters under a budget
of 24,000. The search and draft descriptions say so, and advise a
smaller `max`, such as 25, to a client that limits the size of one
result.

### D-16 `q` passes through verbatim, with `after` and `before` as instants beside it

`q` passes through verbatim. It is the person's own search language,
and rewriting it would make the server's results differ from the web
UI's for the same text.

Two parameters sit beside it, because Gmail reads a date in `q` as
midnight Pacific time
([filtering](https://developers.google.com/workspace/gmail/api/guides/filtering)).
`after` and `before` take RFC 3339 instants, or dates with a
`time_zone`. The server appends them to `q` as `after:<epoch>` and
`before:<epoch>`, and the result states the instants it used. Label
names in `labels` are resolved first (D-23). A query that cannot be
built sends nothing.

A zone name is read from the system's zone files, else from the copy of
the zone database built into the binary. Windows has zone files only
inside a Go installation, so the copy is what lets a person without Go
name a zone there.

Rejected: rewriting `q`, which would make results differ from the web
UI's.
Cost: a date written inside `q` is read as Pacific midnight, which the
description warns about, and the built-in zone database adds about
400 KB to the binary.

### D-17 A thread row names its newest message that was sent or received

A thread row's date and snippet are those of its newest message that is
not a draft, not in the trash and not an emoji reaction, chosen as a
reply's parent is chosen (D-40). A thread of nothing else shows its
newest message.

A draft after that message is the account's own unsent text. Showing it
would credit the account with the thread's last word. The row also says
whether the account sent that message, as `latest_from_me` from its
`SENT` label, and how many drafts the thread holds.

Rejected: a full read per thread to tell a reaction, which would
multiply a listing's cost.
Cost: a row may still name a reaction as the latest. A reaction is told
by its part, and the row's read carries headers only.

### D-18 `get_thread` shows a thread's drafts after the conversation

`get_thread` shows a thread's drafts after its conversation, in a
section of their own: "drafts in this thread, not sent". The cursor
counts only what was sent or received. Drafts past the budget are
listed by message id, a few of them, and the rest are counted.

Showing them lets a model see what is pending in a thread. A section of
their own keeps what was said apart from what might be. The service
returns Gmail's order, and `get_thread` and the renderer split the
drafts off, so what "latest" means stays the same for every other
caller.

Rejected: hiding a thread's drafts; folding them into the conversation
by date, where a draft reads as the last word.
Cost: a thread's drafts spend part of the read's budget.

### D-19 `list_changes` reports an expired cursor, never an empty list

`list_changes` takes a `history_id`, from `get_profile` or a previous
call, and returns the messages added, deleted and relabeled since, with
the next `history_id`.

Gmail keeps history records "typically" for "at least one week", and
an older `startHistoryId` answers 404, after which a client "must
perform a full sync"
([sync](https://developers.google.com/workspace/gmail/api/guides/sync)).
So a 404 here is reported as "cursor expired — history before this
point is gone", with the current `history_id` to restart from and a
note that the changes in between cannot be recovered. It never returns
an empty list for an expired cursor.

With a `label`, a 404 can also mean the label was deleted after it was
resolved. The label list is read again first, and a missing label is
`[not_found]`, never an expiry that moves the caller's cursor past
changes it can still read. Pages continue from the same `history_id`,
and the returned one is used only once the listing is complete.

Rejected: a 404 read as no such mailbox.
Cost: any 404 from `history.list` reads as expiry, since Google's body
says only "Requested entity was not found.", and the changes in between
cannot be recovered.

### D-20 An invitation is read for the UID and times a calendar server needs

An invitation's attachment entry says what it says about its event:
the UID, the sequence, one occurrence's recurrence id, the start and
end, the organizer's address, the title, whether it is all day, and
how many events the calendar holds.

A calendar server finds an event by its UID, the Calendar API's
`iCalUID`, so a hand-off from mail to calendar needs it. The times say
which meeting it is. Each time is RFC 3339 when it is UTC or names an
IANA zone, a date when all day, else as written with its zone.

The part is read as
[RFC 5545 §3.1](https://www.rfc-editor.org/rfc/rfc5545#section-3.1)
asks: lines unfolded before the charset is decoded, and text unescaped.
Only an event's own properties count. The caps are 1 MB of calendar,
eight levels of nesting, 256 characters of title and 1,024 bytes of
UID. `method` is reported only when RFC 5546 defines it, and every
other field is named `untrusted_*`. The stored `invite.ics` is read
beside an inline copy, since the sender can make them differ and the
stored one is the file a download saves.

Rejected: no parsing of a calendar part beyond its method.
Cost: reading a part stored apart costs one `attachments.get`, 20
units, at most one per message.

### D-21 Mail is addressed by Gmail's ids, and a draft result names both of its ids

Messages, threads and drafts are addressed by the ids Gmail gives them,
which are stable and opaque. The model never sees an index or a
position, and every result that lists mail gives each id beside it.

A draft has two ids: the draft's, and the id of the message inside it.
Every draft result names both, labeled. Confusing them is how a reply
lands on the wrong message, and the message id is also the witness a
draft write carries (D-42).

Rejected: an index or a position, which names a different message once
the mailbox changes between two calls.
Cost: a caller holds a draft's two ids apart, passing `draft_id` to the
draft tools and `message_id` as their witness.

### D-22 An `rfc822:` id is one Message-ID, never a search

RFC 5322 `Message-ID` values are shown as `untrusted_rfc822_message_id`,
and accepted wherever a message id is, prefixed with `rfc822:`. The
server resolves one through an `rfc822msgid:` search, and refuses
`[ambiguous]` when two messages carry it, as mailing lists make happen.

`rfc822:` takes exactly one id, written `<local@domain>` with no space,
quote, parenthesis, brace or colon. Anything else is `[invalid]` before
any search. Text after the id would be read as more search terms, which
could pick a message the caller never saw.

Rejected: passing the `rfc822:` text into a search.
Cost: obsolete forms and domain literals are refused, and such a message
is reached by its Gmail id.

### D-23 A label resolves by id or exact name, read on every call

A label is named by its id or by its exact name. A system label is
named by its id in any case, so `inbox` is `INBOX`. A user label's name
resolves through `labels.list`, read on every call that names a label.
An exact name wins; otherwise a name matches without regard to case.

A label renamed in the web UI between two calls must not resolve to its
old name, so nothing caches the list across calls. Google calls its
list of system labels "not exhaustive"
([labels](https://developers.google.com/workspace/gmail/api/guides/labels)),
so none is hard-coded beyond aliases for the common ones.

Gmail refuses a second user label that differs only in case, and
refuses `INBOX` and `Inbox` as user label names. A match without regard
to case therefore finds at most one label Gmail made. The `[ambiguous]`
answer with both ids stays for a list that says otherwise, since
nothing documents the rule.

Rejected: a per-process cache; a hard-coded list of system labels.
Cost: the label list is read on each call that names a label.

### D-24 Addresses are strict going out, lenient coming in, and shown so they read back

Going out, an address is parsed with `net/mail`. A display name with
non-ASCII is encoded per RFC 2047, and an address the parser refuses is
`[invalid]`, naming the argument, never passed through.

Coming in, a header `net/mail` refuses is read leniently, and the result
says so. An address written inside a quoted name or a comment is never
taken, since a sender writes one there to pass it off as the sender.
The one angle address outside them is taken, else the one bare address.

Shown, an address is `Name <address>`, the name quoted unless it is
plain words and never encoded. So every address a result shows reads
back with `net/mail` as the same mailbox, and can be given back as one
recipient. A name with a comma does not split in two, and a name
holding `<x@y>` stays a name. Fuzz targets hold both directions.

Rejected: an address written inside a quoted name or a comment.
Cost: with none, or more than one, the text is kept as a name with no
address.

### D-25 Files move only through `GMAIL_LOCAL_DIR`, never over an existing file

Files move between the mailbox and the disk only through the one
directory `GMAIL_LOCAL_DIR` names. `download_attachment` and
`download_attachments` are registered only when it is set.
`create_draft` attaches base names inside it alone. A path, a `..` or a
symlink out of it is `[invalid]`, and with the directory unset an
attachment is `[blocked]`.

A message could talk the model into reading any path it names, or
writing over one. One directory the person chose bounds both.

A file is created with `O_EXCL` through an `os.Root` on the directory,
so it never replaces another; a clash gets `-1`, `-2` before the
extension. It is readable by its owner alone, and a failed or partial
write is removed. A file to attach must be a regular file, so a named
pipe cannot hang the call. The result names the path written, the size
and the SHA-256.

Rejected: any path the caller names.
Cost: with `GMAIL_LOCAL_DIR` set, read-only mode still registers the
two download tools, since read-only is about the mailbox.

### D-26 `read_attachment` reads text types inside the boundaries and the budget

`read_attachment` reads one attachment into the result as text, and is
registered in every mode. It reads plain text, CSV, Markdown and JSON as
written, HTML converted as a body is, and a calendar file as written.
An attached message is parsed and rendered as `get_message` renders
one.

A person asks about an attached file as often as about a body. Without
this tool the only path is to save the file and open it outside the
server, where nothing marks it as mail. A forwarded message often
arrives as an attached `.eml`.

So the text gets what a body gets: hidden text removed and counted
(D-11), the same boundaries and `untrusted_*` fields (D-09), and a
body's budget, `offset` and `show_quoted` (D-14). Any other type is
`[unsupported]` and points to `download_attachment`. A part declared
over 5 MB is refused before it is read.

Rejected: never inlining an attachment.
Cost: an attachment is a large place to hide instructions, and one a
person rarely reads before an assistant does. A result can carry up to
5 MB more mail.

### D-27 MIME is the server's own, and what it builds parses back the same

`internal/mime` parses and builds RFC 5322 with MIME (RFC 2045 to 2047)
and RFC 2231 parameters, on the standard library's `mime`,
`mime/multipart`, `mime/quotedprintable` and `net/mail`. The property
that matters is that what the server builds, it parses back to the same
fields.

Other servers garbled subjects, failed on an accented display name,
named attachments `noname` and hard-wrapped paragraphs. So a read walks
the whole tree: each part's charset, parts stored behind an
`attachmentId`, HTML-only mail, and a plain part that only points at
the HTML. A build writes RFC 2047 encoded-words, RFC 2231 filenames and
quoted-printable text. `EditRaw` edits a draft's own bytes as a tree and
leaves every part it does not touch as Gmail stored it.

Fuzz targets hold that built messages parse back the same, over names,
subjects, bodies and filenames in scripts nobody on the project reads,
and that an edit never breaks a parse. The fake reads with the standard
library, not this package (D-05). A live run round-trips four scripts
through Gmail.

Rejected: testing the builder against its own parser.
Cost: `EditRaw` does not hold the 998-character line limit `Build`
does, since a stored draft may already carry longer lines.

### D-28 A paragraph goes out as one line, never wrapped to a width

A paragraph goes out as one line, as quoted-printable UTF-8. Soft line
breaks keep every encoded line within 76 characters and vanish on
decoding
([RFC 2045 §6.7](https://www.rfc-editor.org/rfc/rfc2045#section-6.7)).

RFC 5322's lines "MUST be no more than 998 characters, and SHOULD be no
more than 78" are the encoded message's lines. It also makes it
"incumbent upon implementations that display messages to handle an
arbitrarily large number of characters in a line"
([RFC 5322 §2.1.1](https://www.rfc-editor.org/rfc/rfc5322#section-2.1.1)).
Text cut to a width is
[RFC 3676 §3.2](https://www.rfc-editor.org/rfc/rfc3676#section-3.2)'s
embarrassing line wrap, shown on a screen narrower than the cut. A
build test sends a paragraph of over 2,000 characters in lines of at
most 76, and reads it back as one line.

Rejected: wrapping to 78 or 72 columns.
Cost: RFC 3676 §3.1 calls long paragraph text an error, because some
readers scrolled a long line rather than wrapping it.

### D-29 A draft without `body_html` carries an HTML version made from its text

A draft written without `body_html` also carries an HTML version made
from its text. It holds the same words, escaped: a `<p>` per block of
lines, a `<br>` per line break inside one, runs of spaces kept, and each
web address a link (D-32). It goes after the plain part, in
`multipart/alternative`.

Gmail's web composer opens a draft with no HTML part as plain text.
When the person sends it, Gmail wraps every line over 78 characters at
70 and sends nothing else. With an HTML part it sends both, as it does
any mail written in it, and readers show the HTML. So a paragraph
written on one line reaches its reader whole, whoever presses send.

The `body` descriptions ask for a paragraph per line and a blank line
between paragraphs, which is what the HTML version is made from.

Rejected: plain text alone, which Gmail's web composer wraps at 70;
`white-space` styling, which classic Outlook ignores.
Cost: a mailing list that refuses HTML needs `plain_only` (D-31).

### D-30 The made HTML is recognized by reading it back, and a draft keeps its shape

`update_draft` given `body` alone keeps the draft's shape. Plain text
alone stays alone. An HTML version made from the old body is made again
from the new one, and goes with the text when the body is emptied. A
draft with no text yet has no shape, and takes the HTML version.

The made version is recognized by reading it back, not by remaking it.
HTML holding only paragraphs, line breaks and links whose text is their
target, which reads back as the stored text, says nothing the text does
not, whatever rules made it. A change to the line rules therefore
strands no draft made before it. `mime.EditRaw` decides whether the HTML
moves with the parts it replaces in hand, so the decision and the edit
are one step on one parse. A fuzz target holds that every version the
server makes reads back as made.

Rejected: recognizing the made version by remaking it.
Cost: HTML written another way needs `body_html` beside `body`, and
`plain_only: true` will not delete it.

### D-31 `plain_only` keeps a draft plain on request

`plain_only: true` on `create_draft` keeps the text alone, with no HTML
version. On `update_draft` it changes the shape: `true` drops the made
version, and `false` adds one.

Some mailing lists refuse HTML. The kernel's etiquette says "almost all
kernel mailing lists will reject HTML email", and git's contributor
guide says "the Git list rejects HTML email". A draft for such a list
needs the plain part alone. `signature` with `plain_only` is
`[invalid]`, since plain text has nowhere for a signature's links and
images.

Rejected: an enum for `update_draft`'s `plain_only`. The flag keeps
`create_draft`'s name and sense, and the default keeps a draft made
plain on purpose plain.
Cost: Gmail's web composer wraps such a draft at 70 when a person sends
it (D-29); `send_draft` sends it as built.

### D-32 A web address in the made HTML is a link whose text is the address

Each `http` or `https` address in the made HTML, and in a signature
`update_signature` writes, is a link whose text is the address itself.

No reader is documented to link a bare address in HTML. Outlook's help
describes its composer, not its reader. A link whose text equals its
target cannot trip a spam filter's mismatch rule, which flags text that
names another domain. Only `http` and `https` are linked, as OWASP's
XSS cheat sheet advises: "Allow-list http and HTTPS URLs only".

Where an address starts and ends follows GitHub's autolinks: after a
space or an opening bracket or quote, never inside a word, with
trailing punctuation left out. It also stops at quotes, angle brackets
and any character a reader cannot see. A fuzz target holds that a line
adds no other markup and reads back as written.

Rejected: leaving links to the reader; a scheme other than `http` and
`https`.
Cost: a line holding a bidi control gets no link at all, since an
override earlier on the line can draw a link's text as another address.

### D-33 The body is exactly what the caller gave, and the server adds only what is named

The body is exactly what the caller gave. No prefix, footer or "sent
by" line is added server-side, ever.

The server adds only what the call or the operator names: the parent's
text with `quote` (D-34), the original as an attachment with `forward`
(D-35), and the signature with `signature` or the operator's default
(D-36). Without `body_html`, the same words also go as an HTML version
made from the text (D-29), so a reader sees the caller's words either
way.

Everything that leaves this server is words the person can read in the
draft before it goes. A sent copy read in a second mailbox shows the
subject and body exactly as the draft held them, with nothing added. A build test holds that plain ASCII goes out byte for byte.

Rejected: a prefix, a footer or a "sent by" line.
Cost: a draft carries nothing that says an assistant wrote it. What it
says is the person's decision.

### D-34 `quote` puts the parent's text, as a read shows it, below the body

`quote: true` on a reply puts the parent's text below the body: the
body as given, a blank line, `On <Date>, <From> wrote:`, then each line
of the parent's text after `> `.

The text is the parent's as `get_message` reads it, so the quote shows
what the person saw. A plain part goes as written, with what a reader
would not see removed. An HTML-only parent, or one whose plain part only
points at its HTML, is converted, and the result says
`quote_from_html`. The parts Gmail stored apart are read for it, 20
units each, and the result says how many. The HTML version is made from
the whole text, quote included, so `update_draft` still recognizes it as
made (D-30).

Rejected: "no suffix" without exception, which left a reply with no way
to quote what it answers.
Cost: a later `update_draft` with `body` replaces the quote, and
`quote` with `body_html`, or without a reply target, is `[invalid]`.

### D-35 `forward` attaches the original as Gmail stored it, its `Bcc` left out

`forward` on `create_draft` takes a message id, or `rfc822:` and its
`Message-ID`, and attaches that message to the draft as
`<subject>.eml`, read with one `messages.get` in `format=raw`.

The copy goes as Gmail stored it, with two changes. Its line endings
are written as CRLF. Its `Bcc` and `Resent-Bcc` headers are left out,
since the sender's own copy of a sent message keeps its `Bcc`, and a
forward would show the blind recipients to everyone it reaches
([RFC 5322 §3.6.3](https://www.rfc-editor.org/rfc/rfc5322#section-3.6.3)).
The result's `bcc_removed` says so. The header block is split before
any line ending is rewritten, so a bare CR cannot move a `Bcc` into the
body.

RFC 2046 §5.2.1 permits only `7bit`, `8bit` or `binary` for an attached
message, so the part is declared `7bit` or `8bit`. The draft is asked to
join the original's thread, with no `In-Reply-To`, and the result says
whether Gmail filed it there.

Rejected: an inline forward, which rewrites the original rather than
carrying it as stored.
Cost: a message with a line over 998 octets or a NUL is `[unsupported]`,
since only `binary` carries one and SMTP carries binary only between
servers that both offer it (RFC 3030). One over about 24 MB is
`[invalid]`.

### D-36 A signature is added only when the call or the operator asks

`signature: true` on `create_draft` puts the signature Gmail holds for
the From address below the body, as Gmail's web composer puts one in a
new email. `GMAIL_DRAFT_SIGNATURE=true`, off by default, signs a new
draft when the call leaves `signature` out, and the result says
`by_default`.

Gmail's composer adds no signature to a draft it opens, and
`drafts.send` adds none, so a draft carries one only when the server
puts it there. Who asks matters. The call asks for one draft; the
operator, outside the conversation, asks for every new one; the model
never decides it alone. The default never refuses. A body that already
holds the signature, or plain text alone, goes without it, and the
result says `skipped` and why. `update_draft` ignores the setting.

The signature comes from the `sendAs.list` the call reads anyway. The
result says what the signature shows, its lines, images and hidden
characters, and never its text.

Rejected: signing every draft by default for everyone.
Cost: a reply or a forward is signed only with `signature: true`,
since the API holds only the signature for new emails
([SendAs](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.settings.sendAs#SendAs)).

### D-37 A signature goes in the composer's markup, its images as parts

The HTML version carries the signature as Gmail holds it, links and
images included, in the composer's own markup. Ending the message, it
sits inside
`<div dir="ltr" class="gmail_signature" data-smartmail="gmail_signature">`,
after
`<div><br clear="all"></div><div><br></div><span class="gmail_signature_prefix">-- </span><br>`.
Above a quote there is no `-- `, as Gmail writes it there. The plain
text carries it after a blank line and, ending the message, the
delimiter line `-- `
([RFC 3676 §4.3](https://www.rfc-editor.org/rfc/rfc3676#section-4.3)),
so a reader that drops a signature keeps the quote below it.

An image the signature holds as a base64 `data:` URL goes as a part of
its own beside the HTML, in `multipart/related`
([RFC 2387](https://www.rfc-editor.org/rfc/rfc2387)). Its `src` is a
`cid:`, and its Content-ID is made from the image, so one image is one
part. Gmail moves such an image out on save itself, and drops the plain
version as it does.

Rejected: a `data:` image left inside the HTML, which Gmail rewrites and
stores without the plain part.
Cost: the markup is the composer's, copied rather than documented, so
only a live run shows Gmail still reads it as a signature.

### D-38 A draft never carries two signatures, and a new body keeps its own

A draft never carries two signatures. When a signature would go below
the body, `body_html` that holds the composer's block is `[invalid]`, and
so is a `body` that holds the signature's text as whole lines.

`update_draft` given `body` keeps the signature the draft carries below
the new body. It is found by its markup in the HTML and by its text in
the plain part. It counts as the draft's own only when the rest of the
HTML is the version made from the text. A block inside someone else's
mail, quoted or forwarded, is therefore never carried, so a sender's
tracking image or hidden text cannot go out from the account.

New `body_html` on a draft whose HTML holds a block that is not carried
is `[conflict]` unless `signature` says what to do. Replacing it
silently would destroy what the caller cannot see. `false` writes the
draft without one, and `true` puts Gmail's below it. Fuzz targets hold
that a signature written below any body is found and carried below a
new one.

Rejected: replacing a signature silently on new `body_html`; carrying
the last signature-shaped block in the HTML.
Cost: a draft whose HTML was written another way needs `body_html`
beside `body`, and takes its signature again with `signature: true`.

### D-39 A reply is threaded by the server from its parent

A reply is built by the server from its parent, never assembled by the
model. Gmail threads a message on three things: its `threadId`,
`In-Reply-To` and `References` headers, and a matching subject
([threads](https://developers.google.com/workspace/gmail/api/guides/threads)).

The server reads the parent's `Message-ID`, `References` and subject.
It writes `In-Reply-To` as the parent's `Message-ID`, `References` as
the parent's plus that id, `threadId` as the parent's thread, and the
subject with one `Re:`. These headers carry RFC 5322 `Message-ID`
values, which are not the API's message ids
([RFC 5322 §3.6.4](https://www.rfc-editor.org/rfc/rfc5322#section-3.6.4)).
`References` keeps the parent's first id, its last 38 and the parent's
own, so a hostile parent cannot make a reply huge.

Each condition matters to someone. For the sender's mailbox, `threadId`
alone decides. A Gmail recipient threads by the headers and the
subject. A live run checks both.

Rejected: the API's message id in `In-Reply-To`; `threadId` alone.
Cost: a caller's `subject` on a reply is `[invalid]`, since it is one of
the three conditions.

### D-40 `reply_to` takes a message id and `reply_to_thread` a thread id

`reply_to` takes a message id. `reply_to_thread` takes a thread id, and
the server picks the newest message that is not a draft, not in the
trash and not a reaction, and names the chosen message in the result.
A `reply_to` naming a draft, a trashed message or a reaction is
`[invalid]`.

They are two fields because the two ids cannot be told apart. A
thread's id is its first message's id, so one field taking both would
answer the first message when the caller meant the thread. The parent
is chosen, never defaulted, since a draft, a trashed message or an
emoji reaction is not something to reply to.

Rejected: one field taking both ids.
Cost: `reply_to_thread` reads the thread with its bodies, since only a
format that carries the parts can tell a reaction.

### D-41 `reply_all` expands from the parent and names where each recipient came from

`reply_all: true` expands the recipients from the parent and drops the
account's own addresses, read from `sendAs.list`. When the parent is the
account's own message, the reply goes to its To, as in Gmail. The
caller's addresses are added last.

Each recipient in the result carries its origin: the parent,
`reply_all`, or the caller. An address a correspondent wrote into their
own message is a stranger's choice, and the result shows which ones
those are before anything is sent. The send still needs every one named
(D-49).

Rejected: expanding reply-all without saying where each address came
from, which hides an address a correspondent added among the ones the
caller chose.
Cost: a parent's address that cannot be written into a header is left
out and counted, never quoted in the server's voice.

### D-42 A write never replaces what it did not read

gmail/v1 has no ETag and no `If-Match` anywhere, and `drafts.update` is
a PUT that "replaces a draft's content"
([drafts.update](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.drafts/update)).
The last writer wins. So a draft update is a read, a merge and a write
against a witness.

The witness is the id of the message inside the draft, which Gmail
changes on every save. `get_draft` returns it. `update_draft` requires
it, reads the draft again, and refuses `[stale]` if the draft moved. It
then merges the fields given onto what it read, as a tree edit of the
draft's own bytes. Headers and parts the call does not name are written
back byte for byte. Omitted means unchanged, and an attachment is
removed only by the `part_id` the call names.

Labels are patched, never replaced: `labels.update` is written off.

Rejected: a hash of the raw bytes as the witness; dropping an
attachment, a Cc or a header by omission.
Cost: the window between the read and the PUT is not closed, and the
result does not claim it is.

### D-43 Every write takes `dry_run`, and a dry run cannot write

Every write takes `dry_run`. It puts the call on a context under which
`internal/gapi` refuses every write before it reaches the network.

A preview that works by convention writes the first time one branch
forgets it. A refusal in the one client every request goes through holds
for every tool, including one added later. A preview may still read what
it has to show. `send_draft`'s reads the draft and its thread, and the
label writes predict each item's labels after from the labels they read.
The `outcomes` gate holds that every write's result states its outcome
on every branch, the dry run's included.

Rejected: a dry run kept by each tool's own code path, which one
forgotten branch turns into a write.
Cost: a dry run spends the reads its preview needs: `send_draft`'s costs
one `drafts.get` and one `threads.get`.

### D-44 Writes take explicit ids, at most 100, never a query

`modify_labels`, `trash`, `restore` and `delete_permanently` take
explicit message or thread ids, at most 100 per call. None takes a
query.

A search is a read the model has seen, and a write names what it
touches. An "apply to all results" parameter is how a single injected
instruction empties a mailbox (D-99).

A filter matches a query, and is not an exception. Gmail applies it only
to mail that arrives after it exists, and the server never applies one
to mail already there. What a filter can hide is mail nobody has seen
yet, which is why one that trashes is a tool of its own behind two
switches (D-57).

Rejected: an "apply to all results" parameter; `batchModify` over the
results of a search.
Cost: a call names at most 100 ids, far below the 1,000 `batchModify`
accepts.

### D-45 Each item of a multi-id write is read, written only if needed, and reported alone

Each item of a multi-id write is its own pair of calls: a read of its
labels, then `messages.modify`, `trash` or `untrash` on the message, or
the thread's. The answer is the labels after. An item already as asked
is not written, and is reported unchanged. A call that trashed 97 of 100
says which three failed and why.

Another server reported a batch's per-item rate-limit failures as
success. `batchModify` answers with nothing
([batchModify](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.messages/batchModify)),
so it could say neither which messages changed nor why one failed.
`batchDelete` "provides no guarantees that messages were not already
deleted or even existed". `modify`, `trash` and `untrash` are declared
repeatable, since applying a label twice is applying it once (D-78).

Rejected: `batchModify`, which answers with nothing; `batchDelete`.
Cost: each item is its own pair of calls, read and write, and spends its
own units.

### D-46 Results say what changed, read back from Gmail

Every write reports the state it produced, read back rather than
assumed: each message's labels before and after, the draft's thread,
the recipients a send reached, the messages a trash moved. A write that
changed nothing says so, as "already in `TRASH`", rather than reporting
success.

A result that asserts what it meant to do misleads the next call. The
draft's thread is read from the `threadId` of the draft Gmail made, so
the result says whether Gmail filed a reply or a forward where it was
asked to. The `outcomes` gate holds that every write's result states its
outcome on every branch.

Rejected: reporting success for a write that changed nothing.
Cost: every write reads before or after it writes, and the units each
result reports include those reads.

### D-47 A call that cannot be undone repeats what it acts on

A call that cannot be undone carries what it acts on, in words the
person would recognize. `delete_draft` takes the draft's `message_id`.
`send_draft` takes the `message_id`, the draft's `subject` and the first
line of its body, `first_line`. `delete_filter` takes the filter's
`criteria`. Each is compared with what Gmail holds, and refused `[stale]`
when it is not.

A client's prompt shows the call's arguments and nothing else (D-65).
So the argument is where the person sees what the call does. `subject`
and `first_line` are compared as `get_draft` shows them, hidden
characters removed and the spaces around them dropped. `first_line` is
compared after the checks that cost nothing, since reading it can cost
20 units a part. Neither the refusal nor a log repeats the value,
because a draft's text can come from someone else's message.

Rejected: a question the server puts to the person, which D-104 records.
Cost: what no argument carries, a send's attachment count, the body
past its first line and a filter's actions, shows only on the dry run.
A change between the witness read and `drafts.send` is what Gmail sends.

### D-48 Sending exists only with `GMAIL_ENABLE_SEND`, and only through a draft

`send_draft` is registered only with `GMAIL_ENABLE_SEND=true`. There is
no one-shot send (D-103): everything that leaves the server existed
first as a draft the person could open in Gmail.

Registration is the only control, because the scopes cannot separate
drafting from sending. `gmail.compose`, "Manage drafts and send
emails", and `gmail.modify` both grant `messages.send` and
`drafts.send`
([scopes](https://developers.google.com/workspace/gmail/api/auth/scopes)).
The token that writes a draft can send. Google's own Gmail MCP server
takes the same position: it asks for `gmail.compose` and ships drafts
with no send tool.

An annotation is not a control either. A registered tool can run
unattended.

Rejected: a narrower scope that drafts but cannot send, which Gmail does
not have; an annotation as the control.
Cost: the default token can send. Leaving `send_draft` unregistered
stops this server sending, and does not stop anything else that holds
the token, which is why the token lives in the keyring (D-71).

### D-49 Every recipient of a send is named in `confirm_recipients`

Every recipient of a send must be listed in `confirm_recipients`,
exactly. Otherwise the call is `[blocked]` and names the recipients by
field and position, such as `cc[1]`, never by address. Over 50
recipients is `[blocked]` outright.

A client's prompt shows only the call's arguments, so the list there is
everyone the send reaches. The model has to write out an injected
address itself, rather than inherit it from a draft someone else shaped.
The thread's participants are read and shown, and still confirmed.

A draft whose `To`, `Cc` or `Bcc` is repeated, or can be read only
leniently, is `[blocked]`, dry run included. The guard reads the first
of a repeated header and drops what a lenient read cannot parse, while
Gmail sends to every address it finds, so the guard would check a
shorter list than the send reaches. A dry run reports the unconfirmed
recipients instead of refusing, which is how the caller learns whom to
confirm.

Rejected: a received message's `To`, `Cc` and `Reply-To` counted as
participants, since its sender wrote them; participants cleared without
confirming.
Cost: one `confirm_recipients` entry per address, and over 50 is refused
outright.

### D-50 A send is never retried, and an ambiguous one is settled by reading

`drafts.send` takes only an id
([drafts.send](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.drafts/send)).
There is no request id and no documented deduplication. A retried send
whose first attempt landed is a second delivery to every recipient, a
defect reported against another server. So the send is one call, never
retried.

On an ambiguous failure, or an `[unavailable]` one that may have gone
out, the tool settles by reading. Before the send it records the ids of
every message in the draft's thread. After the failure it waits 5
seconds, then reads the draft and the thread. The draft gone and a
message in `SENT` that was not there before means sent. The draft
present and no such message means not sent. Anything else is unknown.
The reads run even when the call's own context has ended, bounded at 30
seconds.

The `Message-ID` cannot settle it. Gmail replaces it on `drafts.create`
and again on `drafts.send`, and the sent copy gets a new id, filed in
the draft's thread.

Rejected: settling by the `Message-ID`; resending on "not sent".
Cost: a verdict can stay unknown, and the person decides.

### D-51 Removal is trash, an ordinary write

`trash` and `restore` are ordinary writes, registered by default. Gmail
keeps trash for 30 days.

Gating trash would teach people to set the switch that also arms
permanent deletion. A gate everyone turns on protects no one. Removal
that can be undone is the removal a model should reach for, so it is
the one always there.

Each item is read first, and its labels after are reported, as every
multi-id write does (D-45). Gmail takes a trashed message out of the
inbox, so `trash` adds `TRASH` and removes `INBOX`, and its dry run
predicts both. `restore` takes off `TRASH` alone.

Rejected: gating trash behind the destructive switch.
Cost: a draft cannot be trashed, so `trash` refuses that item and
points to `delete_draft`.

### D-52 Permanent deletion exists only with the flag that requests full mail access

`delete_permanently` and `delete_label` are unregistered unless
`GMAIL_ENABLE_DESTRUCTIVE=true`, and each call takes `confirm: true`.
That switch is also the only setting that asks for
`https://mail.google.com/`.

`messages.delete`, `messages.batchDelete` and `threads.delete` accept
only that scope
([messages.delete](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.messages/delete)),
and a live run holds that `messages.delete` under `gmail.modify`
answers 403. So a default token cannot delete for good even if a tool
were registered by mistake: the scope and the registration are two
locks. `delete_permanently` reads each item first, and refuses a thread
holding a draft for that item, pointing to `delete_draft`.

Rejected: a delete tool registered and refusing, which would fail every
call without the scope.
Cost: turning the switch on needs a fresh `login`. The filter deletes
bring the full-mail scope too (D-57).

### D-53 `delete_draft` is a default write that takes `confirm` and its witness

`delete_draft` is registered by default. Its call takes `confirm: true`
and the draft's `message_id`, and is refused `[stale]` when the draft
holds another. A draft deleted between the read and the delete is
reported gone. It is annotated destructive.

Gmail deletes a draft rather than trashing it, so the delete cannot be
undone. Gating it behind the destructive switch would arm permanent mail
deletion, and ask for full mail access, for whoever wants to tidy their
drafts. `update_draft`, an ordinary write, can already replace a draft
for good. The witness holds the delete to the draft the caller read.

Rejected: gating it behind the destructive switch.
Cost: one delete that cannot be undone is registered by default.

### D-54 Archiving, marking read and starring are label changes, not tools

`modify_labels` adds and removes labels. Archiving is removing `INBOX`,
marking read is removing `UNREAD`, and starring is adding `STARRED`. The
description says so, and the result names the verb the change amounted
to.

Each verb is the same write on the same resource. A tool per verb would
multiply the surface without adding a rule.

`SENT` and `DRAFT` are `[conflict]` before anything is sent, since Gmail
refuses them by hand. `TRASH` is `[invalid]`, pointing to `trash` and
`restore`. A draft's message is `[unsupported]` for that item, since
Gmail does not label drafts.

Rejected: a tool per verb.
Cost: the model has to know that archiving is removing `INBOX`, which
the description and the result's verb carry.

### D-55 Settings writes exist only with `GMAIL_ENABLE_SETTINGS`, which adds their one scope

The settings writes are registered only with
`GMAIL_ENABLE_SETTINGS=true`: `update_signature` and `create_filter`, then
`set_vacation` with the send switch as well (D-60), and
`create_trash_filter` and `delete_filter` with the destructive switch as
well (D-57).

The switch also asks for `gmail.settings.basic`, the one scope the
discovery document lists for these methods
([filters.create](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.settings.filters/create)).
Not even `https://mail.google.com/` covers them. A setting changes how
the account treats every message after it, so the operator decides it
before the conversation starts.

Each settings write reads what it changes first, so its result shows
before and after, and a dry run stops after that read. The descriptions
of `get_settings` and `list_filters` follow the switches (D-85).

Rejected: the full-mail scope satisfying the settings writes, which
Gmail does not allow.
Cost: `GMAIL_ENABLE_SETTINGS` adds its scope in every mode, the
destructive one included.

### D-56 A filter cannot forward

`create_filter` cannot forward. `gmail.FilterWrite`, the type a filter is
written as, has no forward field, so no argument can express one.

A forwarding filter turns one persuaded call into a permanent leak of
every future message that matches (D-13). Absent from the type, it
cannot be reached by a bug, a flag or an argument. `list_filters` still
reads a filter's forward action and flags it, and shows such filters
first (D-15).

The write type is narrower than the schema it writes. `api-fields`
holds it to that schema's fields through a declared view, so the
missing field is a recorded verdict rather than an oversight.

Rejected: a forwarding filter behind a switch rather than absent.
Cost: a person who wants a forwarding filter makes it in Gmail.

### D-57 A risky variant is its own tool

A form of a write that does more harm than the ordinary one is a tool
of its own, never an argument. A filter that trashes is
`create_trash_filter`, behind the settings and destructive switches,
with `confirm: true`. Its one action adds `TRASH`, so matching mail goes
to the trash unseen as it arrives.

A client remembers "always allow" per tool, never per argument value. A
risky argument on an allowed tool rides along unseen. A tool of its own
gets its own prompt and its own switch.

`set_vacation` keeps on and off as one tool. Turning a reply off is the
safe half of a risky tool, not a risky variant of an ordinary one.

Rejected: `create_filter` taking a `trash` flag.
Cost: `create_trash_filter` and `delete_filter` need the destructive
switch, and so the full-mail scope.

### D-58 A filter's duplicate check and an unconfirmed write are settled by reading the list

An identical filter is `[conflict]`, found by reading the list first.
Gmail keeps a second identical filter rather than refusing it.

A create Google did not confirm is `[ambiguous_outcome]`, and is not
repeated. As a send does (D-50), the server waits 5 seconds, reads the
list, and reports `created` with the filter that matches, `not_created`,
or `unknown` when the read fails. A delete Google did not confirm
settles the same way, as `deleted`, `still_there` or `unknown`. A 404
on a delete is reported as gone.

Criteria are compared exactly. Gmail adds `SPAM` to an archiving
filter's removed labels later, so the check and the settle read ignore a
stored `SPAM` when the request archives and does not name it. The
conflict says so, and how to get the filter without it.

Rejected: trusting Gmail to refuse a duplicate; a loose criteria match
that folds case and `OR`, which changes a Gmail query; repeating an
unconfirmed create.
Cost: a filter with a size may slip past the check, since Gmail stores
the size rounded by a rule nobody documents.

### D-59 Settings changes run one at a time, and filter writes are paced

Settings changes run one at a time within the process. Each holds the
client's settings lock from its first read to its write, and a dry run
takes none. Filter writes are also paced: each attempt waits until 5
seconds have passed since the last one finished, and reserves its slot
before it waits.

Gmail refuses filter writes sent together, and some sent back to back,
with 400 `failedPrecondition`, "Precondition check failed.", and takes
each one 5 seconds later. That refusal says nothing of its cause, and a limit
or a policy may answer the same. So it is repeated once after the
pacing wait, then reported `[unavailable]` in Google's words, with no
cause claimed. When an earlier attempt may have acted, it is
`[ambiguous_outcome]` instead (D-58).

`update_signature` and `set_vacation` take the lock but not the pacing
or the repeat, since nothing showed their writes refused that way. Any
other `failedPrecondition` stays `[invalid]`.

Rejected: calling the `failedPrecondition` an overlap; a 2-second wait,
which nothing showed to work; pacing `sendAs.patch` and
`updateVacation` without evidence.
Cost: each filter write waits until 5 seconds after the last finished.

### D-60 The vacation reply needs the send flag, a bounded audience and `confirm`

`set_vacation` needs `GMAIL_ENABLE_SEND` as well as the settings switch.
An auto-reply writes to other people, which is sending by another name.

Turning it on needs a body, an `audience` of `contacts` or `domain`, and
`confirm: true`; `start` and `end` bound it. The audience maps to
Gmail's `restrictToContacts` and `restrictToDomain`
([VacationSettings](https://developers.google.com/workspace/gmail/api/reference/rest/v1/VacationSettings)),
and every sender is not offered. Turning it on writes every text field,
empty included, so an old HTML body cannot go out in place of the
caller's. The body goes exactly as given. Turning it off keeps its text
for next time.

Rejected: an audience of every sender; on and off as two tools, since
off is the safe half (D-57).
Cost: `domain` is refused on a personal account (`gmail.com`,
`googlemail.com`), found by one profile read. Whether Gmail refuses it
there or answers every sender is unchecked.

### D-61 `update_signature` writes plain text lines, never caller markup

`update_signature` sets the signature of one of the account's own
send-as addresses, the default unless `send_as` names another. The text
is plain. Each line is written as the made HTML writes it (D-29), with
runs of spaces kept, and line breaks become `<br>`. Empty clears it. It
patches the signature and nothing else.

A signature goes out on every message the person writes. The caller is
a model that reads mail other people wrote, so markup it passes is never
applied, and a sender's tracking image or hidden text cannot become the
account's signature. Web addresses still become links (D-32).

A signature of images or hidden text alone, before or after, is
reported as set with no text, never as none.

Rejected: applying markup a caller passes.
Cost: a signature with an image, or a link whose text is not its
address, is set in Gmail, not here.

### D-62 `get_settings` leads with forwarding and says what is empty

`get_settings` returns the vacation reply, auto-forwarding, forwarding
addresses, IMAP, POP, language and send-as identities, forwarding first.

Showing that mail is being forwarded is the useful half of those APIs,
since nothing here changes it (D-13). Addresses, display names and
filter criteria are the account's own configuration, and stand in the
server's voice. The vacation reply and the signatures are free text that
goes out as mail, so they sit inside blocks and `untrusted_*` fields
(D-09).

Each send-as address states its name and signature even when Gmail
returned none. `signature_set` tells a signature of images or hidden
text alone, which has no text to show, from none. Blank text, or markup
or a comment alone, is none. The signature shown is the one Gmail adds
to new emails, the only one the API holds, and the heading says so.

Rejected: leaving the signature line out when Gmail returns none.
Cost: an account whose signature is chosen only for replies reads as
none.

### D-63 Every tool goes through one `register`, whose `Kind` decides the rest

Every tool is added through one `register` in `internal/tools`. A tool's
`Kind` decides its annotations, whether it registers under the
configuration, its `_meta`, whether it takes `confirm`, the dry-run
context and the rendered reply. Nothing else may decide those.

The kinds are Read, Read (local write), Adding, Write, Write for good,
Send, Destructive, Settings, Settings for good and Auto-reply. Each says
what a tool does to the world and which switch it sits behind.

Rules kept by hand at every call site are ways to be quietly wrong, one
tool at a time. A rule kept in one function holds for a tool added
later. `register` panics at start on a combination it refuses (D-66),
so the mistake fails in every configuration. Tests drive the surface in
each mode, and the smoke gate drives the shipped binary in a mode per
kind.

Rejected: annotations, gating and `confirm` written per tool.
Cost: Write, Write for good and both Settings kinds look alike to a
client, and Auto-reply like Send, so the schema dump names each tool's
kind.

### D-64 A gated tool is unregistered, never registered and refusing

A tool behind a switch is absent when the switch is off. It is never
registered and refusing.

The permanent-delete methods accept only a scope the server asks for
only under the destructive switch (D-52). A registered delete tool would
therefore fail every call by default, which is two defects: a tool that
cannot work, and a scope nobody asked for. A registered send tool is
exactly the control an injected instruction gets to argue with. A client
that runs with nobody watching gets none of the gated tools.

The download tools follow the same rule: they register only when
`GMAIL_LOCAL_DIR` is set (D-25).

Rejected: a tool registered in every mode that refuses each call while
its switch is off.
Cost: changing what registers means changing the configuration, and for
the destructive switch a fresh `login`.

### D-65 The server asks the person nothing; per-call approval is the client's

The server puts no question to the person. Approval of each call is the
client's.

The MCP specification puts consent on the host. Clients "SHOULD prompt
for user confirmation on sensitive operations"
([tools](https://modelcontextprotocol.io/specification/2025-11-25/server/tools)).
It lets a server ask through elicitation, but never makes the answer
consent: the client gathers it "from the user or other sources"
([elicitation](https://modelcontextprotocol.io/specification/2025-11-25/client/elicitation)).
D-104 records why asking was declined.

What holds on every client is what the server itself enforces:
registration (D-64), `confirm` (D-66), the guards and witnesses (D-47,
D-49), and `dry_run` (D-43). forbidigo refuses both ways of asking,
`ServerSession.Elicit` and an input-required result. The test harness's
client declares elicitation and fails any call that asks, so every tool
test holds the server to asking nobody.

Rejected: a question the server puts to the person before a write
(D-104).
Cost: a client that runs with nobody watching makes every registered
write. Leave the send, destructive and settings switches off for one.

### D-66 `confirm` is a witness on every write that cannot be undone, never approval

Every write that cannot be undone takes `confirm: true`: every delete,
`create_trash_filter`, and `set_vacation` turning the reply on. It is a
witness that the caller meant the call, never approval, since the model
sets it.

`register` refuses three things at start. A kind that cannot be undone
must take `confirm`. A tool that takes `confirm` must be annotated
destructive. A Write must not take it, since another write undoes a
Write. Each tool's service refuses a call that leaves it out, in its own
words.

The descriptions and the server instructions say to pass it only when
the person asked for that write. A model that passes it has read that
sentence, which is all a witness can show.

Rejected: `confirm` on a Write, which another write undoes, so the
witness would be noise on every routine call.
Cost: the model sets it, so it is never approval, and each service
refuses its absence in its own words rather than in one place (#52).

### D-67 `destructiveHint` is true on every kind but Read, Read (local write) and Adding

The MCP schema reads `destructiveHint: false` as "the tool performs only
additive updates", and the default is true
([ToolAnnotations](https://modelcontextprotocol.io/specification/2025-11-25/schema)).
So every kind is annotated destructive except Read, Read (local write)
and Adding. That covers a write that changes or removes what is there,
one that cannot be undone, a send, the vacation reply and every
settings write. `readOnlyHint` is on the Read kinds alone, and
`openWorldHint` is true only where the call reaches another person.

Clients decide their prompts from these. claude.ai and Codex prompt by
`destructiveHint`. VS Code ignores it and prompts on every tool without
`readOnlyHint`. The README's Claude Code example allows the server's
tools and asks for each one that sends, changes a setting or deletes for
good. `staleness` holds that example to every tool annotated
destructive outside the Write kind.

Rejected: `destructiveHint: false` on a write that can be undone, which
the schema does not mean.
Cost: claude.ai and Codex prompt for every write but an add, and VS Code
for every tool without `readOnlyHint`.

### D-68 Only `delete_permanently` and `delete_label` carry the mark

`delete_permanently` and `delete_label` carry Claude Code's
`anthropic/requiresUserInteraction`. The first skips the trash Gmail
offers, and the second takes a label off every message. Neither can be
undone.

Claude Code then prompts on every call in every interactive mode,
whatever the operator's rules say, and denies the call where no prompt
can be drawn. No other client reads the mark. It is for tools whose
prompt is the point, so it goes only on the Destructive kind.

A send is none of those cases: the person confirms its recipients by
name (D-49), and the operator chose to enable it. A delete done many
times in a row, as `delete_draft` is, would bring back a prompt on every
call. Everything else is the operator's to decide, with Claude Code's
allow and ask rules.

Rejected: the mark on `send_draft` and `set_vacation`; the mark on
`delete_draft`.
Cost: an allow rule for `send_draft` or `set_vacation` takes effect in
Claude Code.

### D-69 No tool widens the server's own reach

No tool turns a setting on, names another directory or profile, or
writes the server's stored configuration.

The model can be persuaded, so it must not be able to widen its own
reach. What the server may do is the operator's decision, made before
the conversation, in the process's environment and flags.

A test drives every tool with its guards set and holds the environment,
the profile directory and the home directory unchanged. It also holds
that no tool takes an input named after a setting, with the names
derived from `internal/config`.

Rejected: a tool that changes the server's configuration, such as
turning sending on for one call.
Cost: a change of reach needs the operator to restart the server with
other settings, and for a scope a new `login`.

### D-70 Read-only mode registers the reads and asks for `gmail.readonly`

`GMAIL_READ_ONLY=true` registers the Read kind and asks for
`gmail.readonly`. The download tools stay when `GMAIL_LOCAL_DIR` is set
(D-25).

The difference matters here more than for most APIs. The alternative to
`gmail.readonly` is a scope that can send (D-48). `gmail.metadata` is
narrower still, but it refuses `q`
([messages.list](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.messages/list)),
which every search needs.

Read-only mode with any enable switch is refused at start, naming both.
A configuration that asks for both says two contrary things, and
starting with either would surprise whoever set the other.

Rejected: write scopes in read-only mode, to spare a second consent
when the mode changes.
Cost: read-only with any enable switch is refused at start, and leaving
read-only mode needs a new `login`.

### D-71 Login is a loopback flow with PKCE, and the token lives in the OS keyring

`login` runs Google's flow for desktop apps: a loopback redirect on
`127.0.0.1` at a random port, PKCE with S256, and `state` checked.
[RFC 8252 §7.3](https://www.rfc-editor.org/rfc/rfc8252#section-7.3)
describes the loopback redirect for native apps. There is no
out-of-band flow. `--no-browser` prints the URL and the exact `ssh -L`
line, so login works over SSH.

The client JSON comes from `--client-secret` or `GMAIL_CLIENT_SECRET`,
over the stored profile. The refresh token resolves from the
environment, then the OS keyring, then an owner-only file that warns on
every use. The environment wins so automation can run without a
keyring. The config directory override is refused outside the home
directory.

The token is the account. It belongs where the operating system guards
secrets, and where no tool can read it.

Rejected: an out-of-band flow; the keyring before the environment.
Cost: on Windows a `0600` mode protects nothing, so the file fallback
needs an access list, and it warns on every use.

### D-72 Consent is forced only when the scope set grows

`login` asks Google to show the consent screen only when the scope set
grows, or with `login --consent`.

A login without forced consent still gets a refresh token, and `login`
stores it in place of the old. So skipping consent spares the person a
screen, not a token. An account holds at most 100 refresh tokens per
client, and Google invalidates the oldest past that
([OAuth 2.0](https://developers.google.com/identity/protocols/oauth2#expiration)).
A login that gets no refresh token keeps the stored one. `login` says
whether Google issued a token.

Rejected: forcing consent on every login.
Cost: every login still mints a refresh token toward the 100 per
client.

### D-73 Scopes follow the flags, and a granted set that differs is named

`internal/scopes` is the one source of the scopes each mode asks for,
and of which granted scope covers which: `https://mail.google.com/`
covers `gmail.modify`, which covers `gmail.readonly`. It also says what
each tool needs, and generates the scope block of
[gcp-setup.md](gcp-setup.md), which `staleness` compares exactly.

Startup and `doctor` compare the granted scopes with the required ones
and name what is missing, so a switch that changes the scopes fails at
start rather than on the first call that needs them. `doctor` also
names a token wider than the configuration, such as full mail access
left after the destructive switch is turned off, and says `logout` then
`login` narrows it.

Google asks for the scope list on the consent screen only for apps used
outside a Workspace organization, so the setup page asks for it on
External apps only.

Rejected: finding a missing scope on the first call that needs it.
Cost: a token wider than the configuration stays until `logout` and
`login`.

### D-74 Every setting is read through one reader, and a removed one is refused

Every setting is a `GMAIL_*` variable with a flag bound to it, except
the config directory and the refresh token, which are environment only.
A flag overrides the environment, and the environment overrides the
default. `Define` and `Build` validate everything once at start, with
every problem reported together.

Every reader goes through `config.Lookup`, which drops the spaces around
a value and reads a value of spaces alone as unset. `config.Vars` is the
registry of every variable. A scan holds every `GMAIL_` name spelled in
`cmd/`, `internal/` and `scripts/` to it, and the configuration page and
the bundle gate read it.

A removed setting that is turned on is refused at start, naming its
replacement. `GMAIL_REQUIRE_PROMPT` is one. Ignoring it would allow what
its owner set it to stop.

Rejected: `main` wrapping the environment once, which a reader that
skips it escapes; ignoring a removed setting.
Cost: every reader goes through `config.Lookup` and into the registry,
and the scan fails a name that does not.

### D-75 The server starts and lists its tools without credentials

The server starts, serves and lists its tools with no credentials. Each
call then answers `[auth]`, naming `login`. `doctor` and
`--dump-schemas` work signed out, and the token is warmed off the
startup path.

A server that serves signed out can answer each call with what to do
next. `doctor` can then walk the setup and name the missing step, and
the schema dump, which CI runs, needs no account at all.

Rejected: refusing to start without a token, which leaves the person
no tool that says what to do.
Cost: a signed-out server lists tools it cannot run, and every call
checks for credentials before it does anything.

### D-76 `logout` revokes at Google, which ends the whole grant

`logout` revokes the refresh token at Google before it deletes the
local copy. It leaves a token from the environment alone.

Google's page says "Revocation removes all OAuth 2.0 scopes previously
granted to a project, invalidating any issued access or refresh tokens
for all clients registered under that project"
([revoking a token](https://developers.google.com/identity/protocols/oauth2/native-app#tokenrevoke)).
So a revoke signs out every profile that uses the same Cloud project.
`login` records the project from the client JSON, and `logout` compares
what was recorded and names the other profiles the revoke signs out. The
note follows a revoke that succeeded.

Rejected: reading the client file's project at logout time, since a
relative or replaced file could hide a profile the revoke signs out.
Cost: a revoke ends every token the account granted the project, on
every machine. The rule rests on Google's page, not a live probe, since
the probe costs a grant.

### D-77 Every error is `[class] message` from a closed vocabulary

Every tool error is `[class] message`. The class comes from a closed
vocabulary declared in `internal/gapi/errors.go`, where each class's
comment says what the caller should do. `internal/service` is the only
place a Google failure becomes a tool error.

The class tells the caller what to do next, so two failures that need
different actions need two classes. `stale` and `conflict` stay apart:
one says read again, the other says reconsider. `ambiguous_outcome`
exists because a send may have landed (D-50). `rate_limited` tells the
sending limit from the request limits in its message.

`make classes` holds the vocabulary from three sides: the constants, the
`Classes` list beside them, and the classes the code emits. A class
declared and never emitted fails, and so does one emitted and never
declared.

Rejected: a vocabulary of six classes, with `stale` folded into
`conflict` and no class for an outcome that may have happened; and a
table of the classes in this document, a second copy the gate had to
hold to the code.
Cost: the class list and the code change in the same commit, and a
reader finds what each class asks for in the code, not here.

### D-78 Repeatability comes from the HTTP method, and a POST is retried only when turned away

Whether a request may be repeated comes from its HTTP method. A GET is
retried. A POST is not, except `modify`, `trash` and `untrash` on a
message or thread, which are declared repeatable at the call site with
the reason: applying a label twice is applying it once. `drafts.send`
is never retried (D-50).

A status that turns a request away before Google acts is retried for
any method. That covers 429, 503, a 403 with a rate-limit reason, a
connection never made, and a filter write refused with "Precondition
check failed." once (D-59). `Retry-After` is a minimum, the backoff has
full jitter, and a call makes at most four attempts, as Google's error
guide advises backoff
([handle errors](https://developers.google.com/workspace/gmail/api/guides/handle-errors)).
Gmail's per-user unit limit arrives as a 403 `rateLimitExceeded` with
no `Retry-After`, which a live run checks.

Rejected: retrying `drafts.create`, which makes two drafts.
Cost: the sending-limit 429 is not retried, since its wait is hours;
`rate_limited` says which limit it was.

### D-79 Quota is spent in units against a per-user budget, and each result reports it

Gmail meters quota in units per user per minute, and a method's cost
runs from 1 to 100 units
([quota](https://developers.google.com/workspace/gmail/api/reference/quota)).
The client carries each method's cost and spends it against a per-user
budget of 6,000 units a minute, waiting rather than earning a refusal.
Concurrent calls share the budget.

Each tool result and each log line reports the units the call spent.
`search_threads` defaults to 20 results because 20 thread reads cost
800 units, and its description says so.

Rejected: a request-rate limiter, which is wrong by up to 100 times,
since the costs differ that much.
Cost: every method needs its unit cost in the client's table, or the
client refuses the call.

### D-80 Every request is bounded by progress and by size

Every request has a timeout, `GMAIL_HTTP_TIMEOUT`, and a response body
is capped at 32 MiB. Other servers hung on a dead socket.

A stream is bounded by time without progress, not in total. That
covers an attachment downloaded to disk and a draft uploaded. A large
file on a slow link finishes, and a stalled one fails. A whole-request
timeout on an upload would end a slow 35 MB draft as
`[ambiguous_outcome]`, and a retry could make a second draft.

`attachments.get` answers JSON, so the client reads it token by token up
to `data` and decodes that string as it arrives, never holding it whole.
It stops at 64 MB, and an attachment declared larger is refused before
it is read.

Rejected: a whole-request timeout on a stream or an upload.
Cost: a body cut anywhere is a failed read, and a retried read starts
the file over.

### D-81 A credential goes only to Google's origins, checked on every redirect

`internal/gapi` keeps an allow-list of Google's origins, port included.
It checks the list before any credential is attached, and again on
every redirect. An HTTP client injected for tests keeps the list.

The access token is the account. An answer can name any host in a
redirect, and a client that follows it with the token attached hands
the account to that host. Checking the origin where the credential is
attached closes that for every method, including one added later.

Rejected: following a redirect wherever Google's answer points, with
the credential attached.
Cost: a Google endpoint outside the list fails until it is added to the
list.

### D-82 A reply carries both halves, never the same bytes twice

Every reply carries both halves: `structuredContent` under the tool's
output schema, and a readable rendering in `content`. The SDK is given
`Content`, so it does not add the JSON as a second text block.

The two halves serve different readers. A program reads the fields. A
model reads the rendering, which carries the boundaries (D-09) and the
budget (D-14). The same JSON twice would spend the model's context on
bytes it has already been given.

Rejected: the same bytes twice, in `content` and in
`structuredContent`.
Cost: every read carries its mail text twice, in `content` and in
`untrusted_text`, a released field (D-84). The listing budget counts
both copies.

### D-83 An unknown value is left out, never sent as a zero

A message's `date` and a thread row's `latest` are left out of the
structured result when unknown. An `internalDate` of zero reads as no
date, and the `Date` header stands in.

A zero sent as a value is a date a program believes. Go's zero time
reads as the first year of the calendar, and the Unix epoch as 1970.
No mail Gmail holds dates from 1970, so a zero `internalDate` is a
missing value, and nothing is shown as sent then. The text half says
"unknown date".

Rejected: Go's zero time; the Unix epoch as a date.
Cost: the schema allows null, though the server omits rather than nulls,
as the optional settings and search times already do.

### D-84 The released tool surface is a contract held by a committed baseline

`testdata/schema-baseline.json` is the released surface: every tool,
resource and resource template, dumped whole with `_meta` and the output
schema. `schema-diff` fails on a removed tool, or an input or output
field lost at any depth. It also fails on a type change that breaks a
caller, a new required input, an input enum that loses a value, or an
output field that stops being required. The dump carries no `GMAIL_` setting.

The baseline is the CHANGELOG's newest release, stamped with its
version. With nothing under `[Unreleased]`, the build must match it
exactly. `schema-baseline` refuses a break unless the release is a new
major version.

In CI, `schema-ack` compares the pull request's head with its
merge-base, and needs every changed item named in a commit footer.
`SCHEMA-CHANGE:` is read in any case, and `BREAKING CHANGE:` only in
capitals, as
[Conventional Commits 1.0.0](https://www.conventionalcommits.org/en/v1.0.0/)
items 15 and 16 say.

Rejected: a baseline read from the last tag, since CI's checkout is
shallow and has no tags.
Cost: a break needs a major release and a **Breaking:** entry naming the
tool.

### D-85 Instructions and descriptions are built from the configuration

The server instructions are built from the tools actually registered,
so a read-only server and one that sends say different things. Four
descriptions depend on the settings: `get_settings` and `list_filters`
by which writes exist, `create_filter` by whether `create_trash_filter`
does, and `create_draft` by `GMAIL_DRAFT_SIGNATURE`.

Text that names a tool not registered sends the model to a call that
fails. Text that says a write exists when it does not is worse. The
schema dump carries every sentence of the instructions, and tests hold
each description to its switches.

Rejected: one fixed wording for every configuration.
Cost: `--dump-schemas` prints one wording of each description that
depends on the settings, so a change to another wording passes
`schema-ack` unseen (#51).

### D-86 Resources carry the same text as their tools

`gmail://threads/{id}` and `gmail://messages/{id}` carry the same text
as `get_thread` and `get_message`, under the same budget and
boundaries. `gmail://labels` carries `list_labels`. Resource reads are
logged and counted like tool calls.

A client that attaches a resource gives the model the same marked,
bounded text a tool returns. A rendering of its own would be a second
place to get the boundaries wrong. The resource templates are part of
the released surface, and the schema diff holds their URIs as it holds
the tools (D-84).

Rejected: a rendering for resources of their own.
Cost: a resource takes no options, so a read that needs one, such as an
offset or every header, needs the tool.

### D-87 The live driver reads only what it wrote and reaches nobody by default

`scripts/livemail` drives the shipped binary against a real mailbox. It
creates a label named for the run and inserts its own messages with
`messages.insert`. Every read it makes is constrained to that label,
and it never searches the mailbox unconstrained. At the end it trashes
what it inserted and deletes its label, unless `-keep` is set. `-clean`
removes a stranded run by its name.

A real mailbox holds other people's mail, which the run has no reason
to read and every reason not to print. The driver's guard refuses a
read outside the run and holds every write to the run's own mail.

A step that sends goes only to the address the maintainer passes as
`-send-to`, and none sends without it. The settings steps save the
signature and the vacation reply first and restore both exactly. Their
filters match only the run's own sender at a domain that never
resolves, the vacation reply starts far in the future, and a signature
Gmail will not take back unchanged is never changed.

Rejected: an unconstrained search of the mailbox.
Cost: the sending steps need the maintainer's second address, so a run
without `-send-to` skips them.

### D-88 The live transcript goes through one redacting writer

Everything the live driver prints goes through one writer, which
redacts. The `transcript` gate holds that the drivers cannot reach
`os.Stdout`, `os.Stderr` or `log.*`. Exactly one exempt package may
write, at exactly one site.

A transcript is read by a person and can be pasted into a pull request.
Nothing in it may identify the account (D-04). The redactor masks
addresses, history ids, label and filter ids, the system's temporary
directory, and the Cloud project number in the `Received` header Gmail
stamps on inserted mail. `-raw` turns redaction off, for a terminal
nobody else sees.

Rejected: redaction at each print site, where one forgotten site leaks.
Cost: the redactor masks by shape, so an id of a kind it does not know
prints in the clear until a rule is added for it.

### D-89 A write-path change counts only after a live run whose transcript is read

A change to the write path, or to how the server reads an API response,
counts as done only after a live run whose transcript a person read.

Every fake in the unit suite is written from a belief about Gmail, so a
unit test asserts the behavior the code was written to have. Only
Gmail can show the belief wrong. `make live` drives the shipped binary
against a real account.

`make live-cover` runs in `make check`. It records at run time, through
the stdio client, every tool option the driver's steps pass, and fails
on one no step drives. A waiver in `testdata/live-waivers.tsv` carries a
reason, the waivers have a ceiling, and a waiver whose option a step
drives fails. How to run a live check is in
[CONTRIBUTING.md](../CONTRIBUTING.md#the-live-run).

Rejected: green gates as done.
Cost: a run needs a real account, and a person reads the transcript,
which no gate can hold. Some checks need more than one account or a
real send and are not run yet (#53).

### D-90 Evals score the calls and the mailbox, with planted instructions

`scripts/evals` gives tasks to a model through the claude CLI, fenced to
this server's tools. The real server answers over streamable HTTP on the
loopback interface, against a fresh `gmailtest` mailbox per trial. A
task is scored on the calls the model made, its answer and the mailbox
afterward.

What the model says it did is not evidence. Some tasks carry a planted
instruction in their mail. Obeying it, by the tool it asks for or by
its address in any argument, is counted apart from an ordinary failure.
A task passes only when every trial does, and a run the CLI stopped is
reported rather than scored.

`make evals-check` runs in `make check` and needs no model. Every canned
transcript must get its expected verdict, and every task must fail on a
mailbox nobody touched.

Rejected: scoring what the model says it did.
Cost: runs spend money and are run by hand.

### D-91 A rule is made to fail: a test, a list derived from the code, a floor

A rule is a test, its list is derived from the code, and its checker
asserts a floor on how much it read. Every check the repository runs on
itself is Go under `scripts/gates`, behind one registry.

A checker that reads nothing passes, and so does a list kept by hand
beside the code it describes. So the error classes come from the
declared constants, the variables from `config.Vars`, the tools from the
binary, and the methods from the discovery document. A gate with a
floor fails when it read fewer files, rows or names than the floor, and
a test holds the floor.

Rejected: a list kept by hand beside the code, which passes while the
code moves.
Cost: every gate carries a floor, which a smaller repository has to
lower on purpose.

### D-92 `make check` and CI run the same set of gates

`make check` and CI run the same set of gates. Each registry entry
declares where it runs: by hand, in `make check` and CI, in CI alone,
or in the release. A missing declaration is refused.

The two lists live in different files, and a contributor edits one of
them. `parity` maps each Makefile target to the CI step that runs it by
its recipe, with Makefile variables expanded and `run: |` blocks read. A
step's name is not counted as running it. Each build tag a `//go:build`
line needs has a `go vet` pass on both sides, so tagged code compiles in
CI and not only on a laptop. A tool `ci.yml` runs with `go run` is one
`make check` runs at the same version.
Only what a push to `main` runs counts: a job or step a pull request
alone runs, or whose failure passes, does not. CI calls a gate through
its make target, so both sides run one recipe. A recipe or a CI line
that lets a failure pass is refused: a `-` prefix, `.IGNORE`, `|| true`,
a last `; true`, `&`, `!` or `set +e`. The Makefile is read as make
reads it, and a line the reader does not read is refused by name.
`checklist` holds the definition of done in `CLAUDE.md` to the `check:`
prerequisites, both ways.

Rejected: a Makefile comment claiming to be everything CI runs, which
stops the next person checking.
Cost: a gate added to one list fails `parity` until it is in the other.

### D-93 A release is reproducible, signed, attested, and refused unless CI is green

`release.yml` refuses a tag whose CI run is not green, and runs the tests
before it signs anything. It builds one target twice and compares the
hashes, which is what proves the build reproducible. goreleaser builds
with `-trimpath` and the commit's timestamp, and runs `go mod download`,
never `tidy`.

The checksum file carries a keyless cosign signature. Each archive has
an SBOM, and build provenance names the archives, the checksum file and
the bundle. A person can verify every artifact from outside, without
trusting the page it came from.

The `release` gate holds `.goreleaser.yaml` against the release workflow
on every commit, and `make goreleaser-check` validates the config. The
attest step sits in each job that runs goreleaser, in any form, and that
job grants `id-token` and `attestations` write. Its subject-path names
each file the release writes, and nothing else. The
steps are in [release.md](release.md).

Rejected: trusting a tag alone, with no check that its commit passed CI.
Cost: the steps that need an OIDC token run only in a real release, so
no rehearsal reaches them.

### D-94 The registry entry's hash is read only after the checksum signature verifies

The registry entry is published by its own workflow, after the release.
It runs `cosign verify-blob` on the checksum file, with the
certificate identity pinned to this repository's release workflow at
the exact tag, before it reads the bundle's hash.

A hash read from a file nobody verified vouches for whatever the file
says. The entry is built from the manifest the bundle gate reads, and
only once that gate passes on it. The checksum file must hold exactly
one bundle row, and the entry is validated against the vendored
registry schema.

Rejected: reading the bundle's hash before the signature is checked.
Cost: an entry cannot be taken back, so a prerelease tag skips the
step.

### D-95 Every tool is pinned to one exact version everywhere it is named

Every tool the build, CI and the release run is pinned to one exact
version, and each action to a full commit SHA with its version in a
trailing comment, and each container image to its digest. The Go version
comes from `go.mod`, its toolchain line before its go line, as setup-go
reads it.

A tool named in two places at two versions passes in one and fails in
the other. The `pins` gate parses the workflows as YAML and classes every
action as an installer or not, failing on one it does not know. An
installer's tool pin is required. A Makefile variable that pins a tool
is the one its recipe runs. The Makefile's goreleaser pin must equal the
workflow's, and the pre-commit and CI gitleaks versions must match.
It reads every `uses:` off the parse, wherever YAML lets one sit, input
and env version names in any case, and a version a reusable workflow is
given. A `go-version` beside `go-version-file` is refused, and every
level of a workflow runs bash. A known tool's module@version is a pin on
any line of the workflows, the Makefile, the git hook,
`.goreleaser.yaml` and the Go under `scripts/`. A recipe or the hook
that runs a known tool from the PATH, or fetches whatever release is
newest, is refused. The goreleaser config's commands are read as
goreleaser renders them, by a reader checked against the version the
release runs. A line break YAML parsers do not all read alike is
refused. Each pin is checked against upstream when it is set.

Rejected: a pin copied from another project, which trailed upstream.
Cost: a version bump touches every file that names the tool, in one
commit.

### D-96 The bundle defaults every switch off and passes settings through `env` alone

The Claude Desktop bundle asks for what a person sets in another
client's configuration: the OAuth client file, the profile, read-only
mode, the send, destructive and settings switches, the signature default
and the attachment folder. Each switch defaults to off, the server's own
default, and every other field to empty.

Claude Desktop approves per tool from its annotations, and a person can
choose "Always allow to skip the prompt for that tool from now on"
([connectors](https://claude.com/docs/connectors/getting-started)).
There is no per-call rule behind that. So a switch left off is the one
guard a bundle install keeps.

Settings reach the server through `mcp_config.env` alone. The reference
host takes an override's `args` or `env` in place of the base's
([config.ts](https://github.com/anthropics/mcpb/blob/v2.1.2/src/shared/config.ts)),
so the base `args` is `[]` and an override carries neither. The bundle
asks for every variable in `config.Vars` unless one is left out with a
reason, and the gate starts every set of switches from the defaults.

Rejected: a `tools` list in the manifest, which is display only and
wrong for most installs; `args` or `env` on an override; an optional key
with no default, which reaches the server as unsubstituted text.
Cost: a person turns each switch on in the bundle's settings.

### D-97 The bundle manifest is held to its vendored schema and read once

`packaging/mcpb/manifest.json` is validated against the schema vendored
for its own `manifest_version`, with a floor on the vendored file's
properties. Its `$schema` must be a whole URL at a full tag or SHA.

The gate, the packer and the registry entry read the manifest once,
through one function. A key repeated in any object, or two keys equal
but for case, is refused before any other check. Go's typed decode
merges a repeated object and matches keys without regard to case, while
the schema validator, the packer's stamp and a JavaScript host keep the
last and read a key only as spelled. One document would mean two
things.

Every command must start with `${__dirname}/`. The
[manifest specification](https://github.com/anthropics/mcpb/blob/v2.1.2/MANIFEST.md)
says it "is replaced with the absolute path to the extension's
directory", and says nothing of what a relative command resolves
against. The packer stamps the version by decoding and encoding, and
holds the result to the schema.

Rejected: a relative or absolute command; text substitution when
stamping.
Cost: a vendored schema says nothing about upstream until
`make schema-refetch` runs.

### D-98 `watch` pushes changes through Cloud Pub/Sub

`users.watch` could push changes as they happen. It is not used.

`watch` delivers notifications to a Cloud Pub/Sub topic
([push](https://developers.google.com/workspace/gmail/api/guides/push)).
A stdio process owns no topic, and a person's own client brings no
infrastructure to receive one. Building that would make this a hosted
service, which D-01 declines. `list_changes` reads the same history a
notification would point to, and says when its cursor has expired, so
nothing a push would carry is out of reach.

Rejected: `watch`, since it delivers to a Pub/Sub topic and a stdio
process owns none.
Cost: `list_changes` polls `history.list` (D-19), so a change shows only
when the model asks for it.

### D-99 A write acts on every result of a query

A write could take a search query and act on every result: archive
everything from a sender, empty the trash, relabel by query. None does.

That is how a single injected instruction empties a mailbox. A write
that names its ids acts only on mail the model has seen in a result
(D-44). Gmail's `batchModify` takes 1,000 ids, and the server still
offers neither the query nor that size.

Rejected: a write over the results of a query, because one persuaded
call would reach every message the query matches.
Cost: there is no "archive everything from X", no "empty trash" and no
relabel by query. The model searches, reads the ids, and passes at most
100 a call.

### D-100 The server imports or inserts mail

The server could import or insert mail into the mailbox. It does not.

`messages.import` and `messages.insert` deliver mail that was never
sent, under whatever sender the message names, and `messages.insert`
skips the classification a received message gets. A persuaded call could
plant mail that reads as if someone the person trusts sent it. Both
methods are written off in `testdata/api-coverage.tsv` with that
reason. Moving a
mailbox is a migration job, not an assistant's.

Rejected: `messages.import` and `messages.insert` as tools, since they
deliver mail that was never sent.
Cost: `messages.insert` is used only by the live driver, for its own
synthetic mail (D-87). A migration takes another tool.

### D-101 The server administers a Workspace through service accounts and delegation

The server could administer a mailbox as an administrator does:
delegates, forwarding addresses, send-as identities and auto-forwarding.
It does not.

The discovery document says the delegate methods, creating and deleting
a forwarding address, creating, deleting and verifying a send-as
address, and `updateAutoForwarding` are only for service accounts with
domain-wide authority. `sendAs.update` is too, for any address but the
primary. This server signs in as one person, not as such an account, so
no switch could turn those methods on. Each would also redirect mail
(D-13).

Rejected: service accounts and domain-wide delegation, which this
server never holds.
Cost: delegation, forwarding addresses and send-as identities beyond a
signature are out of reach. Whether `sendAs.patch`, which
`update_signature` calls, reaches an address other than the primary is
unproven.

### D-102 A tool resolves a name to an address

A tool could turn a person's name into an address. None does.

Addresses come from the caller or from the message being answered,
whose own headers give a reply its recipients (D-41). A directory of
names belongs to a server built on the People API, which
has its own scopes. A name resolved here would also be a guess the
person never saw, and a guess is a poor way to choose who receives mail.

Rejected: a contacts lookup that resolves a name to an address.
Cost: the caller brings every address. A recipient that is only a name
is `[invalid]`, naming the argument (D-24).

### D-103 `send_message` sends without a draft

A one-shot `send_message` could send mail without a draft. There is
none, and `messages.send` is written off.

Everything that leaves this server existed first as a draft the person
could open in Gmail. The draft is where the words can be read before
they go, and `send_draft` checks the draft against what the call says
it holds (D-47). A reply drafted first is also where the server threads
it (D-39). A send with no draft would skip all three.

Rejected: `send_message`, since everything that leaves the server
exists first as a draft the person could open.
Cost: a reply that sends is two calls, `create_draft` then
`send_draft`.

### D-104 The server confirms a write by asking the person through elicitation

Before the writes that cannot be taken back, the server could put a
form to the person through MCP elicitation and wait for an accept. It
does not.

The specification gives approval of each call to the client, and never
makes an elicitation's answer consent (D-65). Most widely used servers
confirm nothing themselves. Clients answer the question with nobody
there, as a Claude Code `Elicitation` hook and Codex with full access
do. Others do not draw it, leave it hanging or cut its message short,
and no client's allow and ask rules match it. People approve almost
every permission prompt, and stop reading as the prompts repeat.

`GMAIL_REQUIRE_PROMPT`, which refused those writes on a client that
could not ask, is removed with the question. A configuration that turns
it on is refused at start (D-74).

Rejected: a form elicitation before the writes that cannot be taken
back; `GMAIL_REQUIRE_PROMPT`.
Cost: a client that runs with nobody watching makes every registered
write, so the registration switches carry the weight (D-64).

### D-105 Arguments a client stringified are decoded leniently

Some clients send an array or an integer as a JSON string. The server
could decode those leniently. It does not: an input decodes exactly as
its schema declares, and a JSON string where an array or integer is
declared is `[invalid]`.

The MCP specification says servers "MUST validate all tool inputs"
([tools](https://modelcontextprotocol.io/specification/2025-11-25/server/tools)).
The client defects still open stringify only a parameter whose schema
is empty, or an object declared through `$ref`, and this surface has
neither. [RFC 9413](https://www.rfc-editor.org/rfc/rfc9413) describes
how tolerating a peer's error entrenches it. The schema stays an exact
contract.

Rejected: decoding a stringified array or integer, since the defects
that do it do not reach this surface and tolerance would entrench them.
Cost: a client that stringifies arrays fails here. Adding tolerance
later is not a breaking change, so the decision can be revisited.

### D-106 Plain text goes out as `format=flowed`

Plain text could go out as `format=flowed`
([RFC 3676](https://www.rfc-editor.org/rfc/rfc3676)): lines of 72 with
soft breaks a reader reflows. It does not.

A reader without support "treats flowed lines as normal Text/Plain", so
it shows every break, which is the defect the format is meant to fix.
Only Thunderbird is documented to support it. Google and Apple document
nothing either way. A paragraph on one line in quoted-printable (D-28)
and an HTML version beside it (D-29) reach every reader whole.

Rejected: `format=flowed`, since a reader without support shows every
break.
Cost: how Gmail and Apple Mail draw a long plain-text line is not
probed.

### D-107 `login` revokes the refresh token it replaces

`login` could revoke the refresh token it replaces, so stale tokens do
not pile up toward the cap. It does not.

Google revokes the grant, not one token. Revocation removes "all OAuth
2.0 scopes previously granted to a project", invalidating "any issued
access or refresh tokens"
([revoking a token](https://developers.google.com/identity/protocols/oauth2/native-app#tokenrevoke)).
Revoking the old token would end the grant the new one belongs to, so
`login` would sign itself out (D-76).

Rejected: revoking the replaced token at login, which signs the new
login out.
Cost: the cap of 100 tokens per client is met only by repeated logins,
and Google invalidates the oldest past it (D-72).

### D-108 `Kind` splits into an effect and a gate

`Kind` encodes two things: what a tool does to the world, which decides
its annotations, and which switches it registers behind. Write for good,
Settings for good and Auto-reply exist because the two combine. An
effect with a set of switches could replace the vocabulary with two
fields.

It is declined. Each kind names one combination the surface has, and
`register` reads one value to decide the annotations, the gating and
`confirm` (D-63). The split would restructure every registration with
no change in behavior. Tests already drive every mode, and the smoke
gate a mode per kind, so a wrong combination fails either way.

The argument for splitting before the kinds grow is fair. It is the one
to revisit if a kind appears only because effect and switch disagree.

Rejected: splitting `Kind` into an effect and a set of switches.
Cost: a kind exists for each combination of effect and switch, and the
vocabulary grows with them.

Next ID: D-109
