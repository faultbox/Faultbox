# Releasing Faultbox

The process for cutting a release. Applies to humans and agents equally.

## Release triggers

A release starts when a `release-<version>` tag is pushed to `main`.
[.github/workflows/release.yml](../.github/workflows/release.yml)
watches this tag, cross-compiles binaries for linux/darwin × amd64/arm64,
publishes a GitHub Release with tar.gz artifacts + checksums, and pushes
a multi-arch Docker image to `ghcr.io/faultbox/faultbox`.

## Pre-release checklist

Run through this list before pushing the tag. Each item is a hard
requirement — if any is red, the release is blocked.

### 1. CI is green on `main`

Check [the latest CI run](https://github.com/faultbox/Faultbox/actions/workflows/ci.yml?query=branch%3Amain).
All jobs must be ✅. This covers:
- `go build ./...`
- `go vet ./...`
- `go test ./... -race -count=1` (unit + integration, excluding testops)
- `go test ./testops/... -race` (golden-trace regression corpus)
- Cross-compile targets

### 2. Feature manifest coverage

[docs/feature-manifest.md](./feature-manifest.md) is the authoritative
record of what this release claims to do and with what confidence.

- **All Critical rows must be 🟢 green.** A red or 🟡 partial Critical
  row blocks the release. Either land the coverage in a PR first, or
  explicitly downgrade the row to Supported with a commit explaining why
  (and a linked issue tracking the regression in promise).
- **Supported rows may be 🟡 partial** but each 🔴 red Supported row
  must have an open GitHub issue linked in the `Notes` column.
- **Experimental rows** are advisory; status is informational.
- If the manifest has been modified since the previous release, read
  the diff — tier changes require a maintainer sign-off commit.

### 3. New features have manifest rows

For every feature added since the previous release:
- If Critical or Supported → there must be a matching manifest row in
  the same release cycle. No row = not shipped.
- If the feature is genuinely Experimental, mark it so in the manifest
  and in the release notes. Do not ship undocumented capability as
  though it were supported.

### 4. FINDINGS are issues, not files

The testops harness occasionally discovers product bugs. Each one must
become a GitHub issue, not a markdown artifact. Before release:
- `rg -l "FINDINGS" --type md` must return no matches, OR only
  historical references in RFCs / changelogs.
- Each open issue tagged `testops-finding` should have a triage note
  from the release owner: fix before release, or defer with a visible
  skip reason in the testops registry.

### 5. Changelog / release notes

- README installation snippet still works (`install.sh` resolves to the
  new tag).
- Notable changes are summarized in the tag's release notes (GitHub's
  auto-generated notes are acceptable as a base; add a paragraph for
  user-visible changes).

## Cutting the release

Once the checklist is clean:

```bash
# 1. Decide the version. Follow semver; breaking changes bump major.
VERSION=v0.X.Y

# 2. Tag and push.
git tag -a "release-${VERSION#v}" -m "Release ${VERSION}"
git push origin "release-${VERSION#v}"

# 3. Watch the Release workflow.
gh run watch
```

The workflow publishes the GitHub Release and Docker image automatically.

## Post-release

- Verify `install.sh` pulls the new version on a clean machine.
- Smoke-test the Docker image: `docker run --rm ghcr.io/faultbox/faultbox:${VERSION#v} --help`.
- Update [docs/feature-manifest.md](./feature-manifest.md) summary
  counts in a follow-up PR if they have materially shifted (e.g., a
  Critical row went green).

## Rule enforcement for agents

Claude Code agents and other automation invoked during a release must:
1. Refuse to push a `release-*` tag if any checklist item is red.
2. Refuse to modify the manifest tier column without a human in the
   loop (tier is a project-level promise).
3. When fixing a testops FINDING mid-release, open an issue first,
   then land the fix referencing the issue.

Humans can override with a commit explaining why; agents cannot.
