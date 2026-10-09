# Common development tasks. `make parity` holds `check` against
# .github/workflows/ci.yml; this comment does not.

GO        ?= go
# .exe on Windows, where a file without one cannot be executed.
EXE       := $(if $(filter Windows_NT,$(OS)),.exe,)
BIN       ?= ./google-mail-mcp$(EXE)
VERSION   ?= dev
PKG        = github.com/mmedum/google-mail-mcp/v2
LDFLAGS    = -s -w -X $(PKG)/internal/version.Version=$(VERSION)
# The same list CI's coverage step uses, cmd/ included.
COVERPKG   = ./cmd/...,./internal/...
# Where the release's binaries are, what version the bundle claims, and
# where it lands. Only `mcpb-pack` reads them; `gates release` holds
# MCPB_OUT against the path in .goreleaser.yaml.
DIST      ?= dist
MCPB_OUT  ?= dist/google-mail-mcp_$(VERSION).mcpb
# The ref a pull request's CHANGELOG entry is measured from.
CHANGELOG_BASE ?= origin/main
CHANGELOG_HEAD ?= HEAD

# The repository's own checks, one command with one registry.
GATES     ?= $(GO) run ./scripts/gates

# Every tool pinned and fetched the way CI fetches it, never whatever is
# on the PATH. A distribution's golangci-lint built with an older Go
# refuses this module and reports it as "can't load config".
GOLANGCI_LINT ?= github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
GOVULNCHECK   ?= golang.org/x/vuln/cmd/govulncheck@v1.8.0
# v1.6.0 until docs/architecture.md §17.3 settles v1 against v2.
GOLICENSES    ?= github.com/google/go-licenses@v1.6.0
# The module path is zricethezav: the project moved organization and the
# module path did not follow.
GITLEAKS      ?= github.com/zricethezav/gitleaks/v8@v8.30.1
ACTIONLINT    ?= github.com/rhysd/actionlint/cmd/actionlint@v1.7.12
# release.yml runs goreleaser through goreleaser-action, which installs
# its own copy. `pins` holds this version against that one: a rehearsal
# on a different goreleaser is not a rehearsal.
GORELEASER    ?= github.com/goreleaser/goreleaser/v2@v2.18.2

.PHONY: all
all: check

.PHONY: check
check: fmt vet tidy lint cover vuln licenses secrets leaks pins classes api-coverage api-fields schema-diff smoke staleness checklist changelog-links transcript live-cover outcomes evals-check mcpb release server-json actionlint goreleaser-check parity

# --- build ---------------------------------------------------------------

.PHONY: build
build: ## Build the binary
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN) ./cmd/google-mail-mcp

.PHONY: install
install: ## go install the binary
	CGO_ENABLED=0 $(GO) install -trimpath -ldflags="$(LDFLAGS)" ./cmd/google-mail-mcp

.PHONY: dump-schemas
dump-schemas: build ## Write the tool and resource schemas to schemas.json
	$(BIN) --dump-schemas > schemas.json

# --- go hygiene ----------------------------------------------------------

.PHONY: fmt
fmt: ## Fail if gofmt would change anything
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt would change:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## go vet, the tagged code included so it keeps compiling
	$(GO) vet ./...
	$(GO) vet -tags=live ./...
	$(GO) vet -tags=evals ./...

.PHONY: tidy
tidy: ## go.mod and go.sum are what `go mod tidy` would write, and match the cache
	$(GO) mod tidy -diff
	$(GO) mod verify

.PHONY: lint
lint:
	$(GO) run $(GOLANGCI_LINT) run

.PHONY: test
test: ## Unit tests with the race detector and a coverage profile
	$(GO) test -race -shuffle=on -coverpkg=$(COVERPKG) -coverprofile=cov.out -covermode=atomic ./...

.PHONY: cover
cover: test ## The per-package coverage floors
	$(GATES) coverage cov.out

.PHONY: vuln
vuln:
	$(GO) run $(GOVULNCHECK) ./...

.PHONY: licenses
licenses:
	$(GO) run $(GOLICENSES) check ./... --allowed_licenses=Apache-2.0,BSD-2-Clause,BSD-3-Clause,MIT,ISC

.PHONY: secrets
secrets: ## Credentials in the tree and in every commit the clone holds
	$(GO) run $(GITLEAKS) dir . --config .gitleaks.toml --redact --no-banner
	$(GO) run $(GITLEAKS) git . --config .gitleaks.toml --redact --no-banner

.PHONY: actionlint
actionlint: ## The workflows are valid
	$(GO) run $(ACTIONLINT)

.PHONY: goreleaser-check
goreleaser-check: ## The release config is valid
	$(GO) run $(GORELEASER) check

# --- gates ---------------------------------------------------------------

.PHONY: leaks
leaks: ## Identifiers and mail content in the working tree
	$(GATES) leaks

.PHONY: leaks-history
leaks-history: ## Every blob, commit message and tag in the history
	$(GATES) leaks history

.PHONY: pins
pins: ## Actions pinned by SHA, tool versions exact and equal
	$(GATES) pins

.PHONY: classes
classes: ## The error vocabulary, held closed from both sides
	$(GATES) classes

.PHONY: api-coverage
api-coverage: ## Every published API method has a verdict
	$(GATES) api-coverage

.PHONY: api-fields
api-fields: ## Every published field is modeled or written off
	$(GATES) api-fields

.PHONY: api-diff
api-diff: ## Refetch the discovery document and rewrite the snapshot (network; manual)
	$(GATES) api-diff

.PHONY: schema-diff
schema-diff: build ## The tool surface against the newest release's recorded baseline
	$(GATES) schema-diff $(BIN)

.PHONY: schema-baseline
schema-baseline: build ## Record the release being cut as the baseline: VERSION=vX.Y.Z, in its release commit (manual)
	$(GATES) schema-baseline $(BIN)

.PHONY: schema-refetch
schema-refetch: ## The vendored schemas against what their sources serve (network; manual)
	$(GATES) schema-refetch

.PHONY: smoke
smoke: build ## Drive the binary over stdio
	$(GATES) smoke $(BIN)

.PHONY: staleness
staleness: build ## The docs match the code
	$(GATES) staleness $(BIN)

.PHONY: checklist
checklist: ## CLAUDE.md's definition of done against `check`
	$(GATES) checklist

.PHONY: changelog
changelog: ## A pull request adds a CHANGELOG entry, unless it cuts a release
	$(GATES) changelog $(CHANGELOG_BASE) $(CHANGELOG_HEAD)

.PHONY: changelog-links
changelog-links: ## Every CHANGELOG version heading has its link reference
	$(GATES) changelog-links

.PHONY: transcript
transcript: ## The drivers print only through their redactor
	$(GATES) transcript

.PHONY: live-cover
live-cover: build ## Every tool option is driven live or waived with a reason
	$(GATES) live-cover $(BIN)

.PHONY: outcomes
outcomes: ## Every write's result states its outcome
	$(GATES) outcomes

.PHONY: mcpb
mcpb: ## The bundle manifest describes the bundle the packer builds
	$(GATES) mcpb

.PHONY: release
release: ## The release config builds, signs and uploads what the packer stages
	$(GATES) release

.PHONY: server-json
server-json: ## The registry entry generator holds the registry's rules
	$(GATES) server-json

.PHONY: parity
parity: ## `check` and ci.yml run the same things
	$(GATES) parity

# --- release -------------------------------------------------------------

# Run from the universal binary's post hook in .goreleaser.yaml, which
# calls `go run ./scripts/gates` directly. Not in `check`: it needs the
# built binaries.
.PHONY: mcpb-pack
mcpb-pack: ## Pack the .mcpb from a built dist tree (release; manual)
	$(GATES) mcpb-pack $(DIST) $(VERSION) $(MCPB_OUT)

.PHONY: release-notes
release-notes: ## Print the CHANGELOG section a tag would publish (manual)
	$(GATES) release-notes $(VERSION)

# As far as a laptop can take the release. It skips what needs the OIDC
# token only a workflow run has, and the SBOMs, which need syft. It also
# skips goreleaser's dirty-tree check. docs/release.md says what that costs.
.PHONY: release-rehearse
release-rehearse: ## Build the whole release locally, unsigned (manual)
	$(GO) run $(GORELEASER) release --snapshot --clean --skip=publish,sign,sbom

# --- by hand -------------------------------------------------------------

.PHONY: live
live: build ## Drive the built binary against a real account (docs/development.md)
	$(GO) run -tags=live ./scripts/livemail -binary $(BIN) -profile $(or $(GMAIL_PROFILE),default)

# Not in `check`: it spends money and is not deterministic. Its
# transcript is read like the live driver's.
.PHONY: evals
evals: ## Score a model against the tool surface through the claude CLI (spends money; manual)
	$(GO) run -tags=evals ./scripts/evals $(EVAL_ARGS)

# The deterministic half of the evals: each task's mailbox is built, and
# every scorer must fail on a mailbox nobody touched. No model, no key.
.PHONY: evals-check
evals-check: ## The eval scorers discriminate, with no model and no key
	$(GATES) evals-check

.PHONY: hooks
hooks: ## Point git at .githooks
	git config core.hooksPath .githooks

.PHONY: clean
clean:
	$(RM) $(BIN) cov.out schemas.json
	$(RM) -r $(DIST)
