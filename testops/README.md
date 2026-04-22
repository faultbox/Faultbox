# testops

Regression harness for Faultbox itself. Runs curated Starlark specs
through the `faultbox` CLI and diffs each run's normalized trace against
a committed golden file.

Background: ../docs/design/testops.md (Phase plan).

## Layout

```
testops/
  harness.go           # Case type + corpus registry (authoritative)
  harness_test.go      # //go:build linux — runs every Case
  goldens/             # committed *.norm files, one per Case
  README.md            # this file
```

## Run

The harness is a plain `go test` target, so it rides along with the
existing CI workflow and lives under the `//go:build linux` tag.

```
go test ./testops/...                 # verify all goldens
go test ./testops/... -run mock_demo  # one case
go test ./testops/... -update         # regenerate goldens
go test ./testops/... -run mock_demo -update -v
```

On a non-Linux host the package compiles but has no tests, which is
intended — `faultbox test` requires Linux kernel primitives (see
[CLAUDE.md](../CLAUDE.md#two-modes)). Use the Lima env for local runs:

```
make env-exec CMD="go test ./testops/... -v"
```

## Adding a case

1. Append a `Case{}` literal to `Cases` in [harness.go](harness.go).
2. Seed the golden from a known-good environment:
   `go test ./testops/... -run <name> -update`
3. Inspect `testops/goldens/<name>.norm` — it must be stable across
   repeated runs with the same seed. If not, open a GitHub issue with
   the reproducer and set `Skip:` on the case pointing at the issue URL
   until fixed.
4. Commit the registry change and the golden in one PR.

## Un-skipping a LinuxOnly case

1. Confirm the prerequisite is provisioned in CI (`make testops-prep`
   runs as a CI step; add Docker service definitions for container
   cases).
2. From a Linux host (Lima VM or a GitHub-hosted runner branch), run:
   `go test ./testops/... -run <name> -update`
3. Verify stability across 5 consecutive runs with the same seed.
4. Remove the `Skip:` field from the Case literal.
5. Commit the golden + registry change together.

## Non-determinism policy

A golden is only committed if the same `(spec, seed)` produces the same
normalized output across at least 5 consecutive local runs. Flaky
goldens poison the corpus — quarantine them via `Skip:` rather than
letting them churn in CI.

## Anti-goals

- This harness does **not** re-implement normalization. It calls
  `faultbox test --normalize` and defers to `faultbox diff` for
  comparison, so the product's own definition of "equivalent traces" is
  what we test against.
- No manual test-case management (TestRail, Zephyr, Xray). Cases live
  in source control as Go literals + golden files.
