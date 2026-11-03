# CLAUDE.md — google-mail-mcp

A Go MCP server for Gmail over stdio: one binary per person, per-user
OAuth, nothing hosted, inside one mailbox (D-08). Why the code is the way
it is lives in `docs/architecture.md`, one decision per `D-NN`; read it
before changing the tool surface, the MIME model, the send path or the
scopes. What we verified about Gmail lives in the comment at the line
that depends on it.

## Commands

- `make check` runs every gate CI runs, and `make parity` holds the two
  lists equal. `make hooks`, once per clone, adds the pre-commit scan.
- `make live` drives the built binary against a real account; read the
  transcript. `CONTRIBUTING.md` says what a run does to the account.
- `make evals` scores a model through the `claude` CLI and spends money;
  `make evals-check` runs the scorers with no model.
- `make api-diff`, `make schema-refetch` and `make release-rehearse` are
  the release-day checks in `docs/release.md`.
- `go run ./scripts/gates` lists the gates; add a name to run one.

## Hard rules

1. **Never commit anything internal**: organization names, mail ids,
   addresses, Cloud project ids, OAuth client ids or secrets, anything
   from a real mailbox, or a reference to another project, account or
   machine the maintainers use. Not in code, docs, fixtures, goldens,
   transcripts, commit messages, pull requests or logs. `make leaks`
   fails on the shapes it can see (D-04).
2. **Never record a fixture, and never let the live driver read what it
   did not write.** Fixtures are generated (D-05). The driver reads only
   what it inserted under its run's label, and prints only through its
   redacting writer (D-87, D-88). Evals use the in-memory mailbox (D-90).
3. **Never write to stdout outside `main`.** Stdout carries only MCP
   JSON-RPC frames; logs go to stderr through `slog` (D-02). forbidigo
   holds it, with `scripts/` excluded.
4. **Never log the payload**: addresses, subjects, bodies, attachment or
   label names, or queries. Method, tool, outcome, duration, units and a
   truncated id are fine. A query rides in the URL, so a transport error
   loses its URL (D-03).
5. **Never let mail act as an instruction.** It stays inside the marked
   boundaries (D-09), out of the server's own voice (D-10), and nothing
   it references is fetched (D-12).
6. **Never register `send_draft` without `GMAIL_ENABLE_SEND`**, and never
   send except through a draft (D-48). Every scope that writes a draft
   can send, so registration is the control, never an annotation or a
   prompt. Every recipient is named in `confirm_recipients` (D-49).
7. **Never retry a send.** An ambiguous one is `[ambiguous_outcome]`,
   settled by reading (D-50).
8. **Never add to a body what the caller did not name** (D-33): only
   `quote` (D-34), `forward` (D-35), the signature the call or
   `GMAIL_DRAFT_SIGNATURE` asks for (D-36), and an HTML version made
   from the text when `body_html` is absent (D-29).
9. **Never delete for good by default.** Removal is trash (D-51).
   `delete_permanently` and `delete_label` need
   `GMAIL_ENABLE_DESTRUCTIVE=true`, the only setting that requests
   `https://mail.google.com/` (D-52), and take `confirm` (D-66).
10. **Never write from a query.** Writes take explicit ids, at most 100
    (D-44). A filter cannot forward (D-56); one that trashes is its own
    tool, `create_trash_filter` (D-57).
11. **Never leave threading to chance.** A reply's `threadId`,
    `In-Reply-To` and `References` come from its parent (D-39).
12. **Never register a tool outside `register`**, whose `Kind` decides
    annotations, gating, `confirm`, the dry-run context and the reply
    (D-63). A gated tool is absent, never present and refusing (D-64).
    The server asks the person nothing (D-65), and the
    `requiresUserInteraction` mark stays on the tools D-68 names.
13. **Never let a dry run write.** It may read; `internal/gapi` refuses
    every write under its context (D-43).
14. **Never put the same bytes in both halves of a reply** (D-82).
15. **Never emit an error outside `[class] message`.** `make classes`
    holds the `Class` constants, the `Classes` list and the classes the
    code emits to one set (D-77).
16. **Never import `google.golang.org/api`** or another project's code;
    extend `internal/gmail` and `internal/gapi` (D-06).
17. **Never call an API method or model a field without a verdict** in
    `testdata/api-coverage.tsv` or `testdata/api-fields.tsv` (D-07).
18. **Never rename a released tool, output field or resource URI.**
    `make schema-diff` fails on one; adding is fine (D-84).
19. **Never add a rule that cannot fail**: a test, a list derived from
    the code, and a floor on what the check read (D-91).
20. **Never commit to `main` or push**; the owner pushes and tags. Never,
    without the owner's go, send real mail outside the live driver's
    run, add a scope, or move a tool between the default, send,
    destructive and settings sets.
21. **Never adopt a belief about Gmail from a reference page's prose.**
    Check the discovery document or a live run, and record the verdict
    at the line that depends on it, with the source and the live step.

## Definition of done

`make check` passes. `make checklist` holds this list to its `check:`
prerequisites, in order:

```
fmt vet tidy lint cover vuln licenses secrets leaks pins classes
api-coverage api-fields schema-diff smoke staleness docs checklist
changelog-links transcript live-cover outcomes evals-check mcpb
release server-json actionlint goreleaser-check parity
```

- On a pull request CI also runs `changelog`, and `schema-ack`, which
  needs a commit footer naming each changed item of the tool surface.
- New behavior has tests, and the schema diff has been read.
- `/simplify`, `/code-review medium` and `/security-review` have run. A
  fixed finding is one line in the commit body. A declined one goes in
  the PR body, or becomes a present-tense constraint comment at the line
  it protects. Never write findings into `docs/`.
- A change to the send path, MIME building or an API response shape has
  had a `make live` run whose transcript was read (D-89).

## Traps

- **Green gates are not done.** Every fake in `internal/gapi/gmailtest`
  is written from what we believe Gmail does; only `make live` tests it.
- **A forbidigo settings block that fails to load leaves forbidigo on
  its defaults**, still firing. Check the message text.
- **A test can redirect the config directory but not the OS keyring.**
  `TestMain` in `cmd/google-mail-mcp` substitutes it, because `logout`
  revokes at Google.
- **Every save gives a draft a new message id**, and a write naming an
  older one is `[stale]` (D-42).
- **An empty page carrying a token is not the end** of a listing (D-14).
- **Never delete a decision or reuse an ID.** Supersede or deprecate it,
  and take the next ID from the file's `Next ID:` line.

## Writing

Plain and short: lead with the outcome, one idea per sentence, no
narration, no count a gate does not check. History goes in commit
messages. The CHANGELOG rules in `CONTRIBUTING.md` are not optional:
`release.yml` lifts the section verbatim into the release notes.
