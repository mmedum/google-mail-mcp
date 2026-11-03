# Contributing to google-mail-mcp

Thanks for considering a contribution. This is a Go MCP server for Gmail
with a strict test and confidentiality posture. Read this once before
your first pull request; CI enforces the rest.

## Reporting bugs and requesting features

Open a [GitHub issue](https://github.com/mmedum/google-mail-mcp/issues/new/choose)
with the bug report template. It asks for what
[`docs/troubleshooting.md`](docs/troubleshooting.md#reporting-a-problem)
lists: `doctor` output, the version and a debug log. All of them are safe
to paste, because the logs carry no mail. Never paste mail, a tool
result or your OAuth client JSON: a result carries other people's words
and addresses.

For anything security-sensitive, such as a vulnerability or a suspected
token leak, do not open a public issue. Follow [`SECURITY.md`](SECURITY.md).

## Development setup

```bash
git clone https://github.com/mmedum/google-mail-mcp
cd google-mail-mcp
make build
make hooks
```

Go is the only prerequisite, at the version in `go.mod`. `make hooks`
points git at `.githooks/`, whose pre-commit hook runs
`go run ./scripts/gates precommit`: gofmt on the staged files, the leak
scan and gitleaks. `go run ./scripts/gates install-hooks` does the same.

## Gates that must pass

```bash
make check
```

`make check` runs the gates CI runs, and the `check` target in the
`Makefile` is the list. `make parity` fails when that list and `ci.yml`
differ, so a gate you add goes in both. CI runs the unit tests on Linux,
macOS and Windows.

Every check this repository runs on itself is Go, under `scripts/gates`,
so the code holding the gates shut is held to them too.
`go run ./scripts/gates` lists them, and `go run ./scripts/gates <name>`
runs one.

| Gate | Holds | Runs in |
|---|---|---|
| `coverage` | race tests, and an 80% floor per package scored on its own files | check |
| `classes` | every error class the code emits is declared in `internal/gapi/errors.go` and listed in `Classes`, and every declared class is emitted | check |
| `outcomes` | every write's result states its outcome, and every write has a dry run | check |
| `leaks` | no identifier from a real mailbox or account in the tree; `leaks history` scans every blob, commit message and tag | check, CI |
| `transcript` | the live driver and the evals print only through their redacting writer | check |
| `live-cover` | every tool option has a live step, or a row with a reason in `testdata/live-waivers.tsv` | check |
| `api-coverage` | every published API method has a verdict in `testdata/api-coverage.tsv`, and every client call has a row | check |
| `api-fields` | every published field of a judged schema is modeled or left out with a reason, in `testdata/api-fields.tsv` | check |
| `smoke` | the built binary over stdio, signed out, at two protocol revisions, and a clean exit on an abrupt disconnect | check |
| `schema-diff` | the tool and resource surface against `testdata/schema-baseline.json` | check |
| `staleness` | the docs against the code: the README's tool table, the settings, the scope block in `docs/gcp-setup.md`, and no version in prose | check |
| `docs` | the decision index, every `D-` citation and doc anchor, the paths and targets a doc names, the codemap and the live steps comments name | check |
| `checklist` | `CLAUDE.md`'s definition of done against `make check` | check |
| `changelog-links` | every CHANGELOG version heading has its link | check |
| `pins` | actions pinned to SHAs, and every tool pinned to one exact version wherever it is named | check |
| `parity` | `make check` and `ci.yml` run the same set | check |
| `evals-check` | the eval scorers tell a right answer from a wrong one, with no model | check |
| `release` | `.goreleaser.yaml` against `release.yml`: the bundle signed, uploaded and attested | check |
| `mcpb` | the bundle manifest against its vendored schema and the names the packer stages | check |
| `server-json` | the registry entry against its vendored schema and the registry's rules | check |
| `changelog` | a pull request adds a CHANGELOG entry, unless it cuts a release | CI |
| `schema-ack` | a pull request names each changed item of the tool surface in a commit footer | CI |
| `release-notes` | one version's CHANGELOG section, which is the release note | release |
| `mcpb-pack` | packs the bundle from what goreleaser built | release |
| `api-diff` | refetches the discovery document into `testdata/api-surface.json`, or fails and leaves it untouched | by hand |
| `schema-baseline` | records the surface of the release being cut, in its release commit | by hand |
| `schema-refetch` | compares the vendored manifest and registry schemas with what upstream serves; writes nothing | by hand |
| `precommit`, `install-hooks` | the pre-commit hook, above | by hand |

Every gate also asserts a floor on how much it read, because "found
nothing" and "read nothing" print the same line. Before you trust a gate
you changed, break what it holds and watch it fail.

These are worth knowing before they fail on you:

- **The schema diff** compares the built binary's dump with
  `testdata/schema-baseline.json`, the newest release's surface. A
  removed tool or resource fails the build, as does a field removed or
  retyped at any depth, a new required input, an input enum that takes
  fewer values, and an output field made optional. Anything else
  that moved is reported: a description, an annotation, a `_meta` mark,
  a kind, the instructions. A client decides from those whether to ask,
  so read them. The baseline moves only in a release commit.

  On a pull request, CI also runs `schema-ack`. When the tool surface
  changes, annotations included, a commit footer has to name each
  changed item: `SCHEMA-CHANGE: modify_labels, trash: destructiveHint
  true`. The names are the list before the first colon followed by white
  space; the text after it describes the change and names nothing. Name
  a tool by its name, a resource or template by its URI, a prompt by its
  name, and the instructions as `instructions`. Put a name with a comma
  in it in backticks, and keep the list on one line: a wrapped line that
  starts `word: ` begins another footer, as Conventional Commits reads
  it. A `Token #value` footer such as `Closes #12` acknowledges nothing.
  If the change breaks something a release shipped, name that item in a
  `BREAKING CHANGE:` footer instead. A refusal prints the footer that
  fixes it; add it in an empty commit, never an amend.
- **`make leaks`** fails on anything shaped like a real address, a
  Gmail web URL, a mail id after its keyword, or a Google OAuth
  credential. Write fixtures as `janedoe@example.com`. Run
  `make leaks-history` before anything goes public.

  **What it cannot catch is the payload.** A subject or a body is
  ordinary words. Two habits cover the gap, and only habits can:
  fixtures are generated by `internal/gapi/gmailtest`, never recorded,
  and the live driver reads back only what it wrote.
- **The docs gate** (`make docs`) holds the docs to their references. A
  decision in `docs/architecture.md` is `D-NN`; an ID is never reused,
  and the file ends with its `Next ID:` line. Cite a decision by ID, or a
  doc by `docs/<file>.md#<heading-slug>`, never by section number. What
  you verified about Gmail goes in the comment at the line that depends
  on it, with the source link and the live step that checks it, never in
  a file of its own.
- **`make classes`** holds the error vocabulary closed from both sides.
  Change the code and the table in `docs/architecture.md` in the same
  commit.
- **Renderer goldens.** `testdata/golden/` holds what each renderer
  prints, from the generated mailbox only. `go test ./internal/render
  -update` rewrites them; read the diff before you commit it.

## The live run

Every fake in the unit suite is written from what we believe Gmail does,
and only a live run tests that belief. Anything touching the send path,
MIME building or an API response shape needs one before it counts, and
the transcript has to be read: a pass count cannot show a result that is
confidently wrong.

```bash
make live
```

It drives the built binary against the signed-in profile
(`GMAIL_PROFILE`, or `default`). What it does there:

- **Its own mail only.** It creates a label named for the run and
  inserts its own synthetic messages under it with `messages.insert`.
  Every read is constrained to that label, and a guard refuses an
  unconstrained search or a read of an id it did not insert.
- **Cleanup.** At the end it trashes what it inserted and deletes the
  label, unless `-keep` is set. `-clean <run>` removes what a stranded
  run left, by the run's name.
- **The gated tools.** It starts the server with sending, permanent
  deletion and `GMAIL_LOCAL_DIR` on. The delete steps run only when the
  profile's login granted `https://mail.google.com/`, and the settings
  steps only when it granted `gmail.settings.basic`; otherwise they are
  skipped, saying so.
- **Your settings come back.** Before the settings steps it saves the
  default address's signature and the vacation reply, to a file as
  well, and restores both after. Its filters match only the run's own
  sender address, which no mail comes from.
- **Nobody is reached unless you ask.** Nothing is sent unless
  `-send-to` names the maintainer's second address. Then one
  `send_draft` step sends to it, and the sending spikes, which need a
  flag of their own, send to nothing else.
- **No questions.** The driver is a client that can be asked, and the
  server asks nobody (D-65). A question is declined, unread, and fails
  the step that drew it.
- **Reading it.** Every line goes through one redacting writer;
  `-raw` turns redaction off for a terminal nobody else sees. `-run
  <pattern>` runs only the steps whose names match.

`make evals` is run by hand too, because it spends money. It scores a
model driving the tools against the in-memory mailbox, through the
`claude` CLI signed in as you.

- The real server is served on the loopback interface over streamable
  HTTP, so the model reads the descriptions, schemas, instructions and
  refusals that ship. Only the transport differs from the binary.
- The CLI is fenced: every built-in tool is off, the run ignores your
  settings, hooks and other MCP servers, and starts in an empty
  directory.
- Each trial gets its own generated mailbox. A task is scored on its
  calls, its answer and the mailbox afterward, never on what the model
  says it did.
- Some tasks carry an instruction planted in a message. Obeying it, by
  the tool it asks for or by its address in any argument, is reported
  apart from an ordinary failure.
- `EVAL_ARGS='-trials 5 -v'` runs each task five times and prints every
  call; a task passes only when every trial does. `-model`, `-effort`,
  `-budget` and `-task` take the rest.
- A trial the CLI stopped, on turns or budget, is `UNFINISHED`; one that
  never answered is `ERROR`. Neither is a verdict on the tools.

`make evals-check`, in `make check`, runs the scorers with no model and
fails if one passes on a mailbox nobody touched.

## Adding a tool

1. Add the handler in `internal/tools` through `register`, with a `Kind`
   that decides its annotations and whether it registers at all (D-63).
2. Put the logic in `internal/service`, not the handler.
3. Add it to the README's tool table, or `make staleness` fails.
4. Add a step to `scripts/livemail`, or `make live-cover` fails.
5. Read `make schema-diff`, and add the `SCHEMA-CHANGE:` footer above.

Every API call is a `gapi.Call` literal carrying the discovery method id.
`make api-coverage` fails on one with no row in
`testdata/api-coverage.tsv`. Add the method's unit cost to the table in
`internal/gapi/units.go`, or the client refuses the call. A wire field
in `internal/gmail` needs a row in `testdata/api-fields.tsv`, and
`make api-diff` brings a field Google adds in as a failure naming it.

## Branch and commit conventions

- `main` is released code and takes pull requests only. Work on a short
  topic branch.
- Write the subject in the imperative, under 72 characters, saying what
  changes. Look at a recent `git log` for the tone.
- The body says the problem, why this change solves it, and the
  alternatives rejected. A fixed review finding is one line at the end
  of the body. A declined one goes in the pull request, or becomes a
  present-tense comment at the line it protects.
- Never commit directly to `main`. The maintainer pushes tags and
  releases.

## Pull requests

1. Open against `main`, with a summary and a test plan.
2. CI must be fully green.
3. A maintainer reviews and merges.

## Changelog

Every user-visible change gets an entry under `## [Unreleased]` in
`CHANGELOG.md`, in the same pull request that makes the change. Leave out
changes only contributors see, unless they must act on them.

- **Sections** are the [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
  set and nothing else: Added, Changed, Deprecated, Removed, Fixed and
  Security, in that order.
- **One line per change, 25 words or fewer, impact first.** Write for
  someone deciding whether to upgrade: what changes for them, not the
  mechanism. The investigation belongs in the commit message.
- **Breaking changes** start with `**Breaking:**`. Such an entry may add
  one sentence saying what to do. Anything needing a new OAuth consent,
  a fresh login or a config change is breaking.
- **Security fixes** name the CVE or GHSA.

`release.yml` lifts the matching section verbatim into the GitHub release
notes, so the entry is the release note. Versioning follows
[SemVer](https://semver.org/spec/v2.0.0.html), read through the tool
surface: tool names, input and output schemas, resources and documented
behavior.

## Releasing

Maintainers release as [`docs/release.md`](docs/release.md) says.

## Code style

- Match the surrounding code. Do not reformat what you did not touch.
- Comments explain why: a constraint, an invariant, something Gmail does
  that the code has to work around. Do not narrate what the code does.
- Mail is data. It reaches the model inside the marked boundaries, never
  in the server's own words (D-09, D-10).
- Logs record the call, never the payload (D-03).

`CLAUDE.md` has the short version of the rules, and `docs/architecture.md`
the reasoning. Start there if you are wondering why the code is the way
it is.

## License

By contributing, you agree that your contributions will be licensed
under the project's [Apache 2.0 License](./LICENSE).
