# Reports

`faultbox report` turns a `.fb` bundle into a single, self-contained
HTML file you can email, Slack, commit to git, attach to a Jira
ticket, or publish as a CI artifact. No network calls, no build step,
no server — one file, forever.

**Live example:** [faultbox.io/reports/sample.html](https://faultbox.io/reports/sample.html)

## The command

```sh
faultbox report <bundle.fb>                  # writes report.html next to the bundle
faultbox report <bundle.fb> --output r.html  # custom path
faultbox report <bundle.fb> -o -             # write to stdout
```

Every `faultbox test` run emits a bundle by default (RFC-025). So in
practice you go:

```sh
faultbox test faultbox.star                  # writes run-<ts>-<seed>.fb
faultbox report run-2026-04-22-42.fb         # writes report.html
open report.html                             # macOS / any browser
```

Opening the file works offline. If you drop it in a Slack channel
your teammate downloads one `.html` and double-clicks it — no npm,
no Docker, no VPN, no "deploy-to-see-the-report" dance.

## What's in a report

A report reads exactly one bundle and shows six sections, in order:

1. **Header** — run ID, pass/fail pill, quick-glance pill you can
   screenshot into a Slack message as a status.
2. **Hero & stats** — scenario × fault grid cardinality, faults
   actually delivered, services observed, and total duration. One
   narrative sentence that says whether the run "held up" or
   regressed.
3. **Attention** — failed tests and any warning-level diagnostics
   surfaced first, not hidden in a grid. Each comes with a
   ready-to-paste `faultbox replay` command.
4. **Fault matrix** — scenarios × faults grid. Cells are
   colour-coded (green / red / amber / grey) and carry both an
   icon and a state, so colour-blind readers get the same signal.
   Click any cell to open the per-test drill-down.
5. **Observed coverage** — a per-service table showing how many
   tests touched the service, how many syscalls were captured,
   how many were faulted, and which syscalls dominated. This is
   what actually ran — not what you declared.
6. **Reproducibility** — Faultbox version, Go toolchain, host
   kernel, Docker version, image digests, and a one-click
   "rerun this exact run" replay command.

## Drill-down

Every matrix cell, every attention card, and every row in the
tests table opens a drill-down panel for that test:

- **Reason** for failure (the assertion or timeout that tripped).
- **Faults applied** — service, syscall, action, errno, hit count.
- **Diagnostics** — Faultbox's own hint generator surfaces patterns
  like `FAULT_FIRED_BUT_SUCCESS` (a service silently swallowed an
  error), `FAULT_NOT_FIRED` (your rule matched the wrong syscall),
  or `SERVICE_CRASHED`.
- **Replay** — single `faultbox replay <bundle.fb> --test <name>`
  command, ready to paste into terminal.
- **Event trace** — a swim-lane visualisation: one lane per service,
  markers per syscall / fault / lifecycle / violation. Hover a
  marker to see its syscall and decision; jumping to vector-clock
  causal arrows is on the v0.11.x roadmap.

### Direct-link drill-down

Reports support `#test=<name>` URL fragments. You can paste
`https://example.com/report.html#test=test_order_flow__cache_latency`
into chat and the recipient lands directly on the drill-down.

## What it isn't

- **Not a SaaS**. Everything is local. No telemetry, no accounts,
  no "upload to view". The file is the whole product.
- **Not a prescribed coverage DSL**. The "Observed coverage"
  section shows what happened — we don't take a position on what
  "should have" happened. Your `.star` spec is where intent lives.
- **Not a multi-run dashboard**. One bundle in, one report out.
  Aggregate reports (`--aggregate run-1.fb run-2.fb ...`) are a
  v0.11.x addition; the current report deliberately avoids the
  time-series rabbit hole.

## Publishing reports

Because the output is one HTML file, publishing is trivial:

- **GitHub Pages / Cloudflare Pages** — drop `report.html` in
  your artifact path. Deploy previews can link it from the PR.
- **S3 / object storage** — upload, share a signed URL, done.
- **Git** — commit `report.html` next to `faultbox.star` as
  a baseline; diffing future runs against committed reports
  catches regressions.
- **Slack / email** — the file is self-contained; attach and send.

## FAQ

**How large are reports?** Typical is 50–150 KB: bigger than the
underlying `.fb` bundle because CSS + JS are inlined, much smaller
than any JS-framework report (no node_modules). A bundle with
thousands of events produces a report around 500 KB.

**Does opening the report require an internet connection?** No.
The file works offline in any modern browser (Chrome, Safari,
Firefox, Arc, Edge). No CDN, no fonts loaded from the network.

**Can I customise the report?** Not via flags — the whole point
is that the output is standard. The JSON payload is inlined under
`window.__FAULTBOX__`; any external tool can read the bundle and
render its own variant if needed.

**Does the report contain secrets?** Whatever your services wrote
to stdout / stderr is in the bundle. If your services log
credentials, the bundle (and therefore the report) will too. This
is the same trust boundary as the bundle itself — treat them
equivalently.

**What browsers are supported?** Any browser with `<dialog>`
support — Chrome 37+, Safari 15.4+, Firefox 98+, Edge 79+. For
older environments the drill-down still opens, just without
focus trapping and ESC-to-close.

## Next

- [CLI reference: `faultbox report`](cli-reference.md#faultbox-report)
- [Bundles (`faultbox.fb`)](bundles.md)
- [Tutorial chapter 23 — Reading reports](tutorial/05-advanced/23-reports.md)
