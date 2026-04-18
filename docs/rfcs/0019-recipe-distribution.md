# RFC-019: Recipe Distribution via `@faultbox/` Prefix

- **Status:** Draft
- **Author:** Boris Glebov, Claude Opus 4.7
- **Created:** 2026-04-18
- **Branch:** `rfc/019-recipe-distribution`

## Summary

Embed the standard recipe library into the `faultbox` binary and resolve
`load("@faultbox/recipes/<name>.star", ...)` from that embedded FS. Users
get the full recipe catalog with their installed `faultbox` binary —
no filesystem setup, no network fetch, no copy-paste.

## Motivation

RFC-018 defined the recipe library (`recipes/<protocol>.star` files
exporting namespace structs). It described *what* recipes are but not
*how users get them*. With the recipes living inside the Faultbox source
tree at `/recipes/`, a user writing a spec in their own repo has no
relative path to `load()`:

```python
# This only works if the user's project happens to have a recipes/
# directory as a sibling of their spec. It doesn't.
load("./recipes/mongodb.star", "mongodb")
```

The practical effect: RFC-018 shipped a pattern that most users can't
actually use. This RFC fixes that.

## Design

### Embed at the module root

A new file `/stdlib.go` at the repo root uses Go's `//go:embed` to bake
every file under `recipes/` into the binary:

```go
// /stdlib.go
package faultbox

import "embed"

//go:embed recipes/*.star recipes/README.md
var Recipes embed.FS
```

The module root package (`faultbox`) mostly exists for this embed. The
runtime imports it as `faultbox "github.com/faultbox/Faultbox"` and reads
from `faultbox.Recipes` when resolving stdlib paths.

### `@faultbox/` load prefix

The runtime's `load()` resolver recognizes the prefix:

```python
load("@faultbox/recipes/mongodb.star", "mongodb")
```

Resolution order, top to bottom:

1. `@faultbox/<path>` → embedded FS lookup (drop prefix, read `<path>`)
2. Absolute path → read from filesystem
3. Relative path → read relative to the spec's base directory

Any existing spec that uses a relative `load("./recipes/X.star", ...)`
keeps working unchanged — only the new prefix is special-cased.

### CLI discovery commands

```
$ faultbox recipes list
Available stdlib recipes (load via @faultbox/recipes/<name>.star):
  cassandra
  clickhouse
  http2
  mongodb
  udp

$ faultbox recipes show mongodb
# Faultbox recipes: MongoDB
# ...
mongodb = struct(
    disk_full = lambda collection = "*": error(...),
    ...
)
```

No local filesystem required. Users can inspect what's shipping in their
installed binary before they load it.

### Helpful error on typos

```
load @faultbox/recipes/mongdb.star: not found in faultbox stdlib
(run 'faultbox recipes list' to see available recipes): ...
```

The error always points at the CLI command for discovery.

### User-authored recipes: unchanged

Users can still ship their own recipes in their project tree:

```python
# user-repo/recipes/checkout.star
checkout = struct(
    post_q2_race = lambda: ...,
)

# user-repo/faultbox.star
load("@faultbox/recipes/mongodb.star", "mongodb")  # stdlib
load("./recipes/checkout.star", "checkout")        # user-authored

rules = [mongodb.disk_full(), checkout.post_q2_race()]
```

Both paths work. The runtime only short-circuits on the `@faultbox/`
prefix; everything else hits the filesystem as before.

### Versioning

Recipes ship with the binary. A user running `faultbox v0.7.0` gets the
`v0.7.0` recipes, period. Upgrading `faultbox` upgrades the recipes.
Matches the Go stdlib model (and matches RFC-018's stability contract —
recipes are semver-stable within a major version, so this is safe).

## Alternatives considered

### Package registry / network fetch

`load("@faultbox/recipes/mongodb.star@v0.7", ...)` resolves via HTTP
with a lockfile. This is the long-term direction documented in
[RFC-004 (Package Format & `@scope/` Resolution)](0016-new-protocols.md).

Rejected as the first step because:
- Adds a network dependency to test runs (flaky CI, airgapped environments)
- Requires a lockfile story to stabilize versions
- RFC-004 isn't written yet — designing the full package system just to
  ship recipes is disproportionate

The embedded approach here is a strict subset of what RFC-004 will do.
When RFC-004 lands, `@faultbox/` becomes one well-known scope among
many; third-party recipe bundles (`@stripe/`, `@shopify/`, etc.) work
identically at that point.

### `faultbox init` copies recipes into the user's project

Users run `faultbox init` and get a local `recipes/` directory with the
stdlib copied in. Rejected as a primary mechanism:

- Copies drift from upstream the moment the user upgrades `faultbox`
- Every upgrade requires a re-init or diff/merge
- Puts non-user-authored code in the user's repo, inviting "fix it
  locally" commits that diverge from upstream

Worth offering as a **complement** (users who want to fork a recipe can
copy it with `faultbox recipes show mongodb > recipes/mongodb.star`),
but not as the distribution mechanism.

### Copy-paste from GitHub

Documented, zero tooling. Rejected — stale on day one, no version pinning,
requires the user to find the right tag in the GitHub UI.

## Implementation

1. `/stdlib.go` — `//go:embed recipes/*.star recipes/README.md` → `faultbox.Recipes`
2. `internal/star/runtime.go` — extend `makeLoadFunc` with the stdlib
   prefix branch (read from `faultbox.Recipes` when prefix matches)
3. `cmd/faultbox/main.go` — add `recipes` subcommand with `list` and
   `show` actions
4. Tests: four new tests in `runtime_test.go` covering embedded load,
   per-protocol load (exercises all 5 shipped recipes), unknown-recipe
   error shape, and user-authored-recipe pass-through
5. Docs:
   - `recipes/README.md` updated with the `@faultbox/` canonical load
   - Each `docs/protocols/<name>.md` "Recipes" section shows the new
     load shape
   - Recipe file headers updated (`# Usage: load("@faultbox/...", ...)`)
   - `docs/spec-language.md` gains an "Importing recipes" section

## Open questions

1. **Third-party stdlibs.** Should we reserve `@<anything-else>/` or
   hard-code only `@faultbox/` for v1? Proposal: hard-code for v1.
   RFC-004 will formalize the general scope mechanism.

2. **Versioned imports.** `@faultbox/recipes/mongodb.star@v1` — do we
   need this? Recipes are pinned to the binary, so the user already has
   version locality. Proposal: no version pinning in v1; revisit when
   RFC-004 introduces the general package system.

3. **Listing third-party packages via `recipes list`.** If we extend
   beyond the stdlib, does `recipes list` still make sense as a global
   command? Proposal: rename to `faultbox stdlib list` when RFC-004
   lands and introduce `faultbox packages list` for the registry.

## Non-goals

- **Not a package registry.** No network fetch, no lockfiles, no
  dependency resolution. The embedded approach is intentionally
  minimal — the full package story is RFC-004.
- **Not a recipe editor.** `faultbox recipes show` is read-only.
  Forking a recipe means copying its output and editing locally.
- **Not a way to extend the stdlib at runtime.** `@faultbox/` resolves
  only to what was baked into the binary. Users' own recipes live at
  filesystem paths.

## Success criteria

- A user with only `faultbox` on their PATH (no source checkout) can
  write a spec that loads any stdlib recipe and have it work.
- The full recipe catalog is discoverable via a single CLI command
  without network access.
- Typo errors point to the CLI discovery command.
- User-authored recipes via relative paths continue to work unchanged.
- Binary size impact: embedded recipes are ≤10KB total; negligible.
