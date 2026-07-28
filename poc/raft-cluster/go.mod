module github.com/faultbox/poc/raft-cluster

go 1.24.0

require (
	github.com/hashicorp/go-hclog v1.6.3

	// Pinned to hashicorp/raft main @ 4c8f61ac (2026-05-19) — the exact commit
	// the Antithesis study forked from, not the v1.7.3 release tag.
	//
	// v1.7.3 (2025-03-18) is still the newest *tag*; main is 48 commits ahead
	// of it. Testing the tag would have meant testing code 14 months older
	// than the code the study reported bugs against. See
	// docs/design/2026-07-28-raft-mesh-results.md §4.
	github.com/hashicorp/raft v1.7.4-0.20260519195656-4c8f61ac9255
)

require (
	github.com/armon/go-metrics v0.4.1 // indirect
	github.com/fatih/color v1.13.0 // indirect
	github.com/hashicorp/go-immutable-radix v1.0.0 // indirect
	github.com/hashicorp/go-metrics v0.5.4 // indirect
	github.com/hashicorp/go-msgpack/v2 v2.1.5 // indirect
	github.com/hashicorp/golang-lru v0.5.0 // indirect
	github.com/mattn/go-colorable v0.1.12 // indirect
	github.com/mattn/go-isatty v0.0.14 // indirect
	golang.org/x/sys v0.13.0 // indirect
)
