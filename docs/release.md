# Release runbook

`main` is released code and the tag is the maintainer's. This file is
release day; `docs/development.md` is every other day.

What the tag does is in `.goreleaser.yaml` and
`.github/workflows/release.yml`: six platform archives, `checksums.txt`,
an SBOM per archive, a keyless cosign signature over the checksums,
`actions/attest-build-provenance`, and the `.mcpb` bundle, packed in the
universal binary's post hook so it reaches `checksums.txt` and therefore
the signature. The release then calls `.github/workflows/publish-mcp.yml`
for the MCP registry entry, which is a workflow of its own and can be run
again without another tag.

## Rehearse it

This runs the release as far as a laptop can take it, and leaves the
whole tree under `dist/`:

```
make release-rehearse                  # the whole release, unsigned
make release-notes VERSION=Unreleased  # what the release page would say
```

It runs the goreleaser the tag runs: the Makefile pins the version and
`make pins` holds it against the one `release.yml` installs, so a
rehearsal cannot quietly be of a different tool.

At tag time the `[Unreleased]` heading becomes `[X.Y.Z]` and the same
command takes that version. `gates release-notes` fails on a version with
no section, so a tag pushed before the rename stops the release before
goreleaser runs.

## Before the tag

- `make check` on the commit being tagged, and CI green on it: the
  workflow refuses to publish a commit whose `ci` run was not green.
- A live run of anything that touched the send path, MIME building or an
  API response shape, **with the transcript read**.
- `make schema-diff`, read for anything breaking.
- `[Unreleased]` renamed to the version with the date, in a release-prep
  commit, with an empty `[Unreleased]` heading left above it.
- The status line of `docs/architecture.md` says what changed since the
  last tag and is therefore unproven again. That is the list to watch.
- `make api-diff`. It refetches the discovery document; every method
  Google added since fails `api-coverage` until somebody judges it.
- `make schema-refetch`. The gates hold the manifest and the registry
  entry against **vendored** schemas, and a vendored copy says nothing
  about whether upstream still serves those bytes. This is the only step
  that looks.

## Push the tag

```
git checkout main
git pull --ff-only
git tag -a vX.Y.Z -m "vX.Y.Z"
git push origin vX.Y.Z
```

**Push tags one at a time.** GitHub drops tag events past the third in a
single push, and the release simply never runs.

## Then check the version in five places

The bundle's filename, the archive filenames, `manifest.json` inside the
bundle, the binary's own `--version`, and `checksums.txt`. Four agreeing
is what a broken bundle looks like: the bundle missing from the checksum
file ships unsigned and looks no different.

## And verify from outside

With the commands the release page itself prints:

```
sha256sum -c checksums.txt --ignore-missing
cosign verify-blob checksums.txt --bundle checksums.txt.bundle \
  --certificate-identity-regexp 'https://github\.com/mmedum/google-mail-mcp/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify google-mail-mcp_*.mcpb --repo mmedum/google-mail-mcp
```

An exit code of 0 on empty output is not evidence. Check the attestation
against a deliberately corrupted copy too — it must exit non-zero.

## What no rehearsal reaches

Three steps need an OIDC token that only a real workflow run has: the
**cosign signature**, the **provenance attestation**, and the **registry
publish**. Everything local passes while any of the three is wrong, so
read the first run after any change to the pipeline, and know the
recovery for each:

| Fails | State afterwards | Recovery |
|---|---|---|
| cosign | **no release** — signing precedes publish | fix, delete the tag, tag again |
| attestation | release published, unattested | re-run the failed job |
| registry publish | release fine, no entry | dispatch `publish-mcp.yml` with the tag |
| reproducible-build check | no release | find the non-determinism from the two hashes in the log, fix, delete the tag, tag again |

Delete a tag only if nobody can have pulled the release yet.

Three more things a rehearsal is quiet about:

- **The SBOMs.** `make release-rehearse` skips them, because they need
  syft installed. A broken `sboms:` block is green on a laptop and fails
  the tag before publish.
- **goreleaser refuses a dirty tree, and `--snapshot` skips that check.**
  This is why the `before` hook is `go mod download` rather than
  `go mod tidy`, and why the workflow writes its notes outside the
  checkout.
- **The bundle's own contents.** `make mcpb` holds the manifest against
  the staged names on every commit, and the packer's tests read back an
  archive it wrote — but until somebody installs one in Claude Desktop,
  that is a claim rather than a fact.

## The registry entry, and how to recover it

The entry is published by `.github/workflows/publish-mcp.yml`, which
`release.yml` calls after the release exists and which is dispatchable
on its own:

```
gh workflow run publish-mcp.yml --ref main -f tag=vX.Y.Z
```

The registry sends a HEAD to the bundle's download URL before accepting
an entry, so this step runs last, and a step that runs last needs a way
to run again without another release. If it fails, re-dispatch it; do not
tag again.

Before it reads a hash, the workflow verifies `checksums.txt` with cosign,
the certificate identity pinned to this repository's `release.yml` at the
exact tag. The hash in the entry is the number clients check their
download against, so it is taken only from a file whose signature held.
`mcp-publisher` itself is verified the same way before it is unpacked. A
prerelease tag skips the step: an entry cannot be taken back.
