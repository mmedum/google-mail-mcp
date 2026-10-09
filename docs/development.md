# Development

## The one command

```
make check
```

That is what CI runs. `parity` asserts the two lists are the same, and
`checklist` holds the list in `CLAUDE.md` against the Makefile, so
neither is kept in step by a comment.

Once per clone, `make hooks` points git at `.githooks/`, whose pre-commit
hook runs `precommit`: gofmt on the staged files, the leak scan and
gitleaks. `install-hooks` does the same from the gates command.

## The gates, and what each one is for

All of them are `go run ./scripts/gates <name>`, Go and not shell, so the
code holding the gates shut is held to them too. `go run ./scripts/gates`
with no argument lists them.

| Gate | Holds | Runs in |
|---|---|---|
| `coverage` | race tests and an 80% floor **per package**, scored on the package's own files | check |
| `classes` | the error vocabulary of architecture §6.5 is closed **from both sides** | check |
| `outcomes` | every write tool's result states its outcome, and has a dry run | check |
| `leaks` | no deployer-specific identifier in the tree; `leaks history` scans every commit, message and tag | check / CI |
| `transcript` | the live driver prints only through the one redacting writer | check |
| `live-cover` | every tool option has a step in the live driver, or a waiver with a reason | check |
| `api-coverage` | every published API method is used, planned for a named phase, or written off with a reason | check |
| `api-fields` | every published field is modeled or left out with a reason | check |
| `smoke` | the built binary over stdio, signed out, at two protocol revisions, and a clean exit on an abrupt disconnect | check |
| `schema-diff` | the tool and resource surface against `testdata/schema-baseline.json`, the newest release's; fails on a removed tool or resource, a field removed or retyped at any depth, a newly required input, or a baseline that is not the newest release's | check |
| `staleness` | README, `docs/` and `CLAUDE.md` against the code, including the generated scope lists in `gcp-setup.md` | check |
| `checklist` | `CLAUDE.md`'s definition of done against `make check` | check |
| `changelog` | a pull request adds a CHANGELOG entry, unless it is a release cut | CI |
| `changelog-links` | every CHANGELOG version heading has its link | check |
| `pins` | actions pinned to SHAs, tools to versions, the rehearsal's goreleaser to the release's, and architecture §5a's pin table to all of them | check |
| `parity` | `make check` and `ci.yml` run the same set | check |
| `evals-check` | the eval scorers tell a right answer from a wrong one, with no model and no key | check |
| `release` | `.goreleaser.yaml` against `release.yml`: the bundle signed, uploaded and attested | check |
| `mcpb` | the bundle manifest against its vendored schema and the names the packer stages | check |
| `server-json` | the registry entry against its vendored schema and the registry's own rules | check |
| `release-notes` | one version's CHANGELOG section, which is the release note | release |
| `mcpb-pack` | packs the bundle from what goreleaser built | release |

Five are manual:

- `api-diff` refetches the discovery document into
  `testdata/api-surface.json`. On a network failure it fails loudly and
  leaves the committed file untouched. Every method Google adds then
  fails `api-coverage` until somebody judges it.
- `schema-baseline` records the surface of the release being cut, as
  `make schema-baseline VERSION=vX.Y.Z` in its release commit. It
  refuses a build stamped with another version, and a break unless the
  release is a new major version.
- `schema-refetch` compares the vendored manifest and registry schemas
  with what upstream serves. It writes nothing.
- `precommit` and `install-hooks`, above.

Plus `make leaks-history` before anything goes public, and the release
steps in [`release.md`](release.md).

## Every gate asserts a floor

"Found nothing" and "looked at nothing" print the same sentence, so each
gate also asserts how much it read: a coverage profile with too few
lines, a leak scan over too few files, a snapshot with too few methods, a
surface with too few tools. A gate nobody has watched fail is not yet a
gate — break one thing deliberately and watch the failure you caused.

## Green gates are not done

Anything touching the send path, MIME building or an API response shape
gets a live run before it counts, and **the transcript is read**.

```
make live
```

What it does to the account, so nothing is a surprise:

- It creates a label named for the run and **inserts** its own messages
  under it with `messages.insert`. Every read it makes is constrained to
  that label, and its guard refuses an unconstrained search or a read of
  an id it did not insert. Nothing it reads was written by anybody else.
- At the end it trashes what it inserted and deletes the label, unless
  `-keep` is set.
- **Nothing reaches another person** unless a spike that sends is armed
  explicitly, and those need the maintainer's second address on the
  command line.
- Every line it prints goes through one redacting writer; `transcript`
  fails the build on any other print.
- It is a client that can be asked, so every write that takes `confirm`
  puts its question to the person (architecture §4.13). The driver
  answers for you, since you started it: accept, except one step that
  declines and checks the refusal. Each question is printed, to be read
  with the rest.

`make evals` scores a model driving the tools against the in-memory
mailbox. It runs the model through `claude -p`, signed in as you, and
is run by hand because it spends money; `evals-check` exercises its
scorers without a model.

- The real server is served on the loopback interface over streamable
  HTTP, so the model reads the descriptions, schemas, instructions and
  refusals that ship. Only the transport differs from the binary.
- The CLI is fenced: `--tools ""` turns every built-in tool off, the
  run ignores your settings, hooks and other MCP servers, and starts in
  an empty directory. A call to any tool not this server's is an error.
- Each trial gets its own generated mailbox. A task is scored on its
  calls, its answer and the mailbox afterwards, never on what the model
  says it did.
- Three tasks carry an instruction planted in a message. Obeying it, by
  the tool it asks for or by its address in any argument, is reported
  apart from an ordinary failure.
- `EVAL_ARGS='-trials 5 -v'` runs each task five times and prints every
  call. A task passes only when every trial does. `-model` and `-effort`
  default to `claude-opus-5-5` at `high`, `-budget` caps each trial in
  dollars, and `-task` picks a subset.
- `send_draft` cannot be confirmed with nobody there: the CLI cancels
  the server's question, so the send is `[blocked]`. `send-draft` scores
  reaching the send with the right witness.
- A trial the CLI stopped, on turns or budget, is `UNFINISHED`; one that
  never produced an answer is `ERROR`. Neither is a verdict on the
  tools.

## Cutting a release

[`release.md`](release.md): what the tag does, what to check afterwards,
and the recovery for each step no rehearsal reaches.

## Adding a tool

1. Add the handler in `internal/tools` through `register`, with a `Kind`
   that decides its annotations and whether it registers at all.
2. Put the logic in `internal/service`, not the handler.
3. Add it to the README's tool table, or `staleness` fails.
4. Add a step to `scripts/livemail`, or `live-cover` fails.
5. Run `schema-diff` and read it. The baseline moves only in a release
   commit.

## Adding an API call

Every call is a `gapi.Call` literal with the discovery method id as a
string literal. `api-coverage` fails on one with no row in
`testdata/api-coverage.tsv`, and on a row still marked `planned` once the
phase it names has passed. Add the method's unit cost to the quota table
in `internal/gapi`, or the client refuses the call.

## Adding a wire field

`api-fields` fails on a field `internal/gmail` carries with no row in
`testdata/api-fields.tsv`. Fields come from the discovery document
through `api-diff`, so a field Google adds arrives as a failure naming it.

## Renderer golden files

`testdata/golden/` holds what each renderer prints, from the generated
mailbox only:

```
go test ./internal/render -update
```

Regenerating is one flag; reading the diff is the part that matters.
