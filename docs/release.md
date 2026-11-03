# Releasing

A maintainer releases by pushing a tag. `release.yml` builds, signs and
publishes from it, and the result is a published release, never a draft.

The tag produces the platform archives, their SBOMs and the `.mcpb`
bundle, all in `checksums.txt`, which cosign signs. The workflow attests
provenance, and `publish-mcp.yml` then publishes the MCP registry entry.

## Before the release commit

- `make check` passes, and `make live` has run with the transcript read.
- `make api-diff`. A method or field Google added fails the API gates
  until it has a verdict.
- `make schema-refetch`, the only step that checks the vendored schemas
  against what upstream serves.
- `make release-rehearse` builds the release unsigned under `dist/`, and
  `make release-notes VERSION=Unreleased` prints the release page.
- On a minor release, read what Google's hosted Gmail MCP server offers.
  If it gains trash, local attachment transfer and bounded reads, say so
  in the README's goals.

## Checks the live driver cannot make

- Install the release's `.mcpb` in Claude Desktop, after `login`, and
  call `get_profile`.
- Call a read from a second MCP client.
- When a change touches who a send reaches, run the driver with
  `-send-to` naming your second address. It sends mail.

## Steps

1. Land a release commit on `main`. It renames `[Unreleased]` to
   `## [X.Y.Z] - YYYY-MM-DD`, leaves an empty `[Unreleased]` above it,
   and runs `make schema-baseline VERSION=vX.Y.Z`, without which the
   schema diff fails. A break is recorded only as a new major version.
2. Wait for `ci.yml` to pass on that exact commit. The `verify-ci` job
   refuses a tag whose commit is not green.
3. Tag it and push the tag on its own:

   ```bash
   git checkout main && git pull --ff-only
   git tag -a vX.Y.Z -m "vX.Y.Z"
   git push origin vX.Y.Z
   ```

   Push one tag at a time: GitHub creates no tag events past the third
   in one push. If the release never starts, run `release.yml` by hand
   against the tag.

`gates release-notes` refuses a version with no CHANGELOG section. A
prerelease skips the registry, whose entries cannot be taken back.

## What no rehearsal reaches

Signing, attestation and the registry publish need a workflow's OIDC
token, so read the first run after any change to the pipeline.

| Fails | State afterward | Recovery |
|---|---|---|
| reproducible build | no release | find the non-determinism from the two hashes in the log, fix it, release the next patch |
| cosign | no release; signing precedes publish | fix it, then release the next patch |
| attestation | release published, unattested | re-run the failed job |
| registry publish | release fine, no entry | `gh workflow run publish-mcp.yml --ref main -f tag=vX.Y.Z`; do not tag again |

The rehearsal also skips the SBOMs, which need syft, so a broken
`sboms:` block fails only the tag. And `--snapshot` skips goreleaser's
dirty-tree check, so keep the `before` hook `go mod download` and the
notes file outside the checkout.

## Verify from outside

```bash
cd "$(mktemp -d)" && gh release download vX.Y.Z --repo mmedum/google-mail-mcp
sha256sum -c checksums.txt --ignore-missing
cosign verify-blob checksums.txt --bundle checksums.txt.bundle \
  --certificate-identity-regexp 'https://github\.com/mmedum/google-mail-mcp/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify google-mail-mcp_X.Y.Z.mcpb --repo mmedum/google-mail-mcp
```

`gh attestation verify` prints nothing when it passes, so read its exit
code, not through a pipe. A corrupted copy of the bundle must fail.

The version must agree across the bundle's and archives' filenames, the
bundle's `manifest.json`, `--version` and `checksums.txt`. A bundle
missing from `checksums.txt` ships unsigned and looks no different.

Last, run the README's install command outside the repository, as a user
would, and check that `google-mail-mcp --version` names the release.

## A bad release

A bad release is fixed forward.

- Never move or delete a pushed tag. The Go module proxy caches it within
  minutes, and the checksum database records its hash.
- Ship a patch release with the fix.
- In that patch, add a `retract` directive to `go.mod` for the bad
  version: `retract vX.Y.Z // why`. The go command then skips it for
  `@latest`.
- Mark the bad version's CHANGELOG heading `[YANKED]`:
  `## [X.Y.Z] - YYYY-MM-DD [YANKED]`.
