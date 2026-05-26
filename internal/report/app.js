/* Faultbox Report — v0.11.0 client-side renderer.
 *
 * The Go side embeds manifest.json, env.json and trace.json into a
 * single data <script> tag (id="faultbox-data", type=application/json).
 * This file parses that, then paints each section from the DOM IDs the
 * template already laid out. No framework, no bundler, ~one file of
 * plain functions — matches the "one HTML, offline-forever" promise.
 *
 * Render order matters only in that the hero stats read from the same
 * derived counts the matrix uses; otherwise every render* below is
 * independent and can be read top-to-bottom.
 */
(function () {
  "use strict";

  // ── Data loading ────────────────────────────────────────────────
  // The Go side inlines the manifest+env+trace payload as gzip+base64
  // in <script id="faultbox-data-gz"> (RFC-031, v0.12). Browsers
  // decompress via the DecompressionStream API (Chrome 80+, Safari
  // 16.4+, Firefox 113+). A legacy path handles the v0.11 format
  // where the data sat in #faultbox-data as raw JSON, so pre-0.12
  // reports still render when opened by a newer faultbox build.
  async function loadData() {
    var gzNode = document.getElementById("faultbox-data-gz");
    if (gzNode) {
      try {
        var b64 = (gzNode.textContent || "").trim();
        var bytes = base64ToBytes(b64);
        if (typeof DecompressionStream === "undefined") {
          showUnsupportedBrowserError();
          return null;
        }
        var stream = new Blob([bytes]).stream()
          .pipeThrough(new DecompressionStream("gzip"));
        var text = await new Response(stream).text();
        return JSON.parse(text);
      } catch (err) {
        console.error("faultbox: failed to decode gzip+base64 payload", err);
        return null;
      }
    }
    var legacy = document.getElementById("faultbox-data");
    if (legacy) {
      try {
        return JSON.parse(legacy.textContent || "{}");
      } catch (err) {
        console.error("faultbox: failed to parse embedded data", err);
        return null;
      }
    }
    return null;
  }

  function base64ToBytes(b64) {
    // The Go side encodes with URL-safe base64 (alphabet A-Z, a-z,
    // 0-9, -, _) so that html/template doesn't HTML-escape "+" or
    // "/" to numeric entities inside the <script> tag. atob() only
    // accepts standard base64, so swap the substitutions back.
    var std = b64.replace(/-/g, "+").replace(/_/g, "/");
    var bin = atob(std);
    var len = bin.length;
    var out = new Uint8Array(len);
    for (var i = 0; i < len; i++) out[i] = bin.charCodeAt(i);
    return out;
  }

  function showUnsupportedBrowserError() {
    var host = document.querySelector("main.container") || document.body;
    var banner = el("div", { class: "browser-error" }, [
      el("strong", { text: "This browser is too old to open this report." }),
      el("p", { text: "Faultbox reports use the DecompressionStream API (Chrome 80+, Safari 16.4+, Firefox 113+). Please open in a modern browser." }),
    ]);
    host.insertBefore(banner, host.firstChild);
  }

  // ── Small helpers ───────────────────────────────────────────────
  function el(tag, attrs, children) {
    var e = document.createElement(tag);
    if (attrs) {
      for (var k in attrs) {
        if (k === "class") e.className = attrs[k];
        else if (k === "text") e.textContent = attrs[k];
        else if (k === "html") e.innerHTML = attrs[k];
        else if (k.slice(0, 2) === "on" && typeof attrs[k] === "function")
          e.addEventListener(k.slice(2), attrs[k]);
        else e.setAttribute(k, attrs[k]);
      }
    }
    if (children) {
      for (var i = 0; i < children.length; i++) {
        var c = children[i];
        if (c == null) continue;
        if (typeof c === "string") e.appendChild(document.createTextNode(c));
        else e.appendChild(c);
      }
    }
    return e;
  }

  function fmtDuration(ms) {
    if (ms == null) return "";
    if (ms < 1000) return ms + " ms";
    var s = ms / 1000;
    if (s < 60) return s.toFixed(s < 10 ? 2 : 1) + " s";
    var m = Math.floor(s / 60);
    var rem = Math.round(s - m * 60);
    return m + "m " + rem + "s";
  }

  function fmtTimestamp(iso) {
    if (!iso) return "";
    var d = new Date(iso);
    if (isNaN(d.getTime())) return iso;
    return d.toISOString().replace("T", " ").replace(/\.\d+Z$/, " UTC");
  }

  function escapeText(s) {
    return (s == null ? "" : String(s));
  }

  // compactCount renders a fold count as a short, lane-friendly label
  // (3983 → "3.9k", 86874 → "86k", 4_000_000 → "4M"). Values under
  // 1000 stay as-is. Decimals truncate (not round) so the label
  // never overstates magnitude — a "3.9k" disc always represents
  // ≥ 3900 events.
  function compactCount(n) {
    if (n == null) return "";
    if (n < 1000) return String(n);
    if (n < 10000) return (Math.floor(n / 100) / 10).toFixed(1).replace(/\.0$/, "") + "k";
    if (n < 1000000) return Math.floor(n / 1000) + "k";
    if (n < 10000000) return (Math.floor(n / 100000) / 10).toFixed(1).replace(/\.0$/, "") + "M";
    if (n < 1000000000) return Math.floor(n / 1000000) + "M";
    return (Math.floor(n / 100000000) / 10).toFixed(1).replace(/\.0$/, "") + "B";
  }

  // RFC-027 + issue #75: map the five manifest outcomes to pill /
  // matrix-cell classes. Unknown outcomes fall back to "warn" so a
  // future schema_version that adds a tag we haven't shipped still
  // renders visibly (rather than vanishing into default text).
  function outcomeClass(outcome) {
    switch (outcome) {
      case "passed": return "pass";
      case "failed": return "fail";
      case "expectation_violated": return "violated";
      case "fault_bypassed": return "bypassed";
      case "errored": return "errored";
      // RFC-041 §5.5(c) — timeout with pending temporal expectations.
      case "inconclusive": return "warn";
      // RFC-043 §5.3 — plan-tree pruning (halt() in the body). Render
      // alongside fault_bypassed since neither is a verdict.
      case "halted": return "bypassed";
      default: return "warn";
    }
  }

  // outcomeFromTrace collapses the legacy trace.json result field
  // plus RFC-027 expectation_violated and issue #75 fault_bypassed
  // refinements into the canonical manifest string. Kept in one
  // place so the drill-down header, the tests table, and the matrix
  // stay in lockstep. Precedence mirrors cmd/faultbox/main.go:
  // failed/errored > expectation_violated > fault_bypassed > passed.
  function outcomeFromTrace(test) {
    if (!test) return "";
    if (test.result === "pass") {
      return test.fault_bypassed ? "fault_bypassed" : "passed";
    }
    if (test.result === "fail") {
      return test.expectation_violated ? "expectation_violated" : "failed";
    }
    return "errored";
  }

  function bundleBasename() {
    // When the user opens the report by double-clicking it, the bundle
    // file sits in the same directory. We can't read the filesystem,
    // but we can reconstruct a reasonable replay target from run_id.
    // Fallback: the generic name "<bundle>.fb".
    var m = (window.__FAULTBOX__ && window.__FAULTBOX__.manifest) || {};
    if (!m.run_id) return "<bundle>.fb";
    return "run-" + m.run_id + ".fb";
  }

  // ── Derived counts ──────────────────────────────────────────────
  function deriveMetrics(data) {
    var manifest = data.manifest || {};
    var trace = data.trace || {};
    var tests = trace.tests || [];
    var matrix = trace.matrix || null;

    var services = {};
    var totalEvents = 0;
    var faultsDelivered = 0;

    for (var i = 0; i < tests.length; i++) {
      var t = tests[i];
      var ss = t.syscall_summary || {};
      for (var svc in ss) services[svc] = true;
      totalEvents += (t.events ? t.events.length : 0);
      if (t.faults) {
        for (var j = 0; j < t.faults.length; j++) {
          faultsDelivered += (t.faults[j].hits || 0);
        }
      }
    }

    return {
      scenarios: matrix ? (matrix.scenarios || []).length : 0,
      faults: matrix ? (matrix.faults || []).length : 0,
      serviceCount: Object.keys(services).length,
      eventCount: totalEvents,
      faultsDelivered: faultsDelivered,
      hasMatrix: !!matrix,
      durationMs: manifest.tests ? sumDuration(manifest.tests) : (trace.duration_ms || 0),
    };
  }

  function sumDuration(rows) {
    var total = 0;
    for (var i = 0; i < rows.length; i++) total += (rows[i].duration_ms || 0);
    return total;
  }

  // ── Header pill + run ID ────────────────────────────────────────
  function renderHeaderMeta(data) {
    var host = document.getElementById("header-meta");
    if (!host) return;
    var m = data.manifest || {};
    var s = m.summary || {};
    var pillClass = "pass";
    var pillText = "✓ " + (s.passed || 0) + "/" + (s.total || 0) + " passed";
    if ((s.failed || 0) + (s.errored || 0) > 0) {
      pillClass = "fail";
      pillText = "✗ " + (s.failed || 0) + " failed";
      // RFC-027: a matrix row whose expect predicate rejected the
      // scenario result is counted in failed (kept for legacy
      // consumers) and surfaced separately here so users can scan
      // the header and see at a glance how many rows disagreed.
      if (s.expectation_violated) {
        pillText += " (" + s.expectation_violated + " violated expectation)";
      }
      if (s.errored) pillText += " · " + s.errored + " errored";
    }
    // Issue #75: bypass is a refinement of passed, not a failure, so
    // it has its own badge after the main pill text rather than
    // promoting the header to red.
    if (s.fault_bypassed) {
      pillText += " · " + s.fault_bypassed + " bypassed";
      if (pillClass === "pass") pillClass = "bypassed";
    }
    // RFC-041 §5.5(c): inconclusive rows aren't pass and aren't fail —
    // surface their count separately so users see them at a glance.
    if (s.inconclusive) {
      pillText += " · " + s.inconclusive + " inconclusive";
      if (pillClass === "pass") pillClass = "warn";
    }
    host.innerHTML = "";
    host.appendChild(el("span", { class: "pill " + pillClass, text: pillText }));
    if (m.run_id) host.appendChild(el("span", { class: "mono", text: m.run_id }));
  }

  // ── Hero stats ──────────────────────────────────────────────────
  function renderHeroStats(data, metrics) {
    var host = document.getElementById("hero-stats");
    if (!host) return;
    var m = data.manifest || {};
    var s = m.summary || {};

    var cards = [];
    if (metrics.hasMatrix) {
      cards.push(stat(metrics.scenarios + " × " + metrics.faults, "matrix cells",
        String((m.summary && m.summary.total) || 0) + " runs"));
    } else {
      cards.push(stat(String(s.total || 0), "tests", null));
    }
    cards.push(stat(String(metrics.faultsDelivered), "faults delivered",
      metrics.faultsDelivered === 0 && (s.total || 0) > 0
        ? "no faults matched (zero traffic?)"
        : null));
    cards.push(stat(String(metrics.serviceCount), "services observed",
      metrics.eventCount ? metrics.eventCount + " events captured" : null));
    cards.push(stat(fmtDuration(metrics.durationMs), "duration",
      m.created_at ? fmtTimestamp(m.created_at) : null));

    host.innerHTML = "";
    for (var i = 0; i < cards.length; i++) host.appendChild(cards[i]);
  }

  function stat(value, label, caption) {
    var kids = [
      el("div", { class: "stat-value", text: value }),
      el("div", { class: "stat-label", text: label }),
    ];
    if (caption) kids.push(el("div", { class: "stat-caption", text: caption }));
    return el("div", { class: "stat" }, kids);
  }

  // ── Fault matrix ────────────────────────────────────────────────
  function renderMatrix(data) {
    var section = document.getElementById("matrix-section");
    var host = document.getElementById("matrix");
    var hint = document.getElementById("matrix-hint");
    if (!section || !host) return;
    var matrix = (data.trace && data.trace.matrix) || null;
    if (!matrix || !(matrix.scenarios || []).length || !(matrix.faults || []).length) return;

    section.hidden = false;
    var scenarios = matrix.scenarios;
    var faults = matrix.faults;
    var cells = matrix.cells || [];

    // Build lookup for O(1) cell access.
    var byKey = {};
    for (var i = 0; i < cells.length; i++) {
      var c = cells[i];
      byKey[c.scenario + "∷" + c.fault] = c;
    }

    // Grid: [corner] [fault1] [fault2] ... / [scenario1] [cell] [cell] ...
    var grid = el("div", { class: "matrix" });
    grid.style.gridTemplateColumns = "minmax(140px, max-content) repeat(" + faults.length + ", minmax(90px, 1fr))";

    grid.appendChild(el("div", { class: "matrix-corner", text: "scenario ╲ fault" }));
    for (var f = 0; f < faults.length; f++) {
      grid.appendChild(el("div", { class: "matrix-col-head", text: faults[f] }));
    }
    for (var s = 0; s < scenarios.length; s++) {
      grid.appendChild(el("div", { class: "matrix-row-head", text: scenarios[s] }));
      for (var ff = 0; ff < faults.length; ff++) {
        var cell = byKey[scenarios[s] + "∷" + faults[ff]];
        grid.appendChild(renderCell(cell));
      }
    }

    host.innerHTML = "";
    host.appendChild(grid);
    if (hint) {
      hint.textContent = "Click any cell to open the per-test drill-down: "
        + "faults applied, diagnostics, event trace, and a one-click replay command.";
    }
  }

  function renderCell(cell) {
    if (!cell) {
      return el("div", { class: "matrix-cell skip", title: "not run" },
        [el("span", { class: "matrix-cell-icon", text: "·" })]);
    }
    // RFC-027: prefer the explicit outcome string (passed / failed /
    // expectation_violated / errored). Legacy trace.json files without
    // the field fall back to the old passed/fail binary so older
    // bundles still render.
    var outcome = cell.outcome || (cell.passed ? "passed" : "failed");
    var kind = outcomeClass(outcome);
    var icon;
    switch (outcome) {
      case "passed": icon = "✓"; break;
      case "failed": icon = "✗"; break;
      case "expectation_violated": icon = "≠"; break;
      case "fault_bypassed": icon = "∅"; break;
      case "errored": icon = "!"; break;
      // RFC-041 §5.5(c) — timeout with pending temporal expectation.
      case "inconclusive": icon = "?"; break;
      // RFC-043 §5.3 — body called halt().
      case "halted": icon = "⊘"; break;
      default: icon = "?";
    }
    var title = cell.scenario + " × " + cell.fault + "\n" + outcome;
    if (cell.expectation) title += " (" + cell.expectation + ")";
    if (outcome === "fault_bypassed" && cell.bypassed_rules && cell.bypassed_rules.length) {
      var bits = [];
      for (var br = 0; br < cell.bypassed_rules.length; br++) {
        var r = cell.bypassed_rules[br];
        bits.push(r.service + "." + r.syscall);
      }
      title += "\nfault did not fire: " + bits.join(", ");
    }
    if (outcome !== "passed" && outcome !== "fault_bypassed" && cell.reason) title += "\n" + cell.reason;
    if (cell.duration_ms != null) title += "\nduration: " + fmtDuration(cell.duration_ms);
    // Matrix test names follow the `test_<scenario>__<fault>` convention
    // emitted by the `fault_matrix()` builtin. Binding it here is what
    // turns a cell click into a drill-down open.
    var testName = "test_" + cell.scenario + "__" + cell.fault;
    var attrs = {
      class: "matrix-cell " + kind,
      title: title,
      tabindex: "0",
      role: "button",
      "data-test": testName,
      onclick: function () { openDrillDown(testName); },
      onkeydown: function (e) {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          openDrillDown(testName);
        }
      },
    };
    return el("div", attrs, [el("span", { class: "matrix-cell-icon", text: icon })]);
  }

  // ── Attention list (failures + warnings) ───────────────────────
  function renderAttention(data) {
    var section = document.getElementById("attention-section");
    var host = document.getElementById("attention-list");
    if (!section || !host) return;
    var tests = (data.trace && data.trace.tests) || [];
    var replayCmd = "faultbox replay " + bundleBasename();

    var attention = [];
    for (var i = 0; i < tests.length; i++) {
      var t = tests[i];
      if (t.result === "fail" || t.result === "error") {
        attention.push({ kind: "fail", test: t });
      } else if (t.diagnostics && t.diagnostics.length) {
        var hasWarn = false;
        for (var j = 0; j < t.diagnostics.length; j++) {
          if (t.diagnostics[j].level === "warning" || t.diagnostics[j].level === "error") {
            hasWarn = true; break;
          }
        }
        if (hasWarn) attention.push({ kind: "warn", test: t });
      }
    }
    if (!attention.length) return;
    section.hidden = false;
    host.innerHTML = "";
    for (var k = 0; k < attention.length; k++) {
      host.appendChild(renderAttentionItem(attention[k], replayCmd));
    }
  }

  function renderAttentionItem(entry, replayCmd) {
    var t = entry.test;
    var title = t.name;
    var reason = t.reason || (t.error_detail && t.error_detail.message) || "";
    var cmd = replayCmd + " --test " + t.name;

    var head = el("div", { class: "attention-item-head" }, [
      el("span", { class: "attention-item-title", text: (entry.kind === "fail" ? "✗ " : "⚠ ") + title }),
      el("span", { class: "mono", text: fmtDuration(t.duration_ms) }),
    ]);
    var kids = [head];
    if (reason) kids.push(el("div", { class: "attention-item-reason", text: reason }));
    if (t.diagnostics && t.diagnostics.length) {
      for (var i = 0; i < t.diagnostics.length; i++) {
        var d = t.diagnostics[i];
        if (d.level !== "warning" && d.level !== "error") continue;
        kids.push(el("div", { class: "attention-item-reason",
          text: "[" + d.code + "] " + d.message }));
      }
    }
    kids.push(replayRow(cmd));
    return el("div", {
      class: "attention-item " + entry.kind,
      "data-test": t.name,
      onclick: function (e) {
        // Replay-row clicks handle their own copy behaviour and must
        // not trigger a drill-down.
        if (e.target.closest(".attention-item-replay")) return;
        openDrillDown(t.name);
      },
    }, kids);
  }

  function replayRow(cmd) {
    var code = el("code", { class: "mono", text: cmd });
    var btn = el("button", {
      class: "copy-btn",
      type: "button",
      "aria-label": "Copy replay command",
      onclick: function () { copyToClipboard(cmd, btn); },
      text: "Copy",
    });
    return el("div", { class: "attention-item-replay" }, [code, btn]);
  }

  // ── Tests table (fallback when no matrix) ───────────────────────
  function renderTestsTable(data) {
    var section = document.getElementById("tests-section");
    var body = document.getElementById("tests-tbody");
    if (!section || !body) return;

    var manifest = data.manifest || {};
    var matrix = (data.trace && data.trace.matrix) || null;
    if (matrix) return; // matrix tests already covered
    if (!(manifest.tests && manifest.tests.length)) return;
    section.hidden = false;
    body.innerHTML = "";
    for (var i = 0; i < manifest.tests.length; i++) {
      var r = manifest.tests[i];
      var pillClass = outcomeClass(r.outcome);
      var fa = (r.fault_assumptions || []).join(", ");
      // RFC-042 §8.8 / RFC-043 §5.2 multi-leaf rendering: when a test
      // fans out across choose() / probability axes, every leaf shares
      // the test name. Suffix the display name with the leaf ordinal
      // so the table doesn't read as N duplicates. Single-leaf rows
      // (LeafID empty) keep the rc1 shape.
      var displayName = r.leaf_id ? r.name + " [leaf " + r.leaf_id + "]" : r.name;
      var tr = el("tr", {
        "data-test": r.name,
        onclick: (function (n) { return function () { openDrillDown(n); }; })(r.name),
      }, [
        el("td", { text: displayName }),
        el("td", { class: "outcome" }, [el("span", { class: "pill " + pillClass, text: r.outcome })]),
        el("td", { text: fmtDuration(r.duration_ms) }),
        el("td", { text: r.expectation || "" }),
        el("td", { text: fa }),
      ]);
      body.appendChild(tr);
    }
  }

  // ── Observed coverage ───────────────────────────────────────────
  function renderCoverage(data) {
    var section = document.getElementById("coverage-section");
    var tbody = document.getElementById("coverage-tbody");
    if (!section || !tbody) return;

    var tests = (data.trace && data.trace.tests) || [];
    if (!tests.length) return;

    // Aggregate syscall_summary across all tests, grouped by service.
    // The shape each test already produces is per-service; we union
    // the breakdowns and track tests touching each service. Proxy-mode
    // runs have no syscall events — in that case fall back to the
    // event log so the section still lists the services that
    // participated, with event-type breakdowns instead of syscalls.
    var byService = {};
    var hasSyscalls = false;
    for (var i = 0; i < tests.length; i++) {
      var t = tests[i];
      var ss = t.syscall_summary || {};
      for (var svc in ss) {
        var s = ss[svc];
        if (!byService[svc]) {
          byService[svc] = { tests: {}, total: 0, faulted: 0, breakdown: {} };
        }
        byService[svc].tests[t.name] = true;
        byService[svc].total += (s.total || 0);
        byService[svc].faulted += (s.faulted || 0);
        for (var sc in (s.breakdown || {})) {
          byService[svc].breakdown[sc] = (byService[svc].breakdown[sc] || 0) + s.breakdown[sc];
        }
        hasSyscalls = true;
      }
    }
    // Event-log fallback for services that never registered syscalls.
    // We only fold in events for services not already covered by
    // syscall_summary so binary-mode runs aren't disturbed.
    var byServiceFromEvents = {};
    for (var i2 = 0; i2 < tests.length; i2++) {
      var t2 = tests[i2];
      var events2 = t2.events || [];
      for (var e2 = 0; e2 < events2.length; e2++) {
        var ev2 = events2[e2];
        var svc2 = ev2.service;
        if (!svc2 || byService[svc2]) continue;
        if (!byServiceFromEvents[svc2]) {
          byServiceFromEvents[svc2] = { tests: {}, total: 0, faulted: 0, breakdown: {} };
        }
        var bag = byServiceFromEvents[svc2];
        bag.tests[t2.name] = true;
        bag.total++;
        var kind2 = ev2.type || "event";
        bag.breakdown[kind2] = (bag.breakdown[kind2] || 0) + 1;
        if (ev2.type === "fault_applied" || ev2.type === "violation") bag.faulted++;
      }
    }
    for (var svc3 in byServiceFromEvents) byService[svc3] = byServiceFromEvents[svc3];

    var services = Object.keys(byService);
    if (!services.length) return;
    services.sort();
    section.hidden = false;
    var thAct = document.getElementById("coverage-th-activity");
    var thTop = document.getElementById("coverage-th-top");
    var hint = document.getElementById("coverage-hint");
    if (!hasSyscalls) {
      if (thAct) thAct.textContent = "Events";
      if (thTop) thTop.textContent = "Top event kinds";
      if (hint) {
        hint.textContent = "What actually happened — per service, measured from the event log. "
          + "This run captured no syscall events (proxy-only or non-instrumented mode).";
      }
    }
    tbody.innerHTML = "";

    for (var s = 0; s < services.length; s++) {
      var name = services[s];
      var agg = byService[name];
      var top = topSyscalls(agg.breakdown, 4);
      var tr = el("tr", null, [
        el("td", null, [serviceNameCell(name)]),
        el("td", { class: "num", text: String(Object.keys(agg.tests).length) }),
        el("td", { class: "num", text: String(agg.total) }),
        el("td", { class: "num",
          html: agg.faulted ? '<span class="faulted-badge">' + agg.faulted + '</span>' : "0" }),
        el("td", { class: "coverage-syscalls" }, top),
      ]);
      tbody.appendChild(tr);
    }
  }

  // serviceNameCell renders a service name as a link into the spec
  // section (if a matching definition is found at render time) — or
  // falls back to a plain text span. Keeps the table visually quiet
  // but makes the name actionable.
  function serviceNameCell(name) {
    var def = findServiceLine(name);
    if (!def) return document.createTextNode(name);
    var a = el("a", {
      class: "service-link",
      href: "#" + specAnchorId(def.path, def.line),
      "data-service": name,
      onclick: function (e) {
        e.preventDefault();
        focusSpecLine(def.path, def.line);
      },
      text: name,
    });
    return a;
  }

  function topSyscalls(breakdown, n) {
    var entries = [];
    for (var k in breakdown) entries.push([k, breakdown[k]]);
    entries.sort(function (a, b) { return b[1] - a[1]; });
    var out = [];
    for (var i = 0; i < Math.min(n, entries.length); i++) {
      out.push(el("span", { class: i === 0 ? "hot" : null,
        text: entries[i][0] + " (" + entries[i][1] + ")" }));
    }
    if (entries.length > n) out.push(el("span", { text: "+" + (entries.length - n) + " more" }));
    return out;
  }

  // ── Drill-down modal ────────────────────────────────────────────
  var testsByName = {};

  function indexTests(data) {
    testsByName = {};
    var tests = (data.trace && data.trace.tests) || [];
    for (var i = 0; i < tests.length; i++) testsByName[tests[i].name] = tests[i];
  }

  function openDrillDown(testName) {
    var test = testsByName[testName];
    if (!test) {
      console.warn("faultbox: no trace entry for", testName);
      return;
    }
    var dialog = document.getElementById("drill-down");
    var titleHost = document.getElementById("dd-title");
    var body = document.getElementById("dd-body");
    if (!dialog || !titleHost || !body) return;

    titleHost.innerHTML = "";
    titleHost.appendChild(drillDownHeader(test));

    body.innerHTML = "";
    populateDrillDown(body, test);

    if (typeof dialog.showModal === "function") {
      dialog.showModal();
    } else {
      dialog.setAttribute("open", "");
    }
  }

  function closeDrillDown() {
    var dialog = document.getElementById("drill-down");
    if (!dialog) return;
    if (typeof dialog.close === "function") dialog.close();
    else dialog.removeAttribute("open");
  }

  function drillDownHeader(test) {
    var outcome = outcomeFromTrace(test);
    var pillCls = outcomeClass(outcome);
    var subtitleKids = [
      el("span", { class: "pill " + pillCls, text: outcome }),
      el("span", null, [document.createTextNode(fmtDuration(test.duration_ms))]),
      el("span", null, [document.createTextNode("seed " + (test.seed != null ? test.seed : "—"))]),
    ];
    if (test.expectation) {
      subtitleKids.push(el("span", { class: "mono", title: "expect predicate used by this row",
        text: "expect: " + test.expectation }));
    }
    var subtitle = el("div", { class: "drill-down-subtitle" }, subtitleKids);
    return el("div", null, [
      el("h3", { id: "dd-title-text", text: test.name }),
      subtitle,
    ]);
  }

  function populateDrillDown(body, test) {
    // Structured assertion (when an assert_eq / assert_true fired):
    // surfaces Expected vs Actual at the top of the body so the user
    // doesn't need to read the spec to learn what the test compared.
    if (test.assertion) {
      body.appendChild(el("h4", { text: "Assertion" }));
      body.appendChild(renderAssertion(test.assertion, test.events || []));
    }

    // Reason / assertion message (shown whether or not the test failed
    // — a passing test with a reason is rare, but we want it visible).
    if (test.reason) {
      body.appendChild(el("h4", { text: "Reason" }));
      body.appendChild(el("div", {
        class: "drill-down-reason " + (test.result === "pass" ? "pass" : ""),
        text: test.reason,
      }));
    }

    // Faults applied. Always render the section header so users can
    // tell at a glance "this test ran without injected faults" vs
    // "this test had X faults" — the empty state is informative.
    var proxyFaults = collectProxyFaults(test.events || []);
    var hasSeccomp = test.faults && test.faults.length;
    body.appendChild(el("h4", { text: "Faults applied" }));
    if (hasSeccomp) {
      body.appendChild(el("div", {
        class: "stat-caption",
        text: "Syscall-level faults (seccomp):",
      }));
      body.appendChild(faultsTable(test.faults));
    }
    if (proxyFaults.length) {
      // Seccomp faults match against syscall (rows have syscall+errno);
      // proxy faults match against protocol+assumption (mysql_deadlock,
      // redis_oom). Different columns, so we render two tables under
      // the same header.
      body.appendChild(el("div", {
        class: "stat-caption",
        text: "Proxy-level fault assumptions injected at the protocol layer:",
      }));
      body.appendChild(proxyFaultsTable(proxyFaults));
    }
    if (!hasSeccomp && !proxyFaults.length) {
      body.appendChild(el("div", {
        class: "stat-caption",
        text: "No faults were injected during this test.",
      }));
    }

    // Issue #75: fault_bypassed rows surface the rules the runtime
    // saw installed-but-never-matched. Without this the user is
    // left guessing which fault was inert.
    if (test.fault_bypassed && test.bypassed_rules && test.bypassed_rules.length) {
      body.appendChild(el("h4", { text: "Faults that did not fire" }));
      body.appendChild(bypassedRulesTable(test.bypassed_rules));
      body.appendChild(el("div", { class: "stat-caption",
        text: "These rules were installed but never matched a syscall. " +
              "The scenario likely didn't exercise the faulted code path — " +
              "cache hit, wrong syscall family, or alternate branch." }));
    }

    // Diagnostics.
    if (test.diagnostics && test.diagnostics.length) {
      body.appendChild(el("h4", { text: "Diagnostics" }));
      for (var i = 0; i < test.diagnostics.length; i++) {
        body.appendChild(renderDiag(test.diagnostics[i]));
      }
    }

    // Replay command.
    body.appendChild(el("h4", { text: "Replay" }));
    body.appendChild(drillDownReplay(test));

    // Trace (swim-lane).
    body.appendChild(el("h4", { text: "Event trace" }));
    body.appendChild(renderTrace(test));

    // Topology — which services this test actually touched, each
    // cross-linking to its spec definition.
    body.appendChild(buildTopologyFold(test));

    // Source — owning .star file with line-number gutter, auto-scrolled
    // to `def test_<name>`. Closed by default; opens on demand.
    body.appendChild(buildSourceFold(test));
  }

  // buildTopologyFold lists the services this test actually touched.
  // Derived from syscall_summary (what actually executed syscalls) so
  // the list matches observation, not declaration. Each chip cross-
  // jumps to the service's definition in the top-level Spec section.
  function buildTopologyFold(test) {
    var ss = test.syscall_summary || {};
    var services = Object.keys(ss).sort();
    var fold = document.createElement("details");
    fold.className = "dd-fold";
    var sum = document.createElement("summary");
    sum.innerHTML = "<span>Topology \u2014 " + services.length + " service"
      + (services.length === 1 ? "" : "s") + " touched</span>"
      + "<span>click to toggle</span>";
    fold.appendChild(sum);

    var body = el("div", { class: "dd-fold-body" });
    if (!services.length) {
      body.appendChild(el("div", { class: "dd-fold-empty",
        text: "No services recorded syscalls for this test." }));
      fold.appendChild(body);
      return fold;
    }
    var row = el("div", { class: "dd-topology" });
    for (var i = 0; i < services.length; i++) {
      var name = services[i];
      var stats = ss[name];
      var def = findServiceLine(name);
      var meta = (stats.total || 0) + " syscalls"
        + (stats.faulted ? " · " + stats.faulted + " faulted" : "");
      var chip;
      if (def) {
        chip = el("a", {
          class: "dd-topology-chip",
          href: "#" + specAnchorId(def.path, def.line),
          onclick: (function (p, l) {
            return function (e) {
              e.preventDefault();
              closeDrillDown();
              setTimeout(function () { focusSpecLine(p, l); }, 120);
            };
          })(def.path, def.line),
        }, [
          document.createTextNode(name),
          el("span", { class: "dd-topology-chip-meta", text: meta }),
        ]);
      } else {
        chip = el("span", { class: "dd-topology-chip", title: "no spec definition found" }, [
          document.createTextNode(name),
          el("span", { class: "dd-topology-chip-meta", text: meta }),
        ]);
      }
      row.appendChild(chip);
    }
    body.appendChild(row);
    fold.appendChild(body);
    return fold;
  }

  // buildSourceFold renders the spec file that owns this test. The
  // file body is rendered eagerly (reuses the same line-numbered
  // viewer as the top-level Spec section) and, on open, scrolls the
  // `def test_<name>` line into the center of the scroll container.
  function buildSourceFold(test) {
    var fold = document.createElement("details");
    fold.className = "dd-fold";
    var def = findTestLine(test.name);
    var summaryMeta;
    if (def && def.generated) {
      summaryMeta = "<span>" + def.path.replace(/^spec\//, "") +
        " · generated by fault_matrix on line " + def.line + "</span>";
    } else if (def) {
      summaryMeta = "<span>" + def.path.replace(/^spec\//, "") +
        " · line " + def.line + "</span>";
    } else {
      summaryMeta = "<span>no matching spec def</span>";
    }
    var sum = document.createElement("summary");
    sum.innerHTML = "<span>Source</span>" + summaryMeta;
    fold.appendChild(sum);

    var body = el("div", { class: "dd-fold-body" });
    if (!def) {
      body.appendChild(el("div", { class: "dd-fold-empty",
        text: "Could not locate def " + test.name + "() in any bundled spec." }));
      fold.appendChild(body);
      return fold;
    }
    // For matrix-generated tests, render a header explaining the
    // generation and link out to the scenario / fault assumption
    // defs that compose the test name.
    if (def.generated) {
      var hdr = el("div", { class: "dd-source-generated" });
      hdr.appendChild(el("div", { class: "dd-source-generated-lead",
        text: test.name + " is generated — there is no literal def in the spec." }));
      var bits = el("div", { class: "dd-source-generated-bits" });
      if (def.scenario) {
        bits.appendChild(el("span", { class: "dd-source-bit-label", text: "scenario" }));
        bits.appendChild(specJumpLink(
          def.scenario.def.path, def.scenario.def.line,
          "def " + def.scenario.name + "()"));
      }
      if (def.fault) {
        bits.appendChild(el("span", { class: "dd-source-bit-label", text: "fault" }));
        bits.appendChild(specJumpLink(
          def.fault.def.path, def.fault.def.line,
          def.fault.name + " = fault_assumption(...)"));
      }
      bits.appendChild(el("span", { class: "dd-source-bit-label", text: "matrix" }));
      bits.appendChild(specJumpLink(def.path, def.line, "fault_matrix(...)"));
      hdr.appendChild(bits);
      body.appendChild(hdr);
    }
    // Render a line-numbered viewer. Use a distinct DOM scope so the
    // same spec can also appear in the top-level section without id
    // collisions (anchors differ because the drill-down node is
    // transient and not globally addressable).
    var content = (specIndex && specIndex.byPath[def.path])
      ? specIndex.byPath[def.path].body
      : "";
    var pre = document.createElement("pre");
    pre.className = "source-code";
    var lines = content.split(/\r?\n/);
    var hitRow = null;
    for (var i = 0; i < lines.length; i++) {
      var lineNo = i + 1;
      var codeSpan = document.createElement("span");
      codeSpan.className = "source-line-code";
      codeSpan.innerHTML = highlightStarlark(lines[i]);
      var row = el("div", {
        class: "source-line" + (lineNo === def.line ? " hit" : ""),
        "data-line": String(lineNo),
      }, [
        el("span", { class: "source-line-no", text: String(lineNo) }),
      ]);
      row.appendChild(codeSpan);
      pre.appendChild(row);
      if (lineNo === def.line) hitRow = row;
    }
    body.appendChild(pre);
    fold.appendChild(body);

    // Scroll on open. Native <details> fires a toggle event we can
    // hook — no need for a MutationObserver.
    fold.addEventListener("toggle", function () {
      if (fold.open && hitRow) {
        requestAnimationFrame(function () {
          hitRow.scrollIntoView({ block: "center" });
        });
      }
    });
    return fold;
  }

  // collectProxyFaults walks a test's event list and pairs each
  // proxy_fault_applied with the matching proxy_fault_removed (if any)
  // so the rendered table reads "what was bent and on which target",
  // not a stream of raw lifecycle pings.
  function collectProxyFaults(events) {
    var applied = [];
    var removedBySig = {};
    for (var i = 0; i < events.length; i++) {
      var ev = events[i];
      if (!ev) continue;
      if (ev.type === "proxy_fault_applied") {
        applied.push(ev);
      } else if (ev.type === "proxy_fault_removed") {
        var f = ev.fields || {};
        var sig = (ev.service || "") + "|" + (f.protocol || "") + "|" +
                  (f.interface || "") + "|" + (f.assumption || "");
        removedBySig[sig] = ev;
      }
    }
    var out = [];
    for (var j = 0; j < applied.length; j++) {
      var a = applied[j];
      var fa = a.fields || {};
      var sig2 = (a.service || "") + "|" + (fa.protocol || "") + "|" +
                 (fa.interface || "") + "|" + (fa.assumption || "");
      var rem = removedBySig[sig2];
      out.push({
        service:    a.service || "",
        protocol:   fa.protocol || "",
        iface:      fa.interface || "",
        assumption: fa.assumption || "",
        applied_seq: a.seq || 0,
        removed_seq: rem ? (rem.seq || 0) : null,
      });
    }
    return out;
  }

  function proxyFaultsTable(rows) {
    var tbl = el("table", { class: "dd-faults" });
    var thead = el("thead", null, [el("tr", null, [
      el("th", { text: "Service" }),
      el("th", { text: "Protocol" }),
      el("th", { text: "Interface" }),
      el("th", { text: "Assumption" }),
      el("th", { text: "Applied / removed" }),
    ])]);
    var tbody = el("tbody");
    for (var i = 0; i < rows.length; i++) {
      var r = rows[i];
      var seqCell = "seq " + r.applied_seq;
      if (r.removed_seq) seqCell += " → " + r.removed_seq;
      else seqCell += " → (still active)";
      tbody.appendChild(el("tr", null, [
        el("td", null, [serviceRef(r.service)]),
        el("td", { text: r.protocol }),
        el("td", { text: r.iface }),
        el("td", { class: "mono", text: r.assumption }),
        el("td", { class: "mono", text: seqCell }),
      ]));
    }
    tbl.appendChild(thead);
    tbl.appendChild(tbody);
    return tbl;
  }

  function faultsTable(faults) {
    var tbl = el("table", { class: "dd-faults" });
    var thead = el("thead", null, [el("tr", null, [
      el("th", { text: "Service" }),
      el("th", { text: "Syscall" }),
      el("th", { text: "Action" }),
      el("th", { text: "Errno/detail" }),
      el("th", { text: "Hits" }),
      el("th", { text: "Label" }),
    ])]);
    var tbody = el("tbody");
    for (var i = 0; i < faults.length; i++) {
      var f = faults[i];
      var hitsCls = "";
      if (f.hits > 0) hitsCls = "hits-hot";
      else if (f.action && f.action !== "trace") hitsCls = "hits-zero";
      tbody.appendChild(el("tr", null, [
        el("td", null, [serviceRef(f.service || "")]),
        el("td", { text: f.syscall || "" }),
        el("td", { text: f.action || "" }),
        el("td", { text: f.errno || "" }),
        el("td", { class: hitsCls, text: String(f.hits != null ? f.hits : 0) }),
        el("td", { text: f.label || "" }),
      ]));
    }
    tbl.appendChild(thead);
    tbl.appendChild(tbody);
    return tbl;
  }

  // bypassedRulesTable renders the fault rules the runtime installed
  // but never matched. The shape mirrors the trace-layer BypassedRule
  // struct (service / syscall / action / label) — one row per rule.
  function bypassedRulesTable(rules) {
    var tbl = el("table", { class: "dd-faults" });
    var thead = el("thead", null, [el("tr", null, [
      el("th", { text: "Service" }),
      el("th", { text: "Syscall" }),
      el("th", { text: "Action" }),
      el("th", { text: "Label" }),
    ])]);
    var tbody = el("tbody");
    for (var i = 0; i < rules.length; i++) {
      var r = rules[i];
      tbody.appendChild(el("tr", null, [
        el("td", null, [serviceRef(r.service || "")]),
        el("td", { text: r.syscall || "" }),
        el("td", { text: r.action || "" }),
        el("td", { text: r.label || "" }),
      ]));
    }
    tbl.appendChild(thead);
    tbl.appendChild(tbody);
    return tbl;
  }

  // serviceRef returns a clickable <a> that closes the drill-down
  // and jumps to the service's definition line in the Spec section —
  // or plain text if no definition is found. Used anywhere a service
  // name appears inside the drill-down (faults table, topology, etc.)
  // so cross-linking is consistent across the report.
  function serviceRef(name) {
    if (!name) return document.createTextNode("");
    var def = findServiceLine(name);
    if (!def) return document.createTextNode(name);
    return el("a", {
      class: "service-link",
      href: "#" + specAnchorId(def.path, def.line),
      onclick: (function (p, l) {
        return function (e) {
          e.preventDefault();
          closeDrillDown();
          setTimeout(function () { focusSpecLine(p, l); }, 120);
        };
      })(def.path, def.line),
      text: name,
    });
  }

  // renderAssertion shows the structured "expected vs actual" block
  // produced by failing assert_eq / assert_true builtins. v0.12.3
  // also lifts the assertion expression directly out of the bundled
  // spec source — for assert_true(resp.status in [200, 201], "…")
  // showing only "Actual: False" was uninformative; the expression
  // text answers "what was being checked" without forcing the user
  // into the spec viewer.
  function renderAssertion(a, testEvents) {
    var grid = el("div", { class: "dd-assertion" });
    var head = el("div", { class: "dd-assertion-head" }, [
      el("span", { class: "dd-assertion-func mono", text: (a.func || "assert") + " failed" }),
    ]);
    if (a.message) {
      head.appendChild(el("span", { class: "dd-assertion-msg", text: a.message }));
    }
    grid.appendChild(head);

    var pairs = el("div", { class: "dd-assertion-pairs" });

    // Recover the original assertion expression from the spec source.
    // Best-effort: if the bundle didn't carry the spec or the call is
    // multi-line, the expression block is skipped silently and we
    // fall back to the bare Expected/Actual rows.
    var expr = lookupAssertionExpression(a);
    if (expr) {
      pairs.appendChild(el("div", { class: "dd-assertion-label", text: "Expression" }));
      pairs.appendChild(el("div", { class: "dd-assertion-value mono expression", text: expr }));
    }

    pairs.appendChild(el("div", { class: "dd-assertion-label", text: "Expected" }));
    pairs.appendChild(el("div", { class: "dd-assertion-value mono", text: a.expected || "—" }));
    pairs.appendChild(el("div", { class: "dd-assertion-label", text: "Actual" }));
    pairs.appendChild(el("div", { class: "dd-assertion-value mono actual", text: a.actual || "—" }));

    // Recent context: snapshot of the last few step events at fail
    // time, populated by the runtime in v0.12.4. Surfaces the
    // actual values Starlark folded away (e.g. resp.status = 500)
    // by reading the last step_recv's status_code field. Matches
    // the common test pattern of asserting on a freshly-returned
    // response — when the assertion is about a value 5 steps back
    // this misses, but it gets the 80% case right with no spec
    // changes from the user.
    if (a.context && a.context.length) {
      pairs.appendChild(el("div", { class: "dd-assertion-label", text: "Recent" }));
      pairs.appendChild(renderAssertionContext(a.context, testEvents || []));
    }
    if (a.file && a.line) {
      pairs.appendChild(el("div", { class: "dd-assertion-label", text: "Location" }));
      var loc = el("div", { class: "dd-assertion-value mono" });
      var path = resolveSpecPath(a.file);
      var label = (path || a.file).replace(/^spec\//, "") + ":" + a.line;
      if (path) {
        var link = el("a", {
          href: "#" + specAnchorId(path, a.line),
          class: "dd-assertion-link",
          text: label,
          onclick: (function (p, l) {
            return function (e) { e.preventDefault(); closeDrillDown(); setTimeout(function () { focusSpecLine(p, l); }, 120); };
          })(path, a.line),
        });
        loc.appendChild(link);
      } else {
        loc.appendChild(document.createTextNode(label));
      }
      pairs.appendChild(loc);
    }
    // Fade-and-expand wrapper. The grid sits inside a height-capped
    // container that uses CSS mask-image to fade the bottom of its
    // content to transparent — gradient-overlay-with-bg-color is
    // brittle (the assertion block has a pinkish .fail-bg, not the
    // panel bg). The toggle button sits below the clamp, never on
    // top of the text. Clicking transitions max-height with ease-out.
    var clampHost = el("div", { class: "dd-assertion-clamp clamped" }, [pairs]);
    var toggleWrap = el("div", { class: "dd-assertion-toggle-wrap" });
    var toggle = el("button", {
      class: "dd-assertion-toggle", type: "button",
      text: "Show full assertion ▾",
      onclick: function () {
        var open = clampHost.classList.toggle("clamped");
        toggle.textContent = open ? "Show full assertion ▾" : "Collapse ▴";
      },
    });
    toggleWrap.appendChild(toggle);

    // After mount: if the natural height already fits, drop the
    // clamp and hide the toggle entirely. No fade-affordance on a
    // 3-line assertion.
    requestAnimationFrame(function () {
      var threshold = parseFloat(getComputedStyle(clampHost).maxHeight) || 240;
      if (pairs.scrollHeight <= threshold + 8) {
        clampHost.classList.remove("clamped");
        toggleWrap.style.display = "none";
      }
    });

    grid.appendChild(clampHost);
    grid.appendChild(toggleWrap);
    return grid;
  }

  // renderAssertionContext renders the runtime-captured "what just
  // happened" trail next to Expected/Actual. Each row reads as a
  // mini step-event headline so the user sees the values that drove
  // the assertion result (status_code, error, success).
  //
  // testEvents (full event list for this test) is merged in to
  // surface fault events that fired between the captured steps —
  // without this row, a "← reply ERR: read EOF" looks unmotivated;
  // with the interleaved "fault redis_network_down" the causality
  // is immediately legible.
  function renderAssertionContext(ctx, testEvents) {
    var host = el("div", { class: "dd-assertion-value mono" });
    var list = el("ul", { class: "dd-assertion-context" });
    var rows = buildRecentRows(ctx, testEvents || []);
    for (var i = 0; i < rows.length; i++) {
      list.appendChild(rows[i]);
    }
    host.appendChild(list);
    return host;
  }

  // buildRecentRows merges step context with fault events from the
  // test's full event log, ordered by seq. Fault rows render with a
  // distinct class so the eye picks them out of the call/reply trail.
  function buildRecentRows(ctx, testEvents) {
    var minSeq = Infinity, maxSeq = -Infinity;
    var ctxBySeq = {};
    for (var i = 0; i < ctx.length; i++) {
      var c = ctx[i];
      var s = c.seq || c.Seq || 0;
      if (s < minSeq) minSeq = s;
      if (s > maxSeq) maxSeq = s;
      ctxBySeq[s] = c;
    }
    if (minSeq === Infinity) return [];

    // Window: fault events between the first captured step and the
    // last captured step (inclusive). The runtime captures steps at
    // assertion time, so faults inside this window are the ones that
    // shaped the failing values.
    var faultWindow = [];
    for (var j = 0; j < testEvents.length; j++) {
      var ev = testEvents[j];
      if (!ev) continue;
      var es = ev.seq || 0;
      if (es < minSeq || es > maxSeq) continue;
      var t = ev.type || "";
      if (t === "fault_applied" || t === "fault_removed" ||
          t === "proxy_fault_applied" || t === "proxy_fault_removed" ||
          t === "violation") {
        faultWindow.push(ev);
      }
    }

    // Merge by seq. Build a flat list of {kind, seq, payload}.
    var merged = [];
    for (var k = 0; k < ctx.length; k++) {
      merged.push({ kind: "step", seq: ctx[k].seq || 0, payload: ctx[k] });
    }
    for (var f = 0; f < faultWindow.length; f++) {
      merged.push({ kind: "fault", seq: faultWindow[f].seq || 0, payload: faultWindow[f] });
    }
    merged.sort(function (a, b) { return a.seq - b.seq; });

    var rows = [];
    for (var m = 0; m < merged.length; m++) {
      rows.push(merged[m].kind === "fault"
        ? recentFaultRow(merged[m].payload)
        : recentStepRow(merged[m].payload));
    }
    return rows;
  }

  function recentStepRow(c) {
    var label = c.type === "step_send" ? "→ call" : "← reply";
    var line = label + " · " + (c.target || "?")
      + (c.method ? "." + c.method : "")
      + (c.summary ? "  " + stripArrowFromSummary(c.summary) : "");
    if (c.status_code) line += "  [" + c.status_code + "]";
    if (c.success === "false" && c.error) line += "  ERR: " + c.error;
    var rowCls = c.success === "false" ? "fail" : "";
    return el("li", { class: rowCls }, [
      seqBadge(c.seq, rowCls),
      document.createTextNode(line),
    ]);
  }

  // seqBadge renders a tiny inline pill showing the event's seq.
  // Tone-class lifts the badge color from the row class so faults read
  // red, fails read red, ordinary steps stay neutral. Sits inline so
  // it doesn't push the rest of the line; mono font keeps it tidy.
  function seqBadge(seq, toneCls) {
    return el("span", {
      class: "dd-seq-badge" + (toneCls ? " " + toneCls : ""),
      text: String(seq || 0),
      title: "seq " + (seq || 0),
    });
  }

  function recentFaultRow(ev) {
    var f = ev.fields || {};
    var t = ev.type || "";
    var pre, body;
    if (t === "violation") {
      pre = "✗ violation";
      body = f.reason || "";
    } else if (t === "proxy_fault_applied" || t === "proxy_fault_removed") {
      pre = (t === "proxy_fault_applied" ? "+" : "−") + " proxy fault";
      var bits = [];
      if (f.assumption) bits.push(f.assumption);
      if (f.protocol) bits.push(f.protocol);
      body = bits.join(" · ");
    } else {
      pre = (t === "fault_applied" ? "+" : "−") + " fault";
      var fbits = [];
      if (f.label) fbits.push(f.label);
      if (f.syscall) fbits.push(f.syscall);
      if (f.action) fbits.push(f.action);
      if (f.errno) fbits.push(f.errno);
      body = fbits.join(" · ");
    }
    var text = pre + (body ? "  " + body : "");
    return el("li", { class: "fault" }, [
      seqBadge(ev.seq, "fault"),
      document.createTextNode(text),
    ]);
  }

  // The runtime-emitted summary already starts with "→" or "←", but
  // we render the arrow ourselves alongside type. Strip the leading
  // arrow to avoid `→ → db.exec`.
  function stripArrowFromSummary(s) {
    if (!s) return "";
    return s.replace(/^[→←]\s*/, "");
  }

  // resolveSpecPath maps a Starlark-reported file path (could be
  // absolute, relative, or a bundle-relative `spec/foo.star`) to a
  // key inside specIndex.byPath. Without this we'd fail to find the
  // assertion line whenever Starlark returned a path slightly
  // different from the bundle's storage convention.
  function resolveSpecPath(file) {
    if (!file || !specIndex || !specIndex.byPath) return null;
    if (specIndex.byPath[file]) return file;
    var withSpec = "spec/" + file.replace(/^\.\//, "");
    if (specIndex.byPath[withSpec]) return withSpec;
    // Suffix match: handles absolute paths from a different machine.
    var keys = Object.keys(specIndex.byPath);
    for (var i = 0; i < keys.length; i++) {
      if (file.indexOf(keys[i].replace(/^spec\//, "")) >= 0) return keys[i];
      if (keys[i].indexOf(file) >= 0) return keys[i];
    }
    return null;
  }

  // lookupAssertionExpression slices the assert_*(…) call's first
  // argument out of the bundled spec source. Returns "" when the
  // bundle lacks the file or when the call spans multiple lines —
  // keeps the renderer simple and silent on the unhappy paths.
  function lookupAssertionExpression(a) {
    if (!a || !a.file || !a.line) return "";
    var path = resolveSpecPath(a.file);
    if (!path) return "";
    var body = (specIndex.byPath[path] && specIndex.byPath[path].body) || "";
    var lines = body.split(/\r?\n/);
    var line = lines[a.line - 1] || "";
    return extractAssertionFirstArg(line, a.func || "assert_true");
  }

  // extractAssertionFirstArg walks the line character-by-character,
  // tracking paren / bracket / brace depth and string state, so a
  // first argument like `resp.status in [200, 201]` (which contains
  // a comma!) doesn't get truncated at the bracket-internal comma.
  function extractAssertionFirstArg(line, fnName) {
    if (!line || !fnName) return "";
    var open = fnName + "(";
    var idx = line.indexOf(open);
    if (idx < 0) return "";
    var start = idx + open.length;
    var depth = 0, inString = false, stringChar = 0, escape = false;
    for (var i = start; i < line.length; i++) {
      var c = line.charCodeAt(i);
      if (inString) {
        if (escape) { escape = false; continue; }
        if (c === 92) { escape = true; continue; }
        if (c === stringChar) inString = false;
        continue;
      }
      if (c === 34 || c === 39) { inString = true; stringChar = c; continue; }
      if (c === 40 || c === 91 || c === 123) { depth++; continue; }
      if (c === 41 || c === 93 || c === 125) {
        if (depth === 0) return line.substring(start, i).trim();
        depth--;
        continue;
      }
      if (c === 44 && depth === 0) return line.substring(start, i).trim();
    }
    return "";
  }

  function renderDiag(d) {
    var cls = "dd-diag";
    if (d.level === "error") cls += " error";
    else if (d.level === "warning") cls += " warning";
    var kids = [
      el("div", { class: "dd-diag-code", text: (d.level || "info") + " · " + (d.code || "") }),
      el("div", { class: "dd-diag-msg", text: d.message || "" }),
    ];
    if (d.suggestion) kids.push(el("div", { class: "dd-diag-fix", text: d.suggestion }));
    return el("div", { class: cls }, kids);
  }

  function drillDownReplay(test) {
    var cmd = "faultbox replay " + bundleBasename() + " --test " + test.name;
    var head = el("div", { class: "dd-replay-head" });
    var btn = el("button", {
      class: "copy-btn", type: "button", "aria-label": "Copy replay command",
      onclick: function () { copyToClipboard(cmd, btn); }, text: "Copy",
    });
    head.appendChild(el("span", { class: "mono",
      text: "Reproduce just this test with the original seed." }));
    head.appendChild(btn);
    var host = el("div", { class: "dd-replay" }, [head, el("code", { class: "mono", text: cmd })]);
    return host;
  }

  // ── Swim-lane trace viewer ──────────────────────────────────────
  //
  // Each service is one lane; time is the x-axis, derived from event
  // seq numbers (a total order within the log). Markers distinguish
  // syscalls, faults, lifecycle, step, and violation events.
  //
  // Interaction (v0.11.0):
  //   - Hover:  a small side-mounted balloon with event head +
  //             summary; SVG causal overlay connects the hovered
  //             marker to its ancestors on other lanes.
  //   - Click:  marker gets a persistent "selected" ring and the
  //             detail panel below the lanes switches to that event
  //             (structured: headline fields first, "all fields"
  //             and "vector clock" tucked behind collapsibles).
  //   - A companion event-log table below the detail panel gives a
  //             forensic row-per-event view with type-filter chips.
  //
  // Causality: vector_clock is used when present (proper
  // happens-before), with a fallback to "latest prior event per
  // other lane" when a bundle lacks vector clocks.
  function renderTrace(test) {
    var events = test.events || [];
    if (!events.length) {
      return el("div", { class: "trace-empty",
        text: "No events captured for this test. (Trace collection is enabled by default; empty traces usually mean the test ran entirely in the harness — no service syscalls executed.)" });
    }

    // Trace-level filter state. "Compact" hides framework lifecycle
    // chatter (proxy_started/_active, service_*, mock.*) so the
    // signal-to-noise on a real run is tractable; "Anchors only"
    // strips everything except faults / violations / errored steps;
    // "All events" reverts to historical behavior. Search filters
    // on event-type, headline text, and field values — the matrix
    // tests get hundreds of step events, search lets you jump to
    // the one with `redis_oom` in its summary.
    var traceFilter = { preset: "compact", search: "" };

    var host = el("div", { class: "trace-viewer" });

    // Pinned event survives lane rebuilds across filter changes.
    var pinned = { ev: null };

    // Closure-scoped DOM/state references that lane render touches.
    // Recreated on every rebuild but kept in `state` so other
    // components (event-log onSelect, marker handlers) reach the
    // current generation.
    var state = {
      lanesWrap: null, svg: null, tooltip: null,
      markerNodes: {}, markerEvBySeq: {}, laneIndexFor: {}, order: [],
    };
    var lanesContainer = el("div", { class: "trace-lanes-host" });
    var axisHost = el("div");
    var legendHost = el("div");
    var detail = el("div", { class: "trace-detail empty",
      text: "Click any point in the timeline to see its details here." });

    function rebuildLanes() {
      var laneEvents = events.filter(function (e) {
        return isLaneEvent(e) && passesTraceFilter(e, traceFilter);
      });
      if (!laneEvents.length) {
        // Filter wiped the lane clean. Show a placeholder rather
        // than collapsing to zero height.
        lanesContainer.innerHTML = "";
        lanesContainer.appendChild(el("div", { class: "trace-empty",
          text: "No events match the current filter." }));
        axisHost.innerHTML = "";
        legendHost.innerHTML = "";
        state.markerNodes = {};
        state.markerEvBySeq = {};
        state.order = [];
        return;
      }
      var hiddenFiltered = events.length - laneEvents.length;

      // Group into lanes, apply per-lane budget (preserve existing
      // fold-by-key behavior).
      var lanes = {};
      var order = [];
      for (var i = 0; i < laneEvents.length; i++) {
        var svc = laneFor(laneEvents[i]);
        if (!lanes[svc]) { lanes[svc] = []; order.push(svc); }
        lanes[svc].push(laneEvents[i]);
      }
      var hiddenSteps = 0;
      var budgetedLane = [];
      for (var li = 0; li < order.length; li++) {
        var svc = order[li];
        var result = applyLaneBudgetFilter(lanes[svc]);
        lanes[svc] = result.kept;
        hiddenSteps += result.foldedCount;
        for (var ki = 0; ki < result.kept.length; ki++) budgetedLane.push(result.kept[ki]);
      }
      laneEvents = budgetedLane;

      // Rank-based positioning (preserve existing layout).
      var sortedLane = laneEvents.slice().sort(function (a, b) {
        return (a.seq || 0) - (b.seq || 0);
      });
      var seqRank = {};
      for (var r = 0; r < sortedLane.length; r++) seqRank[sortedLane[r].seq] = r;
      var laneCount = sortedLane.length;
      var firstSeq = sortedLane[0].seq || 0;
      var lastSeq = sortedLane[laneCount - 1].seq || 0;

      var lanesWrap = el("div", { class: "trace-lanes" });
      var grid = el("div", { class: "trace-grid" });
      grid.style.gridTemplateColumns = "max-content 1fr";

      var markerNodes = {};
      var markerEvBySeq = {};
      var laneIndexFor = {};
      for (var j = 0; j < order.length; j++) {
        var svc = order[j];
        laneIndexFor[svc] = j;
        grid.appendChild(el("div", { class: "trace-lane-label", text: svc }));
        var lane = el("div", { class: "trace-lane" });
        var laneEv = lanes[svc];
        for (var k = 0; k < laneEv.length; k++) {
          var ev = laneEv[k];
          var m = renderMarker(ev, seqRank, laneCount);
          markerNodes[ev.seq] = m;
          markerEvBySeq[ev.seq] = ev;
          lane.appendChild(m);
        }
        grid.appendChild(lane);
      }
      lanesWrap.appendChild(grid);

      var svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
      svg.setAttribute("class", "trace-causal-svg");
      lanesWrap.appendChild(svg);

      var tooltip = el("div", { class: "trace-tooltip", role: "tooltip" });
      lanesWrap.appendChild(tooltip);

      // Wire marker handlers — same closures as before but routed
      // through the current `state` (so a stale rebuild's lambdas
      // don't fire after a newer filter change replaces them).
      var lookup = {};
      for (var i2 = 0; i2 < events.length; i2++) lookup[events[i2].seq] = events[i2];
      for (var s in markerNodes) {
        (function (ev, node) {
          node.addEventListener("mouseenter", function () {
            showTraceHover(ev, events, state.lanesWrap, node, state.tooltip, state.svg,
              state.markerNodes, state.order, state.laneIndexFor);
          });
          node.addEventListener("focus", function () {
            showTraceHover(ev, events, state.lanesWrap, node, state.tooltip, state.svg,
              state.markerNodes, state.order, state.laneIndexFor);
          });
          node.addEventListener("mouseleave", function () {
            hideTraceHover(state.lanesWrap, state.tooltip, state.svg, state.markerNodes,
              events, pinned);
          });
          node.addEventListener("blur", function () {
            hideTraceHover(state.lanesWrap, state.tooltip, state.svg, state.markerNodes,
              events, pinned);
          });
          node.addEventListener("click", function (e) {
            e.stopPropagation();
            pinSelection(ev, host, state.markerNodes, detail, events, state.svg,
              state.order, state.laneIndexFor, pinned);
          });
          node.addEventListener("keydown", function (e) {
            if (e.key === "Enter" || e.key === " ") {
              e.preventDefault();
              pinSelection(ev, host, state.markerNodes, detail, events, state.svg,
                state.order, state.laneIndexFor, pinned);
            }
          });
        })(markerEvBySeq[s] || lookup[s], markerNodes[s]);
      }

      // Replace lane DOM + commit state.
      lanesContainer.innerHTML = "";
      lanesContainer.appendChild(lanesWrap);
      state.lanesWrap = lanesWrap;
      state.svg = svg;
      state.tooltip = tooltip;
      state.markerNodes = markerNodes;
      state.markerEvBySeq = markerEvBySeq;
      state.laneIndexFor = laneIndexFor;
      state.order = order;

      // Refresh axis + legend captions.
      var axisRight = laneCount + " marker" + (laneCount === 1 ? "" : "s");
      var hiddenBits = [];
      if (hiddenSteps > 0) hiddenBits.push(hiddenSteps + " folded into slots");
      if (hiddenFiltered > 0) hiddenBits.push(hiddenFiltered + " hidden by filter");
      if (hiddenBits.length) axisRight += " · " + hiddenBits.join(" · ");
      axisHost.innerHTML = "";
      axisHost.appendChild(el("div", { class: "trace-axis" }, [
        el("span", { text: "seq " + firstSeq + " → " + lastSeq }),
        el("span", { text: axisRight }),
      ]));
      legendHost.innerHTML = "";
      legendHost.appendChild(traceLegend());

      // Re-pin if the previously-pinned event is still visible;
      // otherwise drop the pin so the detail panel doesn't show
      // stale data for an event the user just filtered out.
      if (pinned.ev && markerNodes[pinned.ev.seq]) {
        pinSelection(pinned.ev, host, state.markerNodes, detail, events, state.svg,
          state.order, state.laneIndexFor, pinned);
      } else if (pinned.ev) {
        pinned.ev = null;
        detail.classList.add("empty");
        detail.innerHTML = "Click any point in the timeline to see its details here.";
      }
    }

    // Filter bar UI.
    host.appendChild(buildTraceFilterBar(traceFilter, function () {
      rebuildLanes();
      // Event log honors the same filter — toggle a flag on the log
      // so `applyFilters` re-runs against the new traceFilter.
      if (host._refreshEventLog) host._refreshEventLog();
    }));

    host.appendChild(lanesContainer);
    host.appendChild(axisHost);
    host.appendChild(legendHost);
    host.appendChild(detail);
    rebuildLanes();

    // Event log table — passes traceFilter so its filter chips layer
    // on top of the global preset/search.
    events._sourceTest = test;
    var eventLog = buildEventLog(events, state.order, function (ev) {
      pinSelection(ev, host, state.markerNodes, detail, events, state.svg,
        state.order, state.laneIndexFor, pinned);
    }, traceFilter, function (refreshFn) { host._refreshEventLog = refreshFn; });
    host.appendChild(eventLog);

    return host;
  }

  // Event types that count as framework chatter — proxy lifecycle,
  // service lifecycle, mock harness lifecycle. Hidden by default in
  // Compact mode; included in All-events.
  var FRAMEWORK_TYPES = {
    proxy_started: 1, proxy_active: 1,
    service_started: 1, service_ready: 1, service_stopped: 1,
    service_seed: 1, service_reset: 1,
    session_completed: 1,
  };
  function isFrameworkEvent(ev) {
    var t = ev.type || "";
    if (FRAMEWORK_TYPES[t]) return true;
    if (t.indexOf("mock.") === 0) return true;
    return false;
  }

  function passesTraceFilter(ev, filter) {
    if (filter.preset === "anchors") {
      // Cause-relevant only: faults, violations, errored steps.
      // Reuses the same predicate as causal-link drawing for
      // consistency between the two views.
      if (!isCauseCandidate(ev)) return false;
    } else if (filter.preset === "compact") {
      if (isFrameworkEvent(ev)) return false;
    }
    if (filter.search) {
      var hay = (ev.type || "") + " " + (eventHeadline(ev, { full: true }) || "");
      if (ev.fields) {
        for (var k in ev.fields) hay += " " + k + " " + (ev.fields[k] || "");
      }
      if (ev.service) hay += " " + ev.service;
      if (hay.toLowerCase().indexOf(filter.search.toLowerCase()) === -1) return false;
    }
    return true;
  }

  function buildTraceFilterBar(filter, onChange) {
    var wrap = el("div", { class: "trace-filter-bar" });

    var presets = el("div", { class: "trace-filter-presets" });
    var presetButtons = {};
    function addPreset(value, label, title) {
      var btn = el("button", {
        class: "trace-filter-preset" + (filter.preset === value ? " active" : ""),
        type: "button", title: title, text: label,
        onclick: function () {
          if (filter.preset === value) return;
          filter.preset = value;
          for (var k in presetButtons) {
            presetButtons[k].classList.toggle("active", k === value);
          }
          onChange();
        },
      });
      presetButtons[value] = btn;
      presets.appendChild(btn);
    }
    addPreset("compact", "Compact", "Hide framework lifecycle (proxy/service/mock startup chatter)");
    addPreset("anchors", "Anchors only", "Faults, violations, and errored steps only — drop everything chronological");
    addPreset("all", "All events", "Show every event the bundle preserved (historical default)");
    wrap.appendChild(presets);

    var searchWrap = el("div", { class: "trace-filter-search" });
    var search = el("input", {
      type: "search", placeholder: "Search events… (type, headline, fields)",
      class: "trace-filter-search-input",
      oninput: function () {
        filter.search = search.value || "";
        onChange();
      },
    });
    searchWrap.appendChild(search);
    wrap.appendChild(searchWrap);

    return wrap;
  }

  // pinSelection is the single entry-point for "user picked this
  // event": it clears the previous selection's ring, sets the new
  // one, renders the detail panel, keeps the causal overlay live
  // for the pinned marker, and marks the matching row in the log.
  function pinSelection(ev, host, markerNodes, detail, events, svg,
                        order, laneIndexFor, pinned) {
    // Clear previous selection markers and row highlights.
    var prev = host.querySelector(".trace-marker.selected");
    if (prev) prev.classList.remove("selected");
    var prevRow = host.querySelector(".trace-log-table tr.selected");
    if (prevRow) prevRow.classList.remove("selected");
    clearHighlights(host);

    pinned.ev = ev;

    var node = markerNodes[ev.seq];
    if (node) node.classList.add("selected");

    // Render detail panel.
    detail.classList.remove("empty");
    detail.innerHTML = "";
    detail.appendChild(buildDetailContent(ev, events));

    // Open the event log if closed so scroll + highlight land in view.
    var log = host.querySelector(".trace-log");
    if (log && !log.open) log.open = true;

    // Highlight matching log row but DO NOT scroll. v0.12.3: customers
    // reported that clicking a lane marker yanked the page down to the
    // event log every time. The selection class still applies so the
    // user finds the row when they manually scroll, but no jump.
    var row = host.querySelector('.trace-log-table tr[data-seq="' + ev.seq + '"]');
    if (row) row.classList.add("selected");

    // Persist causal overlay for the pinned event + ring the ancestors.
    var lanesWrap = host.querySelector(".trace-lanes");
    drawPinnedCausal(ev, events, svg, lanesWrap, markerNodes);
  }

  // drawPinnedCausal paints the causal lines and ancestor rings for
  // the pinned event. Called from pinSelection and from
  // hideTraceHover (so the pinned overlay survives hover teardown).
  function drawPinnedCausal(ev, events, svg, lanesWrap, markerNodes) {
    clearHighlights(lanesWrap);
    var node = markerNodes[ev.seq];
    if (!node) return;
    node.classList.add("highlight-root");
    lanesWrap.classList.add("causal-active");
    var ancestors = findCausalAncestors(ev, events);
    drawCausalLines(svg, node, ancestors, lanesWrap, markerNodes);
  }

  function clearHighlights(scope) {
    var nodes = scope.querySelectorAll(".trace-marker.highlight-root, .trace-marker.highlight-ancestor");
    for (var i = 0; i < nodes.length; i++) {
      nodes[i].classList.remove("highlight-root", "highlight-ancestor");
    }
    scope.classList && scope.classList.remove("causal-active");
  }

  // isLaneEvent decides whether an event earns a marker on the swim
  // lane. The visual lane is a coarse "what mattered" view — anything
  // higher-cardinality than once-per-step lives in the event log
  // table below. Anything not explicitly listed here also gets a
  // marker (forward-compat for new event kinds).
  function isLaneEvent(ev) {
    var t = ev && ev.type;
    if (!t) return true;
    if (t === "syscall") return false;
    return true;
  }

  // laneFor decides which lane an event belongs to. v0.12.7 routes
  // step_send / step_recv to their *target* service rather than the
  // emitter ("test"): a `db.exec(...)` call that fails surfaces on
  // the db lane (where the user expects to see DB activity), not on
  // the test lane buried among other interactions. Falls back to
  // ev.service when fields.target is missing — keeps older bundles
  // and non-step events on their original lanes.
  function laneFor(ev) {
    if ((ev.type === "step_send" || ev.type === "step_recv") &&
        ev.fields && ev.fields.target) {
      return ev.fields.target;
    }
    return ev.service || "test";
  }

  // applyLaneBudgetFilter shapes the lane down to a manageable
  // marker count. Two passes:
  //
  // 1. Fold by key — a `db.exec SELECT 1 ERR` repeated 1737 times
  //    collapses to a single `× 1737` chip at its median rank, no
  //    matter where in the trace the events sit. This addresses the
  //    "db lane is 50 identical red dots" symptom: identical-summary
  //    events share one visual position so the user sees the chip
  //    plus the count.
  //
  // 2. Slot budget — if the post-fold list still exceeds LANE_BUDGET,
  //    the remaining markers bucket into 50 visual slots in seq order
  //    and each slot picks a representative (severity-weighted, with
  //    a fold-key head fallback).
  //
  // Hard guarantee: ≤ LANE_BUDGET DOM markers per lane regardless of
  // input. Per-lane counts are now usually well under the budget for
  // typical proxy-mode traces because most lanes have only a few
  // unique fold keys.
  var LANE_BUDGET = 50;
  var LANE_FOLD_THRESHOLD = 10;

  function applyLaneBudgetFilter(evs) {
    if (!evs.length) return { kept: [], foldedCount: 0 };

    // v0.12.10: spec-anchored events (the user's own step calls)
    // bypass the fold entirely — always render individually. They're
    // rare by definition (a typical test has 1–10) and represent the
    // user's mental model of the test, so they should never fold
    // into a chip alongside background traffic.
    var passthrough = []; // {pos, ev}

    // Pass 1: fold-by-key. Group by (target.method.summary). Groups
    // larger than LANE_FOLD_THRESHOLD collapse to one chip at their
    // median position; smaller groups stay as individual markers.
    var groups = {};
    var groupOrder = [];
    for (var i = 0; i < evs.length; i++) {
      if (evs[i].fields && evs[i].fields.spec) {
        passthrough.push({ pos: i, ev: evs[i] });
        continue;
      }
      var key = laneFoldKey(evs[i]);
      if (!groups[key]) {
        groups[key] = { positions: [], head: evs[i] };
        groupOrder.push(key);
      }
      groups[key].positions.push(i);
    }

    var foldedCount = 0;
    var emit = passthrough.slice(); // {pos, ev}
    for (var k = 0; k < groupOrder.length; k++) {
      var g = groups[groupOrder[k]];
      if (g.positions.length > LANE_FOLD_THRESHOLD) {
        // Pick the highest-severity event in the group as the head
        // so the chip's color/shape signals the worst case (e.g. a
        // group dominated by errored steps shows red, not yellow).
        var head = g.head;
        for (var hi = 0; hi < g.positions.length; hi++) {
          if (severityScore(evs[g.positions[hi]]) > severityScore(head)) {
            head = evs[g.positions[hi]];
          }
        }
        var med = g.positions[Math.floor(g.positions.length / 2)];
        var chip = {};
        for (var f in head) chip[f] = head[f];
        chip._runCount = g.positions.length;
        chip._runMembers = g.positions.map(function (p) { return evs[p].seq; });
        foldedCount += g.positions.length - 1;
        emit.push({ pos: med, ev: chip });
      } else {
        for (var p = 0; p < g.positions.length; p++) {
          emit.push({ pos: g.positions[p], ev: evs[g.positions[p]] });
        }
      }
    }
    emit.sort(function (a, b) { return a.pos - b.pos; });

    if (emit.length <= LANE_BUDGET) {
      return { kept: emit.map(function (x) { return x.ev; }), foldedCount: foldedCount };
    }

    // Pass 2: slot budget. The post-fold list is still over budget
    // (lots of unique fold keys), so bucket into 50 visual slots
    // and pick a severity-weighted representative per slot.
    var n = emit.length;
    var step = n / LANE_BUDGET;
    var kept = [];

    for (var s = 0; s < LANE_BUDGET; s++) {
      var lo = Math.floor(s * step);
      var hi = (s === LANE_BUDGET - 1) ? n : Math.floor((s + 1) * step);
      if (hi <= lo) continue;
      var slot = emit.slice(lo, hi).map(function (x) { return x.ev; });

      var rep = null;
      var bestScore = -1;
      for (var j = 0; j < slot.length; j++) {
        var sc = severityScore(slot[j]);
        if (sc > bestScore) { bestScore = sc; rep = slot[j]; }
      }
      if (!rep || bestScore <= 0) {
        var counts = {}, heads = {};
        for (var j = 0; j < slot.length; j++) {
          var lk = laneFoldKey(slot[j]);
          counts[lk] = (counts[lk] || 0) + 1;
          if (!heads[lk]) heads[lk] = slot[j];
        }
        var maxK = null, maxC = 0;
        for (var lk in counts) {
          if (counts[lk] > maxC) { maxC = counts[lk]; maxK = lk; }
        }
        rep = heads[maxK] || slot[0];
      }

      var chip = {};
      for (var k in rep) chip[k] = rep[k];
      if (slot.length > 1) {
        // Aggregate counts and members from all slot occupants
        // (some of which may already be folded chips).
        var total = 0, members = [];
        for (var j = 0; j < slot.length; j++) {
          var c = slot[j]._runCount || 1;
          total += c;
          if (slot[j]._runMembers) {
            for (var mIdx = 0; mIdx < slot[j]._runMembers.length; mIdx++) {
              members.push(slot[j]._runMembers[mIdx]);
            }
          } else {
            members.push(slot[j].seq);
          }
        }
        chip._runCount = total;
        chip._runMembers = members;
        foldedCount += slot.length - 1;
      }
      kept.push(chip);
    }
    return { kept: kept, foldedCount: foldedCount };
  }

  // severityScore ranks events for slot-representative picking. Higher
  // wins. Violations and faults are the strongest signals; failed
  // step events come next; lifecycle and ordinary steps are quietest.
  // Score 0 means "use the fold-head fallback" (no urgent signal).
  //
  // v0.12.10: spec-anchored steps (events the user wrote directly in
  // the test body) get a +50 bump — a slot containing both a monitor
  // poll and a user-written step always shows the user's call.
  function severityScore(ev) {
    var t = ev.type || "";
    var bonus = (ev.fields && ev.fields.spec) ? 50 : 0;
    if (t === "violation") return 100 + bonus;
    if (t === "fault_applied" || t === "fault_removed" ||
        t === "proxy_fault_applied" || t === "proxy_fault_removed") return 90 + bonus;
    if (t === "fault_zero_traffic" || t === "fault_skipped_no_seccomp") return 85 + bonus;
    if (t === "step_send" || t === "step_recv") {
      var f = ev.fields || {};
      if (f.success === "false" || f.error) return 80 + bonus;
      var sc = parseInt(f.status_code || "0", 10);
      if (sc >= 500) return 75 + bonus;
      if (sc >= 400) return 60 + bonus;
      return bonus; // spec-anchored happy-path steps still beat zero
    }
    if (t === "service_started" || t === "service_ready" ||
        t === "service_stopped" || t === "session_completed") return 30;
    return 0;
  }

  // isAnchorEvent flags events that pin the surrounding window of
  // detail. Mirrors the bundle-side anchor list (see report.go's
  // anchorTypes) plus errored step events — `success=false` is the
  // user's "this is the bad request" tag and we don't want it to
  // disappear into a fold.
  function isAnchorEvent(ev) {
    var t = ev.type || "";
    if (t === "fault_applied" || t === "fault_removed" ||
        t === "proxy_fault_applied" || t === "proxy_fault_removed" ||
        t === "fault_skipped_no_seccomp" || t === "fault_zero_traffic" ||
        t === "violation" ||
        t === "service_started" || t === "service_ready" || t === "service_stopped" ||
        t === "session_completed") {
      return true;
    }
    if ((t === "step_send" || t === "step_recv") && ev.fields) {
      if (ev.fields.success === "false") return true;
      if (ev.fields.error) return true;
    }
    return false;
  }

  // laneFoldKey is the cardinality-fold partition. Step events fold
  // by (target, method, summary) so polling SELECT 1 collapses while
  // INSERT INTO orders stays distinct. Other event types fold by
  // their type, but in practice non-step events are anchors and
  // never reach this path.
  function laneFoldKey(ev) {
    var t = ev.type || "";
    if (t !== "step_send" && t !== "step_recv") return "_typed_" + t;
    var f = ev.fields || {};
    var summary = f.summary || (f.sql || f.query || f.path || f.command || f.topic || "");
    return "step:" + (f.target || "?") + "." + (f.method || "?") + "::" + summary;
  }

  function renderMarker(ev, seqRank, laneCount) {
    // Rank-based positioning: each kept event gets the same horizontal
    // spacing as its neighbour, regardless of how many syscalls were
    // emitted in between. Without this an 80k-event run renders only
    // its first and last markers at the lane edges — everything in the
    // middle clamps to a few-pixel gap by linear seq scaling.
    var rank = seqRank[ev.seq] || 0;
    var denom = laneCount > 1 ? (laneCount - 1) : 1;
    var pct = (rank / denom) * 100;
    // Clamp to a narrow inner band so the first and last markers don't
    // overhang the lane's rounded corners.
    if (pct < 2.5) pct = 2.5;
    if (pct > 97.5) pct = 97.5;
    var kind = markerKind(ev);
    var label = markerShortLabel(ev);
    var cls = "trace-marker " + kind;
    if (ev._runCount && ev._runCount > 1) cls += " run";
    if (ev.fields && ev.fields.spec) cls += " spec";
    var m = el("div", {
      class: cls,
      tabindex: "0",
      role: "button",
      "aria-label": label,
      "data-seq": String(ev.seq || 0),
    });
    m.style.left = pct.toFixed(2) + "%";
    // v0.12.9: marker radius scales with log10(count + 1). When two
    // chips' badges overlap (small lane, many folds) the badge text
    // becomes unreadable — but the marker disc is always visible, so
    // sizing it proportional to the fold count keeps magnitude
    // legible even in dense regions. Base 8px → ~26px at count=10000.
    if (ev._runCount && ev._runCount > 1) {
      var logN = Math.log10(ev._runCount + 1);
      var size = Math.min(28, 8 + Math.round(logN * 6));
      m.style.width = size + "px";
      m.style.height = size + "px";
      m.appendChild(el("span", {
        class: "trace-marker-count",
        text: "×" + compactCount(ev._runCount),
        title: ev._runCount + " " + (ev.fields ? ((ev.fields.target || "?") + "." + (ev.fields.method || "?")) : "step") + " events folded into this marker",
      }));
    }
    // v0.12.8: stash folded member seqs on the DOM node so causal-
    // line drawing can resolve an ancestor seq to its containing
    // chip (otherwise folded ancestors silently fail the lookup
    // and the line skips them).
    if (ev._runMembers && ev._runMembers.length) {
      m.dataset.members = ev._runMembers.join(",");
    }
    return m;
  }

  function markerKind(ev) {
    var t = ev.type || "";
    if (t === "fault_applied" || t === "fault_removed" ||
        t === "proxy_fault_applied" || t === "proxy_fault_removed") return "fault";
    if (t === "violation") return "violation";
    if (t === "syscall") return "syscall";
    if (t === "step_send" || t === "step_recv") {
      // v0.12.6: failed steps render in the fault palette so the eye
      // finds the DB invalid-connection and the truck-api 500 among
      // a sea of yellow SELECT 1 markers. Without this every step
      // looks identical and the user has to click each one.
      var f = ev.fields || {};
      if (f.success === "false" || f.error) return "fault";
      var sc = parseInt(f.status_code || "0", 10);
      if (sc >= 500) return "fault";
      if (sc >= 400) return "violation";
      return "step";
    }
    if (t === "service_started" || t === "service_ready" ||
        t === "service_stopped" || t === "session_completed") return "lifecycle";
    return "syscall";
  }

  function markerShortLabel(ev) {
    var parts = [ev.type || "event"];
    if (ev.fields && ev.fields.syscall) parts.push(ev.fields.syscall);
    if (ev.service) parts.push("on " + ev.service);
    return parts.join(" ");
  }

  // showTraceHover positions the balloon to the side of the marker
  // (not directly above) so the causal lines we draw across lanes
  // stay visible. We also compute causal ancestors and paint them.
  function showTraceHover(ev, allEvents, lanesWrap, node, tooltip, svg,
                          markerNodes, order, laneIndexFor) {
    tooltip.innerHTML = "";
    tooltip.appendChild(buildTooltipContent(ev));

    var rect = node.getBoundingClientRect();
    var lanesRect = lanesWrap.getBoundingClientRect();
    var cx = rect.left - lanesRect.left + rect.width / 2;
    var cy = rect.top - lanesRect.top + rect.height / 2;

    // Pick a side that leaves the lines visible. Default: top-right
    // of marker. Flip to top-left if we'd clip the viewer on the right.
    // Flip to bottom-* if we're on the top lane.
    var offsetX = 14;
    var offsetY = -14;
    var svc = ev.service || "test";
    var laneIdx = laneIndexFor[svc];
    var isTopLane = (laneIdx === 0);
    if (isTopLane) offsetY = 18;

    tooltip.style.left = (cx + offsetX) + "px";
    tooltip.style.top = (cy + offsetY) + "px";
    tooltip.style.transform = isTopLane ? "translate(0, 0)" : "translate(0, -100%)";
    tooltip.classList.add("active");

    // Clamp against viewport (not just the lanes container) so a wide
    // tooltip on a marker near any screen edge stays fully visible.
    // Two passes: horizontal flip first, then per-side nudge.
    requestAnimationFrame(function () {
      var pad = 8;
      var tr = tooltip.getBoundingClientRect();
      var vw = window.innerWidth || document.documentElement.clientWidth;
      var vh = window.innerHeight || document.documentElement.clientHeight;

      if (tr.right > vw - pad) {
        var flipped = cx - offsetX - tr.width;
        tooltip.style.left = flipped + "px";
        tr = tooltip.getBoundingClientRect();
      }
      if (tr.left < pad) {
        var dx = pad - tr.left;
        var curLeft = parseFloat(tooltip.style.left) || 0;
        tooltip.style.left = (curLeft + dx) + "px";
        tr = tooltip.getBoundingClientRect();
      }
      if (tr.right > vw - pad) {
        var dx2 = tr.right - (vw - pad);
        var curLeft2 = parseFloat(tooltip.style.left) || 0;
        tooltip.style.left = (curLeft2 - dx2) + "px";
        tr = tooltip.getBoundingClientRect();
      }
      if (tr.top < pad) {
        // Bottom-anchored translate(0,-100%) clipped the top — flip
        // below the marker.
        tooltip.style.transform = "translate(0, 0)";
        tooltip.style.top = (cy + 18) + "px";
        tr = tooltip.getBoundingClientRect();
      }
      if (tr.bottom > vh - pad) {
        // Top-anchored placement clipped the bottom — flip above.
        tooltip.style.transform = "translate(0, -100%)";
        tooltip.style.top = (cy - 14) + "px";
      }
    });

    node.classList.add("highlight-root");
    lanesWrap.classList.add("causal-active");

    var ancestors = findCausalAncestors(ev, allEvents);
    drawCausalLines(svg, node, ancestors, lanesWrap, markerNodes);
  }

  function hideTraceHover(lanesWrap, tooltip, svg, markerNodes, events, pinned) {
    tooltip.classList.remove("active");
    svg.innerHTML = "";
    clearHighlights(lanesWrap);
    // Restore the pinned overlay so hovering a non-pinned marker
    // and then leaving it doesn't wipe the pinned event's causal
    // context. Hover always wins while the mouse is over a marker;
    // leaving returns to the pinned view.
    if (pinned && pinned.ev) {
      drawPinnedCausal(pinned.ev, events, svg, lanesWrap, markerNodes);
    }
  }

  function buildTooltipContent(ev) {
    // v0.12.4: prefer the runtime-emitted summary as the headline so
    // a step balloon reads call/reply context instead of the bare
    // type. v0.12.9: wrap the summary with explicit call/reply
    // word so first-time readers don't have to interpret the
    // arrow on its own.
    var f = ev.fields || {};
    var headline;
    if (ev.type === "step_send" || ev.type === "step_recv") {
      var label = ev.type === "step_send" ? "→ call" : "← reply";
      headline = f.summary ? (label + " · " + stripArrowFromSummary(f.summary)) : label;
    } else {
      headline = f.summary || ev.type || "event";
    }
    if (f.spec) headline = "★ " + headline;
    var head = el("div", { class: "trace-tooltip-head", text: headline });
    var sub = el("div", { class: "trace-tooltip-sub" });
    var bits = [];
    if (ev.service) bits.push(ev.service);
    if (f.syscall) bits.push(f.syscall);
    if (f.decision) bits.push("[" + f.decision + "]");
    if (f.status_code && !f.summary) bits.push("status=" + f.status_code);
    if (f.error && f.success === "false") bits.push("ERR: " + truncate(f.error, 50));
    bits.push("seq " + (ev.seq || 0));
    sub.textContent = bits.join(" · ");
    return el("div", null, [head, sub]);
  }

  // buildDetailContent is the pinned-selection view shown below the
  // lanes. Headline fields (type, service, primary summary) sit at
  // the top; everything else tucks into collapsibles so the common
  // case stays compact.
  function buildDetailContent(ev, events) {
    var head = el("div", { class: "trace-detail-head" }, [
      el("span", { class: "trace-detail-title", text: ev.type || "event" }),
      el("span", { class: "trace-detail-meta",
        text: (ev.service || "—") + " · seq " + (ev.seq || 0) +
              (ev.timestamp ? " · " + fmtTimestamp(ev.timestamp) : "") }),
    ]);

    var primary = el("div");
    primary.appendChild(detailRow("service", ev.service || "—", true));
    var summary = eventHeadline(ev, { full: true });
    if (summary) primary.appendChild(detailRow("summary", summary, true));
    if (ev._runCount && ev._runCount > 1) {
      // Surface the dedup pivot — Boris's "I see 81k events but only 3
      // markers" complaint becomes "I see 3 markers, two are runs of
      // 1500 and 22000 events respectively." The members list lets a
      // user jump into the event-log table to inspect any specific
      // iteration.
      var members = ev._runMembers || [];
      var memberSummary = ev._runCount + " consecutive event"
        + (ev._runCount === 1 ? "" : "s") + " collapsed"
        + (members.length ? " (seq " + members[0] + " → " + members[members.length - 1] + ")" : "");
      primary.appendChild(detailRow("collapsed run", memberSummary, false));
    }
    if (ev.fields) {
      // Promote the two fields most users need to see first.
      if (ev.fields.syscall) primary.appendChild(detailRow("syscall", ev.fields.syscall, false));
      if (ev.fields.decision) primary.appendChild(detailRow("decision", ev.fields.decision, false));
      if (ev.fields.path) primary.appendChild(detailRow("path", ev.fields.path, false));
      if (ev.fields.reason && ev.type === "violation")
        primary.appendChild(detailRow("reason", ev.fields.reason, false));
    }

    var container = el("div", null, [head, primary]);

    // Collapsible: full field dump (everything not already promoted).
    var hiddenKeys = { syscall: 1, decision: 1, path: 1, reason: 1 };
    var extras = [];
    if (ev.fields) {
      var keys = Object.keys(ev.fields).sort();
      for (var i = 0; i < keys.length; i++) {
        var k = keys[i];
        if (hiddenKeys[k]) continue;
        extras.push(detailRow(k, ev.fields[k], false));
      }
    }
    if (ev.event_type && ev.event_type !== ev.type)
      extras.unshift(detailRow("event_type", ev.event_type, false));
    if (ev.partition_key)
      extras.unshift(detailRow("partition", ev.partition_key, false));

    // Always render both collapsibles (with an empty-state line when
    // absent) so the panel's height is consistent across different
    // event types — prevents the surrounding layout from twitching
    // as the user clicks between markers.
    container.appendChild(collapsible("All fields",
      extras.length ? extras :
        [el("div", { class: "trace-detail-kv trace-detail-value",
          text: "(no additional fields)" })]));

    // Group members: when this marker folded a run of similar events,
    // render the full member list inside a collapsible (matches "All
    // fields" / "Vector clock" affordance). Capped at 100 rows in a
    // scrollable box so a 22k-event fold doesn't blow up the panel.
    if (ev._runMembers && ev._runMembers.length > 1 && events && events.length) {
      container.appendChild(collapsible(
        "Group members (" + ev._runMembers.length + ")",
        [groupMembersTable(ev._runMembers, events)]));
    }

    var vcChildren = (ev.vector_clock && Object.keys(ev.vector_clock).length)
      ? [el("div", { class: "trace-detail-kv", text: formatVectorClock(ev.vector_clock) })]
      : [el("div", { class: "trace-detail-kv trace-detail-value", text: "(not recorded)" })];
    container.appendChild(collapsible("Vector clock", vcChildren));
    return container;
  }

  // groupMembersTable renders one row per folded member event,
  // paginated GROUP_MEMBERS_PAGE at a time inside a vertically-
  // scrollable wrapper. A "Load next 100 (X remaining)" / "Show all"
  // footer matches the event log's pattern so the affordance is
  // familiar. Failing rows tint via the .fail class.
  var GROUP_MEMBERS_PAGE = 100;
  function groupMembersTable(seqList, events) {
    var bySeq = {};
    for (var i = 0; i < events.length; i++) {
      var ev = events[i];
      if (ev) bySeq[ev.seq || 0] = ev;
    }
    var host = el("div");
    var wrap = el("div", { class: "dd-group-members-wrap" });
    var tbl = el("table", { class: "dd-group-members" });
    var thead = el("thead", null, [el("tr", null, [
      el("th", { text: "seq" }),
      el("th", { text: "type" }),
      el("th", { text: "summary" }),
    ])]);
    var tbody = el("tbody");
    tbl.appendChild(thead);
    tbl.appendChild(tbody);
    wrap.appendChild(tbl);
    host.appendChild(wrap);

    var rendered = 0;
    function renderRow(s) {
      var ev2 = bySeq[s];
      if (!ev2) {
        tbody.appendChild(el("tr", null, [
          el("td", { text: String(s) }),
          el("td", { text: "(not in trace)", "colspan": "2" }),
        ]));
        return;
      }
      var f2 = ev2.fields || {};
      var rowCls = (f2.success === "false" || f2.error ||
                    parseInt(f2.status_code || "0", 10) >= 400) ? "fail" : "";
      tbody.appendChild(el("tr", { class: rowCls }, [
        el("td", { text: String(s) }),
        el("td", { class: "mono", text: ev2.type || "" }),
        el("td", { class: "mono", text: eventHeadline(ev2, { full: true }) || "" }),
      ]));
    }

    function appendBatch(n) {
      var stop = Math.min(rendered + n, seqList.length);
      for (var j = rendered; j < stop; j++) renderRow(seqList[j]);
      rendered = stop;
    }

    var footer = null;
    function refreshFooter() {
      if (footer) { footer.parentNode.removeChild(footer); footer = null; }
      if (rendered >= seqList.length) return;
      var remaining = seqList.length - rendered;
      var next = Math.min(GROUP_MEMBERS_PAGE, remaining);
      var loadBtn = el("button", {
        class: "trace-log-loadmore", type: "button",
        text: "Load next " + next + " (" + remaining + " remaining)",
        onclick: function () {
          appendBatch(GROUP_MEMBERS_PAGE);
          refreshFooter();
        },
      });
      var kids = [loadBtn];
      if (remaining > GROUP_MEMBERS_PAGE) {
        var allBtn = el("button", {
          class: "trace-log-loadmore secondary", type: "button",
          text: "Show all " + seqList.length,
          onclick: function () {
            appendBatch(remaining);
            refreshFooter();
          },
        });
        kids.push(allBtn);
      }
      footer = el("div", { class: "trace-log-loadmore-row" }, kids);
      host.appendChild(footer);
    }

    appendBatch(GROUP_MEMBERS_PAGE);
    refreshFooter();
    return host;
  }

  function detailRow(label, value, emphasis) {
    var v = el("div", { class: "trace-detail-value" + (emphasis ? " emphasis" : ""),
      text: String(value) });
    return el("div", { class: "trace-detail-row" }, [
      el("div", { class: "trace-detail-label", text: label }),
      v,
    ]);
  }

  // detailExpansion tracks which collapsibles in the trace-detail
  // panel the user opened. v0.12.9 restores the open state when
  // pinSelection rebuilds the panel for a different event — without
  // this, every new pin reset the panel to its compact default and
  // the user had to re-expand "All fields" / "Vector clock" each
  // time. Keyed by section title so the persistence survives
  // re-renders and even cross-test drill-down opens.
  var detailExpansion = {};
  function collapsible(title, children) {
    var det = document.createElement("details");
    det.className = "trace-detail-collapsible";
    if (detailExpansion[title]) det.open = true;
    det.addEventListener("toggle", function () {
      detailExpansion[title] = det.open;
    });
    var sum = document.createElement("summary");
    sum.textContent = title;
    det.appendChild(sum);
    for (var i = 0; i < children.length; i++) det.appendChild(children[i]);
    return det;
  }

  // eventHeadline renders a single-line description that answers the
  // question "what happened here?" — the main column of the event log
  // table and the headline of the detail drawer. The goal is
  // specificity: "connect → /var/run/redis.sock" beats "connect"
  // because the reader doesn't have to expand the row to see *what*
  // was connected to.
  //
  // opts.full = true skips the per-field truncation. Use it where the
  // headline lives in a wrap-friendly container (detail panel summary,
  // grouped-detail summary row) so long queries / errors are visible
  // in full. Table rows and tooltips keep the truncated form.
  function eventHeadline(ev, opts) {
    var full = !!(opts && opts.full);
    var clip = function (s, n) { return full ? String(s || "") : truncate(s, n); };
    var t = ev.type || "";
    var f = ev.fields || {};

    // Protocol-level (proxy) events — pick the most informative
    // subject per protocol. The Go proxy layer emits type="proxy" with
    // fields that identify the payload (query / command / topic / etc.),
    // so we reconstruct a human-readable one-liner here.
    if (t === "proxy") {
      var proto = f.protocol || "";
      if (proto === "http" || proto === "http2") {
        var http = (f.method || "HTTP") + " " + (f.path || "");
        if (f.status) http += " → " + f.status;
        return http.trim();
      }
      if (proto === "postgres" || proto === "mysql" || proto === "clickhouse" ||
          proto === "cassandra") {
        var q = clip(f.query || "query", 80);
        if (f.rows) q += " · " + f.rows + "r";
        if (f.error) q += " · " + f.error;
        return q;
      }
      if (proto === "redis" || proto === "memcached") {
        var r = (f.command || "") + (f.key ? " " + f.key : "");
        if (f.result) r += " (" + f.result + ")";
        if (f.error) r += " · " + f.error;
        return r.trim();
      }
      if (proto === "kafka" || proto === "nats" || proto === "amqp") {
        var m = (f.api || "msg") + (f.topic ? " " + f.topic
          : f.subject ? " " + f.subject
          : f.exchange ? " " + f.exchange : "");
        if (f.partition) m += " p" + f.partition;
        if (f.error) m += " · " + f.error;
        return m.trim();
      }
      if (proto === "grpc") {
        var g = f.method || "grpc";
        if (f.code) g += " → " + f.code;
        return g;
      }
      if (proto === "mongodb") {
        var md = (f.command || "") + (f.collection ? " " + f.collection : "");
        return md.trim();
      }
      // Unknown protocol — fall back to a compact key=value dump.
      return compactFields(f);
    }

    if (t === "syscall") {
      var head = f.syscall || "syscall";
      // "connect → /tmp/redis.sock" is far more actionable than
      // "connect → allow". Same for openat + path.
      if (f.path) head += " → " + f.path;
      else if (f.decision) head += " → " + f.decision;
      if (f.path && f.decision && f.decision !== "allow")
        head += " (" + f.decision + ")";
      return head;
    }

    if (t === "step_send" || t === "step_recv") {
      // v0.12.9: the runtime's `summary` field starts with a bare
      // arrow (→ / ←) which several customers found ambiguous.
      // Pair the arrow with an explicit "call" / "reply" word so
      // the direction is unambiguous on a first read. The arrow
      // still scans faster once learned.
      var label = t === "step_send" ? "→ call" : "← reply";
      // v0.12.10: ★ prefix marks spec-anchored steps (calls the user
      // wrote directly in the test body). Helps the eye skim past
      // monitor / proxy noise to land on familiar landmarks.
      var star = f.spec ? "★ " : "";
      if (f.summary) {
        return star + label + " · " + stripArrowFromSummary(f.summary);
      }
      var parts = [star + label, "·", (f.target || "?") + (f.method ? "." + f.method : "")];
      var preview = f.sql || f.query || f.path || f.command || f.topic || f.body;
      if (preview) parts.push(clip(preview, 80));
      if (t === "step_recv" && f.status_code) parts.push("[" + f.status_code + "]");
      else if (t === "step_recv" && f.status) parts.push("[" + f.status + "]");
      if (t === "step_recv" && f.error) parts.push("ERR: " + clip(f.error, 60));
      return parts.join(" ");
    }

    if (t === "fault_applied" || t === "fault_removed") {
      var pre = t === "fault_applied" ? "+" : "−";
      var body = (f.syscall || "?") + " " + (f.action || "");
      if (f.errno) body += " " + f.errno;
      if (f.label) body += " [" + f.label + "]";
      return pre + " " + body;
    }

    if (t === "proxy_fault_applied" || t === "proxy_fault_removed") {
      var pPre = t === "proxy_fault_applied" ? "+" : "−";
      var pBody = "proxy fault";
      if (f.assumption) pBody += " [" + f.assumption + "]";
      var protoBits = [];
      if (f.protocol) protoBits.push(f.protocol);
      if (f.interface) protoBits.push(f.interface);
      if (protoBits.length) pBody += " · " + protoBits.join("/");
      return pPre + " " + pBody;
    }

    if (t === "violation") return f.reason || "violation";
    if (t.indexOf("service_") === 0) return t.replace("service_", "");
    return compactFields(f) || null;
  }

  function truncate(s, n) {
    s = String(s || "");
    return s.length <= n ? s : s.slice(0, n - 1) + "…";
  }

  function compactFields(f) {
    if (!f) return "";
    var parts = [];
    for (var k in f) parts.push(k + "=" + f[k]);
    return parts.join(" ");
  }

  // EVENT_LOG_PAGE_SIZE caps how many event-log rows materialise into
  // the DOM at first paint (RFC-031 Phase 2). Real bundles can carry
  // tens of thousands of syscall events; rendering them all up-front
  // cost first-paint seconds and made the modal lag on scroll. We
  // append a "Load more" footer that adds the next page on demand.
  // 200 was chosen empirically — fits comfortably above the fold of
  // the drill-down dialog and keeps DOM under 2k nodes for the
  // common case where the user never scrolls past the visible window.
  var EVENT_LOG_PAGE_SIZE = 200;

  // buildEventLog returns a collapsible <details> containing the
  // event log table with type-filter chips. Clicking a row invokes
  // onSelect — the same flow as clicking a marker. Rows render in
  // pages of EVENT_LOG_PAGE_SIZE — see comment above.
  function buildEventLog(events, order, onSelect, traceFilter, exportRefresh) {
    var typesPresent = {};
    var servicesPresent = {};
    for (var i = 0; i < events.length; i++) {
      var t = events[i].type || "other";
      typesPresent[t] = true;
      // Service axis matches lane routing (laneFor) so the user
      // filtering by "truck-api" finds step events to truck-api,
      // not just truck-api's own lifecycle.
      var s = laneFor(events[i]);
      servicesPresent[s] = true;
    }
    var knownTypes = ["syscall", "fault_applied", "fault_removed",
      "service_started", "service_ready", "service_stopped",
      "step_send", "step_recv", "violation"];
    var typeOpts = [];
    for (var j = 0; j < knownTypes.length; j++) {
      if (typesPresent[knownTypes[j]]) typeOpts.push(knownTypes[j]);
    }
    for (var kt in typesPresent) {
      if (knownTypes.indexOf(kt) < 0 && kt !== "other") typeOpts.push(kt);
    }
    var serviceOpts = Object.keys(servicesPresent).sort();

    var wrap = document.createElement("details");
    wrap.className = "trace-log";
    // Default-open so the forensic view is discoverable; users who
    // prefer a compact modal can collapse it with one click.
    wrap.open = true;
    var sum = document.createElement("summary");
    // The Phase 3 downsampler attaches events_total / events_dropped
    // on the test object so the user can see at a glance how much was
    // shed and re-run with --full-events if they need every event.
    var headerLabel = "Event log — " + events.length + " entries";
    if (typeof onSelect === "function" && events._sourceTest) {
      var t = events._sourceTest;
      if (t && t.events_total && t.events_total > events.length) {
        headerLabel += " (downsampled from " + t.events_total +
          ", " + t.events_dropped + " dropped — re-run report with --full-events for all)";
      }
    }
    sum.innerHTML = "<span>" + headerLabel + "</span>"
      + "<span>click to toggle</span>";
    wrap.appendChild(sum);

    var body = el("div", { class: "trace-log-body" });

    // v0.12.6: two-axis filter (Service + Type). Each axis has its
    // own chip toolbar. Both default empty (= "all"). Clicking a
    // chip toggles its active state; the row predicate ANDs across
    // axes. Service / Type cells in the table are clickable — click
    // → set that axis to that single value. The active-chips bar
    // shows what's selected with X buttons to remove.
    var activeServices = {};
    var activeTypes = {};

    var filterBar = el("div", { class: "trace-log-filterbar" });
    var serviceFilters = el("div", { class: "trace-log-filters", role: "toolbar", "aria-label": "Filter by service" });
    serviceFilters.appendChild(el("span", { class: "trace-log-filter-label", text: "Service" }));
    // v0.12.8: Type axis becomes click-to-add. The toolbar stays
    // empty until the user clicks a Type cell in the table; chips
    // appear with an inline X to remove. Cuts the at-a-glance UI
    // weight when the user only cares about service filtering.
    var typeFilters = el("div", { class: "trace-log-filters trace-log-filters-active", role: "toolbar", "aria-label": "Active type filters" });
    var typeFiltersLabel = el("span", { class: "trace-log-filter-label", text: "Type" });
    typeFilters.appendChild(typeFiltersLabel);
    var typeFiltersHint = el("span", { class: "trace-log-filter-hint", text: "click a type cell below to filter" });
    typeFilters.appendChild(typeFiltersHint);

    var serviceChips = {};
    for (var sIdx = 0; sIdx < serviceOpts.length; sIdx++) {
      (function (svc) {
        var chip = el("button", {
          class: "trace-log-chip", type: "button",
          "data-filter": svc, text: svc,
          onclick: function () { toggleService(svc); },
        });
        serviceChips[svc] = chip;
        serviceFilters.appendChild(chip);
      })(serviceOpts[sIdx]);
    }
    // typeChips holds DOM nodes for *active* type filters only —
    // entries are added on click-to-filter, removed on X.
    var typeChips = {};
    function addTypeChip(type) {
      if (typeChips[type]) return;
      var chip = el("span", { class: "trace-log-chip active removable", "data-filter": type });
      chip.appendChild(document.createTextNode(type));
      var x = el("button", {
        class: "trace-log-chip-x", type: "button",
        "aria-label": "Remove filter",
        text: "×",
        onclick: function (e) {
          e.stopPropagation();
          delete activeTypes[type];
          chip.parentNode.removeChild(chip);
          delete typeChips[type];
          updateTypeHint();
          applyFilters();
        },
      });
      chip.appendChild(x);
      typeChips[type] = chip;
      typeFilters.appendChild(chip);
      updateTypeHint();
    }
    function updateTypeHint() {
      typeFiltersHint.style.display = Object.keys(typeChips).length ? "none" : "";
    }
    filterBar.appendChild(serviceFilters);
    filterBar.appendChild(typeFilters);
    body.appendChild(filterBar);

    var scroll = el("div", { class: "trace-log-scroll" });
    var table = el("table", { class: "trace-log-table" });
    var thead = el("thead", null, [el("tr", null, [
      el("th", { class: "caret-col", "aria-label": "expand" }),
      el("th", { class: "type", text: "type" }),
      el("th", { class: "service", text: "service" }),
      el("th", { text: "summary" }),
    ])]);
    var tbody = el("tbody");

    // v0.12.7: filter applies to the *full* event set, not the
    // currently-rendered slice. The previous model loaded the first
    // 200 then hid non-matching rows — meaning a filter for a
    // service whose events sat past row 200 returned no visible
    // rows. Now we maintain a `filteredEvents` view; the table
    // pages from it, and toggling filters resets the page.
    var filteredEvents = events.slice();
    var renderedCount = 0;
    var caption;
    function appendBatch(n) {
      var stop = Math.min(renderedCount + n, filteredEvents.length);
      for (var r = renderedCount; r < stop; r++) {
        var pair = logRow(filteredEvents[r], onSelect, wrap);
        tbody.appendChild(pair[0]);
        tbody.appendChild(pair[1]);
      }
      renderedCount = stop;
    }
    appendBatch(EVENT_LOG_PAGE_SIZE);

    table.appendChild(thead);
    table.appendChild(tbody);
    scroll.appendChild(table);
    body.appendChild(scroll);

    var loadMoreWrap = null;
    function refreshLoadMore() {
      if (loadMoreWrap) loadMoreWrap.parentNode.removeChild(loadMoreWrap);
      loadMoreWrap = null;
      if (renderedCount >= filteredEvents.length) return;
      var remaining = filteredEvents.length - renderedCount;
      var btn = el("button", {
        class: "trace-log-loadmore", type: "button",
        text: "Load next " + Math.min(EVENT_LOG_PAGE_SIZE, remaining) +
              " (" + remaining + " remaining)",
        onclick: function () {
          appendBatch(EVENT_LOG_PAGE_SIZE);
          refreshLoadMore();
          updateCaption();
        },
      });
      var allBtn = null;
      if (remaining > EVENT_LOG_PAGE_SIZE) {
        allBtn = el("button", {
          class: "trace-log-loadmore secondary", type: "button",
          text: "Show all " + filteredEvents.length,
          onclick: function () {
            appendBatch(remaining);
            refreshLoadMore();
            updateCaption();
          },
        });
      }
      var kids = [btn];
      if (allBtn) kids.push(allBtn);
      loadMoreWrap = el("div", { class: "trace-log-loadmore-row" }, kids);
      body.appendChild(loadMoreWrap);
    }
    refreshLoadMore();

    caption = el("div", { class: "stat-caption", text: "" });
    body.appendChild(caption);
    updateCaption();

    wrap.appendChild(body);

    function updateCaption() {
      var hasFilter = activeKeys(activeServices).length || activeKeys(activeTypes).length;
      var lead = "Showing first " + renderedCount + " of " + filteredEvents.length;
      if (hasFilter) {
        lead += " matching events (out of " + events.length + " total).";
      } else {
        lead += " events.";
      }
      caption.textContent = lead;
    }

    function toggleService(svc) {
      if (activeServices[svc]) delete activeServices[svc];
      else activeServices[svc] = true;
      applyFilters();
    }
    function toggleType(type) {
      if (activeTypes[type]) {
        delete activeTypes[type];
        if (typeChips[type] && typeChips[type].parentNode) {
          typeChips[type].parentNode.removeChild(typeChips[type]);
        }
        delete typeChips[type];
        updateTypeHint();
      } else {
        activeTypes[type] = true;
        addTypeChip(type);
      }
      applyFilters();
    }

    function activeKeys(set) {
      var out = [];
      for (var k in set) out.push(k);
      return out;
    }

    function matchesFilters(ev) {
      // Trace-level filter (preset/search) is applied first so the
      // event log mirrors what the lanes are showing. Per-axis chip
      // filters layer on top.
      if (traceFilter && !passesTraceFilter(ev, traceFilter)) return false;
      var svcKeys = activeKeys(activeServices);
      var typeKeys = activeKeys(activeTypes);
      if (svcKeys.length && !activeServices[laneFor(ev)]) return false;
      if (typeKeys.length && !activeTypes[ev.type || ""]) return false;
      return true;
    }

    function applyFilters() {
      // Reflect chip active state.
      for (var k in serviceChips) {
        serviceChips[k].classList.toggle("active", !!activeServices[k]);
      }
      // typeChips are dynamic — they only exist while active, so no
      // per-key toggle pass needed.
      // Rebuild the filtered list from scratch and reset the page.
      filteredEvents = events.filter(matchesFilters);
      tbody.innerHTML = "";
      renderedCount = 0;
      appendBatch(EVENT_LOG_PAGE_SIZE);
      refreshLoadMore();
      updateCaption();
    }

    // Click-to-filter on table cells. Service: replace selection.
    // Type: ADD to selection so click-multiple gradually accumulates
    // a multi-type filter (X to remove individual chips).
    wrap._addServiceFilter = function (svc) {
      activeServices = {};
      activeServices[svc] = true;
      applyFilters();
    };
    wrap._addTypeFilter = function (type) {
      if (activeTypes[type]) return; // already active
      activeTypes[type] = true;
      addTypeChip(type);
      applyFilters();
    };

    // Trace viewer calls this when its filter bar changes so the
    // event log re-derives `filteredEvents` against the latest
    // preset/search.
    if (typeof exportRefresh === "function") exportRefresh(applyFilters);

    return wrap;
  }

  // logRow builds one <tr> + a sibling expansion <tr> that toggles on
  // click. The expansion carries the grouped detail view (Request /
  // Response / Fault / System / Other) so users can inspect rich
  // per-event fields right where they clicked, without scrolling back
  // to the trace-viewer detail panel. Clicking the row also drives
  // onSelect — the standard "pin this event on the lanes" behaviour.
  function logRow(ev, onSelect, logWrap) {
    var caret = el("span", { class: "trace-log-caret", text: "▸" });
    // v0.12.7: service display + filter axis follow lane routing —
    // step_send/step_recv land on their target service so filtering
    // by "truck-api" matches the call to it, not just its lifecycle.
    var rowService = laneFor(ev);
    var tr = el("tr", {
      "data-seq": String(ev.seq || 0),
      "data-type": ev.type || "",
      "data-service": rowService,
    });
    tr.appendChild(el("td", { class: "caret-col" }, [caret]));
    // v0.12.6: type + service cells are clickable. Click → set the
    // filter for that axis to *only this value*. Chip toolbar above
    // updates to reflect the selection. Lets a user say "show only
    // step_recv events on truck-api" by clicking two cells, no
    // chip-hunting required.
    var typeCell = el("td", { class: "type" }, [
      el("span", {
        class: "trace-log-type type-" + markerKind(ev) + " clickable",
        text: ev.type || "",
        title: "Click to filter by type",
        onclick: function (e) {
          e.stopPropagation();
          if (logWrap && logWrap._addTypeFilter) logWrap._addTypeFilter(ev.type || "");
        },
      }),
    ]);
    tr.appendChild(typeCell);
    var svcCell = el("td", {
      class: "service clickable",
      text: rowService,
      title: "Click to filter by service",
      onclick: function (e) {
        e.stopPropagation();
        if (logWrap && logWrap._addServiceFilter) logWrap._addServiceFilter(rowService);
      },
    });
    tr.appendChild(svcCell);
    var headlineText = eventHeadline(ev) || "";
    tr.appendChild(el("td", { text: headlineText, title: headlineText }));

    // Expansion row sits directly beneath, same 4-column grid, spans
    // all columns with a single cell. Hidden until the user clicks.
    var expand = el("tr", { class: "trace-log-expand", "data-seq-expand": String(ev.seq || 0) });
    expand.hidden = true;
    var cell = el("td", { colspan: "4" });
    expand.appendChild(cell);

    tr.addEventListener("click", function () {
      onSelect(ev);
      var opening = expand.hidden;
      expand.hidden = !opening;
      tr.classList.toggle("open", opening);
      caret.textContent = opening ? "▾" : "▸";
      if (opening && !cell.firstChild) {
        cell.appendChild(buildGroupedDetail(ev));
      }
    });

    // Return both rows so the caller can append them adjacent.
    return [tr, expand];
  }

  // Known field keys grouped by semantic role. "Request" is what a
  // service tried to do; "Response" is what came back; "Fault" is the
  // rule that bent reality; "System" is low-level attribution.
  // Everything else spills into "Other" so nothing is lost.
  var FIELD_GROUPS = {
    Request:  ["method", "path", "target", "interface", "protocol",
               "query", "topic", "partition", "api", "command", "key",
               "subject", "exchange", "routing_key", "collection", "body",
               "direction"],
    Response: ["status", "rows", "result", "code", "error", "size"],
    Fault:    ["action", "errno", "delay_ms", "label"],
    System:   ["syscall", "pid", "decision", "op", "latency_ms", "ttl"],
  };

  // buildGroupedDetail renders one event's full metadata in sectioned
  // key-value groups. Unlike the pinned-selection panel above the log,
  // this view emphasises readability — it's meant to be skimmed while
  // the surrounding event log rows stay in view.
  function buildGroupedDetail(ev) {
    var f = ev.fields || {};
    var known = {};
    var out = el("div", { class: "log-expand-body" });

    var groups = ["Request", "Response", "Fault", "System"];
    for (var gi = 0; gi < groups.length; gi++) {
      var name = groups[gi];
      var keys = FIELD_GROUPS[name];
      var rows = [];
      for (var i = 0; i < keys.length; i++) {
        var k = keys[i];
        if (f[k] != null && f[k] !== "") {
          rows.push(kvRow(k, f[k]));
          known[k] = true;
        }
      }
      if (rows.length) {
        out.appendChild(groupSection(name, rows));
      }
    }

    // "Other" catches any field not in a known group so no data is
    // hidden (forward-compat for new event kinds).
    var other = [];
    for (var k2 in f) {
      if (!known[k2]) other.push(kvRow(k2, f[k2]));
    }
    if (other.length) out.appendChild(groupSection("Other", other));

    // Meta row: seq, vector clock, event_type, partition_key — always
    // shown at the bottom, dim styling, so every row looks consistent.
    var meta = [kvRow("seq", String(ev.seq || 0))];
    if (ev.event_type && ev.event_type !== ev.type)
      meta.push(kvRow("event_type", ev.event_type));
    if (ev.partition_key && ev.partition_key !== ev.service)
      meta.push(kvRow("partition_key", ev.partition_key));
    if (ev.vector_clock && Object.keys(ev.vector_clock).length)
      meta.push(kvRow("vector_clock", formatVectorClock(ev.vector_clock)));
    out.appendChild(groupSection("Meta", meta));

    if (!out.firstChild) {
      out.appendChild(el("div", { class: "log-expand-empty",
        text: "No additional fields recorded for this event." }));
    }
    return out;
  }

  function groupSection(title, rows) {
    var sec = el("div", { class: "log-expand-group" });
    sec.appendChild(el("div", { class: "log-expand-group-title", text: title }));
    var grid = el("div", { class: "log-expand-kv" });
    for (var i = 0; i < rows.length; i++) grid.appendChild(rows[i]);
    sec.appendChild(grid);
    return sec;
  }

  function kvRow(k, v) {
    var valCell = el("div", { class: "log-expand-kv-val" });
    var tree = tryRenderJsonTree(v);
    if (tree) {
      valCell.appendChild(tree);
    } else {
      valCell.textContent = String(v);
    }
    return el("div", { class: "log-expand-kv-row" }, [
      el("div", { class: "log-expand-kv-key", text: k }),
      valCell,
    ]);
  }

  // tryRenderJsonTree returns a 2-column key/value <table> for v iff
  // it parses as a JSON object or array. Nested objects / arrays are
  // flattened with dot-path keys (req.method, items[0].id) so every
  // leaf has its own row. Used by kvRow so stdout events (whose
  // `data` field is a JSON-encoded log line) render as a tidy table
  // instead of a wall of escaped text. Strings, numbers, and
  // ill-formed JSON return null — caller falls back to plain text.
  function tryRenderJsonTree(v) {
    if (typeof v !== "string") return null;
    var s = v.trim();
    if (!s || (s.charAt(0) !== "{" && s.charAt(0) !== "[")) return null;
    var parsed;
    try { parsed = JSON.parse(s); } catch (e) { return null; }
    if (parsed === null || typeof parsed !== "object") return null;
    var rows = [];
    flattenJson("", parsed, rows);
    if (!rows.length) return null;
    var tbl = el("table", { class: "json-kv-table" });
    var tbody = el("tbody");
    for (var i = 0; i < rows.length; i++) {
      tbody.appendChild(el("tr", null, [
        el("td", { class: "json-kv-key", text: rows[i][0] }),
        el("td", { class: "json-kv-val " + jsonValClass(rows[i][1]),
          text: jsonScalarText(rows[i][1]) }),
      ]));
    }
    tbl.appendChild(tbody);
    return tbl;
  }

  function flattenJson(prefix, node, out) {
    if (node === null || typeof node !== "object") {
      out.push([prefix || "(value)", node]);
      return;
    }
    if (Array.isArray(node)) {
      if (!node.length) {
        out.push([prefix || "(value)", "[]"]);
        return;
      }
      for (var i = 0; i < node.length; i++) {
        flattenJson(prefix + "[" + i + "]", node[i], out);
      }
      return;
    }
    var keys = Object.keys(node);
    if (!keys.length) {
      out.push([prefix || "(value)", "{}"]);
      return;
    }
    for (var k = 0; k < keys.length; k++) {
      var nextKey = prefix ? prefix + "." + keys[k] : keys[k];
      flattenJson(nextKey, node[keys[k]], out);
    }
  }

  function jsonScalarText(v) {
    if (v === null) return "null";
    if (typeof v === "string") return v;
    return String(v);
  }

  function jsonValClass(v) {
    if (v === null) return "json-null";
    if (typeof v === "string") return "json-str";
    if (typeof v === "number") return "json-num";
    if (typeof v === "boolean") return "json-bool";
    return "";
  }

  function formatVectorClock(vc) {
    var parts = [];
    var keys = Object.keys(vc).sort();
    for (var i = 0; i < keys.length; i++) parts.push(keys[i] + ":" + vc[keys[i]]);
    return "{" + parts.join(", ") + "}";
  }

  // happensBefore implements the vector-clock partial order used to
  // discover causal ancestors. If either event lacks a vector clock,
  // it falls back to a strict seq ordering — good enough for the
  // "nearest prior event per other lane" fallback, and on real bundles
  // vector clocks are always present anyway.
  function happensBefore(a, b) {
    if (!a.vector_clock || !b.vector_clock) {
      return (a.seq || 0) < (b.seq || 0);
    }
    var hosts = {};
    var k;
    for (k in a.vector_clock) hosts[k] = true;
    for (k in b.vector_clock) hosts[k] = true;
    var strict = false;
    for (k in hosts) {
      var va = a.vector_clock[k] || 0;
      var vb = b.vector_clock[k] || 0;
      if (va > vb) return false;
      if (va < vb) strict = true;
    }
    return strict;
  }

  // isCauseCandidate is the per-event predicate for "could this be
  // the upstream cause of a downstream failure?". Faults and
  // violations are the obvious yes; errored steps (success=false /
  // error field set) capture protocol-level failures the runtime
  // didn't directly inject (e.g. a 5xx from the SUT). Lifecycle
  // events are explicitly excluded — `service_ready` happening
  // before an error doesn't mean it caused the error.
  function isCauseCandidate(e) {
    var t = e.type || "";
    if (t === "fault_applied" || t === "fault_removed" ||
        t === "proxy_fault_applied" || t === "proxy_fault_removed" ||
        t === "fault_skipped_no_seccomp" || t === "fault_zero_traffic" ||
        t === "violation") return true;
    if ((t === "step_send" || t === "step_recv") && e.fields) {
      if (e.fields.success === "false" || e.fields.error) return true;
    }
    return false;
  }

  // findCausalAncestors returns, for each other lane, the cause
  // candidate (fault / violation / errored step) closest in seq to
  // the target. v0.12.16 redesign:
  //
  //   - Use seq-based strict precedence, not vector clocks. Real
  //     bundles routinely emit faults with a partial vector clock
  //     (only the emitting service's tick), so happensBefore returns
  //     false for fault → cross-service-error edges that *clearly*
  //     ordered in seq. Falling back to whatever lifecycle event had
  //     a complete clock produced the "service_ready → error" links
  //     Boris flagged: technically valid, semantically useless.
  //   - Restrict the candidate set to causes (isCauseCandidate). A
  //     `service_ready` is a precondition, not a cause; rendering it
  //     as one buries the actual fault under chrome.
  //   - Target must itself be a cause candidate: ordinary success
  //     steps and syscalls have no meaningful causal upstream.
  //
  // Result: hover the truck-api 500 → arrow points at the
  // proxy_fault_applied(mysql_slow) on db, period.
  //
  // v0.12.8 keys on laneFor() not raw service so step events folded
  // onto the target service's lane (rather than the test driver
  // lane) draw lines back to other services correctly.
  function findCausalAncestors(target, allEvents) {
    if (!isCauseCandidate(target)) return [];
    var targetSeq = target.seq || 0;
    var targetLane = laneFor(target);
    var perLane = {};
    for (var i = 0; i < allEvents.length; i++) {
      var e = allEvents[i];
      if (e === target) continue;
      if (!isCauseCandidate(e)) continue;
      if ((e.seq || 0) >= targetSeq) continue;
      var lane = laneFor(e);
      if (lane === targetLane) continue;
      var existing = perLane[lane];
      if (!existing || (e.seq || 0) > (existing.seq || 0)) perLane[lane] = e;
    }
    var out = [];
    for (var s in perLane) out.push(perLane[s]);
    return out;
  }

  // resolveMarkerForSeq finds the visible marker for a given seq.
  // After v0.12.8's fold-by-key + slot bucketing, an ancestor seq
  // may be hidden inside a chip's `_runMembers` list rather than
  // mapped 1:1 in markerNodes. Walk the chips' member lists once
  // (cached per call site) so causal lines still draw to the
  // representative marker. Without this, every folded ancestor
  // produced a no-op `if (!ancNode) continue;` and the user saw
  // no lines.
  function resolveMarkerForSeq(seq, markerNodes, seqToMarker) {
    var direct = markerNodes[seq];
    if (direct) return direct;
    return seqToMarker[seq] || null;
  }

  function buildSeqToMarkerIndex(markerNodes) {
    var idx = {};
    for (var s in markerNodes) {
      var node = markerNodes[s];
      idx[s] = node;
      // Walk _runMembers stored on the marker via dataset.
      var members = node.dataset && node.dataset.members;
      if (members) {
        var parts = members.split(",");
        for (var p = 0; p < parts.length; p++) {
          if (!idx[parts[p]]) idx[parts[p]] = node;
        }
      }
    }
    return idx;
  }

  function drawCausalLines(svg, rootNode, ancestors, host, markerNodes) {
    svg.innerHTML = "";
    if (!ancestors.length) return;
    var hostRect = host.getBoundingClientRect();
    var rootRect = rootNode.getBoundingClientRect();
    var rx = rootRect.left - hostRect.left + rootRect.width / 2;
    var ry = rootRect.top - hostRect.top + rootRect.height / 2;

    var seqToMarker = buildSeqToMarkerIndex(markerNodes);

    for (var i = 0; i < ancestors.length; i++) {
      var a = ancestors[i];
      var ancNode = resolveMarkerForSeq(a.seq, markerNodes, seqToMarker);
      if (!ancNode) continue;
      ancNode.classList.add("highlight-ancestor");
      var aRect = ancNode.getBoundingClientRect();
      var ax = aRect.left - hostRect.left + aRect.width / 2;
      var ay = aRect.top - hostRect.top + aRect.height / 2;
      // Gentle curve via quadratic Bézier — goes through the mid-x
      // at the mid-y of the two endpoints so lines don't overlap
      // when stacked vertically.
      var mx = (ax + rx) / 2;
      var my = (ay + ry) / 2;
      var path = document.createElementNS("http://www.w3.org/2000/svg", "path");
      path.setAttribute("d", "M " + ax + " " + ay + " Q " + mx + " " + my + " " + rx + " " + ry);
      path.setAttribute("class", "trace-causal-line");
      svg.appendChild(path);
    }
  }

  function traceLegend() {
    return el("div", { class: "trace-legend" }, [
      legendDot("syscall", "syscall"),
      legendDot("fault", "fault"),
      legendDot("step", "step"),
      legendDot("lifecycle", "lifecycle"),
      legendDot("violation", "violation"),
    ]);
  }

  function legendDot(cls, label) {
    return el("span", { class: "trace-legend-item" }, [
      el("span", { class: "trace-legend-dot " + cls }),
      document.createTextNode(label),
    ]);
  }

  // ── Reproducibility panel ───────────────────────────────────────
  function renderRepro(data) {
    var grid = document.getElementById("repro-grid");
    var replay = document.getElementById("repro-replay");
    if (!grid || !replay) return;

    var m = data.manifest || {};
    var env = data.env || {};

    var items = [];
    items.push(reproItem("Faultbox",
      env.faultbox_version || m.faultbox_version || "—",
      env.faultbox_commit ? env.faultbox_commit.slice(0, 8) : null));
    if (env.go_toolchain) items.push(reproItem("Go toolchain", env.go_toolchain, null));
    if (env.host_os || env.host_arch) {
      items.push(reproItem("Host",
        (env.host_os || "?") + "/" + (env.host_arch || "?"),
        env.kernel ? "kernel " + env.kernel : null));
    }
    if (env.docker_version) items.push(reproItem("Docker", env.docker_version, null));
    if (env.runtime_hints && env.runtime_hints.length) {
      items.push(reproItem("Runtime hints", env.runtime_hints.join(", "), null));
    }
    items.push(reproItem("Seed", String(m.seed || 0), null));
    if (m.spec_root) items.push(reproItem("Spec root", m.spec_root, null));
    if (m.created_at) items.push(reproItem("Created", fmtTimestamp(m.created_at), null));

    grid.innerHTML = "";
    for (var i = 0; i < items.length; i++) grid.appendChild(items[i]);

    // Image digests: collapsed table. Scales to dozens of images
    // without taking vertical space, and lets users copy the full
    // digest even when we only show the short form inline.
    if (env.images && Object.keys(env.images).length) {
      grid.appendChild(renderDigestTable(env.images));
    }

    // Replay call-out.
    var cmd = "faultbox replay " + bundleBasename();
    replay.innerHTML = "";
    var head = el("div", { class: "repro-replay-head" }, [
      el("span", { class: "repro-replay-title", text: "Rerun this exact run" }),
    ]);
    var code = el("code", { class: "mono", text: cmd });
    var btn = el("button", {
      class: "copy-btn", type: "button", "aria-label": "Copy replay command",
      onclick: function () { copyToClipboard(cmd, btn); }, text: "Copy",
    });
    head.appendChild(btn);
    replay.appendChild(head);
    replay.appendChild(code);
  }

  function reproItem(label, value, caption) {
    var kids = [
      el("div", { class: "repro-label", text: label }),
      el("div", { class: "repro-value", text: value }),
    ];
    if (caption) kids.push(el("div", { class: "repro-label", text: caption }));
    return el("div", { class: "repro-item" }, kids);
  }

  // renderDigestTable produces a <details> element containing a
  // table of image → short digest → copyable full digest. Spans the
  // full width of the reproducibility grid via `grid-column: 1/-1`.
  function renderDigestTable(images) {
    var wrap = document.createElement("details");
    wrap.className = "digest-details";
    var keys = Object.keys(images).sort();

    var sum = document.createElement("summary");
    sum.innerHTML = "<span>Image digests \u2014 " + keys.length + " image"
      + (keys.length === 1 ? "" : "s") + " pinned</span>"
      + "<span>click to toggle</span>";
    wrap.appendChild(sum);

    var table = el("table", { class: "digest-table" });
    var thead = el("thead", null, [el("tr", null, [
      el("th", { text: "Image" }),
      el("th", { text: "Digest (short)" }),
      el("th", { text: "Digest (full)" }),
      el("th", null),
    ])]);
    table.appendChild(thead);
    var tbody = el("tbody");
    for (var i = 0; i < keys.length; i++) {
      var img = keys[i];
      var full = images[img];
      var shortPart = full.indexOf(":") === -1 ? full : full.split(":")[1].slice(0, 12);
      var copyBtn = el("button", {
        class: "copy-btn", type: "button", "aria-label": "Copy full digest",
        text: "Copy",
      });
      (function (cmd, btn) {
        btn.addEventListener("click", function () { copyToClipboard(cmd, btn); });
      })(full, copyBtn);
      tbody.appendChild(el("tr", null, [
        el("td", { text: img }),
        el("td", { class: "digest-short", text: shortPart }),
        el("td", { class: "digest-full", text: full }),
        el("td", null, [copyBtn]),
      ]));
    }
    table.appendChild(tbody);
    wrap.appendChild(table);
    return wrap;
  }

  // ── Spec section (top-level) ────────────────────────────────────
  //
  // Renders every spec/*.star file from the bundle as a collapsible
  // source viewer. The viewer has line numbers, stable anchors
  // (#spec-<path>-L<n>), and a focusSpecLine() helper used by the
  // coverage-table service links and the drill-down topology /
  // source cross-jumps.
  var specIndex = null; // {byPath: {path: {lines: [...]}}}

  function renderSpec(data) {
    var specs = data.specs || {};
    var paths = Object.keys(specs).sort();
    if (!paths.length) return;
    var section = document.getElementById("spec-section");
    var host = document.getElementById("spec-body");
    if (!section || !host) return;
    section.hidden = false;

    specIndex = { byPath: {} };
    host.innerHTML = "";

    for (var i = 0; i < paths.length; i++) {
      var p = paths[i];
      var body = specs[p] || "";
      var fileEl = buildSpecFile(p, body);
      host.appendChild(fileEl);
      specIndex.byPath[p] = { body: body, element: fileEl };
    }
  }

  // RFC-042 §8.10 — Plan tab. Reads data.plan (the plan.json the
  // bundle carries) and renders the test tree + coverage summary in a
  // compact, scannable form. Falls back to a "no plan data" note for
  // pre-RFC-042 bundles so older fixtures still render.
  function renderPlan(data) {
    var section = document.getElementById("plan-section");
    var host = document.getElementById("plan-body");
    if (!section || !host) return;
    var plan = data.plan;
    if (!plan || typeof plan !== "object") {
      section.hidden = true;
      return;
    }
    section.hidden = false;
    host.innerHTML = "";

    // Header line: spec / seed / determinism / totals.
    var head = el("div", { class: "plan-head" });
    if (plan.spec_path) head.appendChild(el("span", { class: "mono", text: plan.spec_path }));
    var det = plan.determinism || {};
    var detText = "Determinism " + (det.level || "?");
    if (det.strict) detText += " (strict)";
    head.appendChild(el("span", { class: "plan-pill", text: detText }));
    var totals = plan.totals || {};
    head.appendChild(el("span", {
      class: "plan-pill",
      text: (totals.instances || 0) + " instance" + ((totals.instances || 0) === 1 ? "" : "s"),
    }));
    host.appendChild(head);

    // Test list.
    var tests = plan.tests || [];
    if (!tests.length) {
      host.appendChild(el("p", { class: "plan-empty", text: "No tests discovered." }));
    } else {
      var list = el("ul", { class: "plan-tests" });
      for (var i = 0; i < tests.length; i++) {
        list.appendChild(renderPlanTest(tests[i]));
      }
      host.appendChild(list);
    }

    // Embedded coverage summary if the bundle carried one.
    var cov = plan.coverage;
    if (cov && cov.edges && cov.edges.length) {
      host.appendChild(renderPlanCoverage(cov));
    }
  }

  function renderPlanTest(t) {
    var item = el("li", { class: "plan-test plan-test-" + (t.kind || "unknown") });
    var head = el("div", { class: "plan-test-head" });
    head.appendChild(el("span", { class: "plan-test-name mono", text: t.name }));
    head.appendChild(el("span", { class: "plan-test-kind", text: "[" + (t.kind || "?") + "]" }));
    head.appendChild(el("span", { class: "plan-test-count", text: (t.instances || 1) + "x" }));
    item.appendChild(head);

    if (t.compositions) {
      for (var ci = 0; ci < t.compositions.length; ci++) {
        var comp = t.compositions[ci];
        var compEl = el("div", { class: "plan-comp" });
        compEl.appendChild(el("span", { class: "plan-comp-kind", text: comp.kind }));
        for (var ai = 0; ai < (comp.axes || []).length; ai++) {
          var ax = comp.axes[ai];
          var vals = (ax.values || []).join(", ");
          compEl.appendChild(el("span", { class: "plan-axis", text: ax.name + ": [" + vals + "]" }));
        }
        item.appendChild(compEl);
      }
    }
    if (t.faults && t.faults.length) {
      item.appendChild(el("div", { class: "plan-faults", text: "faults: [" + t.faults.join(", ") + "]" }));
    }
    if (t.matrix_cells && t.matrix_cells.length) {
      // Show the individual cell names for a fault_matrix entry. The
      // tree groups them under one PlanTest; the cell list lets users
      // see exactly which (scenario, fault) tuples ran.
      item.appendChild(el("div", {
        class: "plan-cells",
        text: "cells: " + t.matrix_cells.join(", "),
      }));
    }
    if (t.expect) {
      item.appendChild(el("div", { class: "plan-expect", text: "expect: " + t.expect }));
    }
    if (t.timeout) {
      item.appendChild(el("div", { class: "plan-timeout", text: "timeout: " + t.timeout }));
    }
    return item;
  }

  function renderPlanCoverage(cov) {
    var wrap = el("div", { class: "plan-coverage" });
    wrap.appendChild(el("h3", { text: "Coverage" }));
    wrap.appendChild(el("p", {
      class: "plan-coverage-summary",
      text: cov.uncovered_edges + " of " + (cov.edges || []).length + " edges without a fault test",
    }));
    var table = el("table", { class: "plan-coverage-table" });
    for (var i = 0; i < cov.edges.length; i++) {
      var e = cov.edges[i];
      var row = el("tr", { class: e.fault_tests && e.fault_tests.length ? "covered" : "uncovered" });
      row.appendChild(el("td", { text: e.from + " → " + e.to }));
      row.appendChild(el("td", { text: e.protocol || "" }));
      row.appendChild(el("td", {
        text: (e.fault_tests && e.fault_tests.length) ? e.fault_tests.join(", ") : "—",
      }));
      table.appendChild(row);
    }
    wrap.appendChild(table);
    return wrap;
  }

  function specAnchorId(path, line) {
    return "spec-" + path.replace(/[^a-zA-Z0-9_.-]/g, "_") + "-L" + line;
  }

  function buildSpecFile(path, body) {
    var file = document.createElement("details");
    file.className = "spec-file";
    file.id = "spec-" + path.replace(/[^a-zA-Z0-9_.-]/g, "_");

    var sum = document.createElement("summary");
    var displayPath = path.replace(/^spec\//, "");
    var bytes = body.length;
    sum.appendChild(el("span", { class: "spec-file-path", text: displayPath }));
    sum.appendChild(el("span", { class: "spec-file-size",
      text: bytes < 1024 ? bytes + " B" : (bytes / 1024).toFixed(1) + " KB" }));
    file.appendChild(sum);

    var bodyWrap = el("div", { class: "spec-file-body" });
    bodyWrap.appendChild(renderSourceCode(path, body));
    file.appendChild(bodyWrap);

    return file;
  }

  function renderSourceCode(path, body) {
    var pre = document.createElement("pre");
    pre.className = "source-code";
    var lines = body.split(/\r?\n/);
    for (var i = 0; i < lines.length; i++) {
      var lineNo = i + 1;
      var codeSpan = document.createElement("span");
      codeSpan.className = "source-line-code";
      codeSpan.innerHTML = highlightStarlark(lines[i]);
      var row = el("div", {
        class: "source-line",
        id: specAnchorId(path, lineNo),
        "data-line": String(lineNo),
      }, [
        el("span", { class: "source-line-no", text: String(lineNo) }),
      ]);
      row.appendChild(codeSpan);
      pre.appendChild(row);
    }
    return pre;
  }

  // highlightStarlark is a small hand-rolled tokenizer tuned to the
  // Starlark dialect Faultbox specs use. Emits the same HTML a full
  // syntax highlighter would, but in ~2 KB and without an external
  // dependency. Colours come from CSS variables (.tok-*), so the
  // scheme follows light/dark mode automatically.
  //
  // Tokens recognised:
  //   - line comments (#…)
  //   - single- and double-quoted strings (incl. escape skip)
  //   - decimal numbers
  //   - keywords (Python subset Starlark supports)
  //   - built-ins (Faultbox surface: service, fault, scenario, …)
  //   - def-name (the identifier immediately after `def`)
  //
  // Triple-quoted strings and line continuations are not supported;
  // they would bleed across line boundaries and are rare enough in
  // spec files that the loss is acceptable for v0.11.0.
  var STAR_KEYWORDS = {
    "def": 1, "lambda": 1, "return": 1, "if": 1, "elif": 1, "else": 1,
    "for": 1, "while": 1, "in": 1, "not": 1, "and": 1, "or": 1,
    "True": 1, "False": 1, "None": 1, "pass": 1, "break": 1, "continue": 1,
    "load": 1, "with": 1, "as": 1,
  };
  var STAR_BUILTINS = {
    "service": 1, "mock_service": 1, "domain": 1,
    "fault": 1, "fault_assumption": 1, "fault_scenario": 1, "fault_matrix": 1,
    "scenario": 1,
    "assert_eq": 1, "assert_true": 1, "assert_false": 1,
    "assert_never": 1, "assert_eventually": 1, "assert_error": 1,
    "expect_success": 1, "expect_error_within": 1, "expect_hang": 1,
    "delay": 1, "deny": 1, "hold": 1, "allow": 1,
    "load_file": 1, "load_yaml": 1, "load_json": 1,
    "jwt_keypair": 1, "jwt_sign": 1, "jwks": 1,
    "healthcheck": 1, "interface": 1, "port": 1, "image": 1,
    "reset": 1, "seed": 1, "depends_on": 1,
    "print": 1, "len": 1, "range": 1, "str": 1, "int": 1, "dict": 1, "list": 1,
  };

  // Note: the regex `/[<]/g` is deliberately written with a character
  // class so the literal two-character sequence `</` never appears in
  // this file. The Go side runs escapeScriptContent on the embedded JS
  // to neutralise `</script>` closures; a bare `/</g` would be mangled
  // to `/<\/g` and break the regex parser.
  function htmlEscape(s) {
    return s.replace(/&/g, "&amp;").replace(/[<]/g, "&lt;").replace(/[>]/g, "&gt;");
  }

  function tok(type, value) {
    return '<span class="tok-' + type + '">' + htmlEscape(value) + "</span>";
  }

  function highlightStarlark(line) {
    var out = "";
    var i = 0;
    var expectDefName = false;
    while (i < line.length) {
      var c = line.charAt(i);

      // Comment runs to end of line.
      if (c === "#") {
        out += tok("comment", line.slice(i));
        break;
      }

      // String literal — match the opening quote, skip escapes.
      if (c === '"' || c === "'") {
        var q = c;
        var j = i + 1;
        while (j < line.length) {
          if (line.charAt(j) === "\\") { j += 2; continue; }
          if (line.charAt(j) === q) { j++; break; }
          j++;
        }
        out += tok("string", line.slice(i, j));
        i = j;
        continue;
      }

      // Number (decimal, int or float).
      if (/\d/.test(c)) {
        var nm = /^\d+(?:\.\d+)?/.exec(line.slice(i));
        if (nm) {
          out += tok("number", nm[0]);
          i += nm[0].length;
          continue;
        }
      }

      // Identifier / keyword / builtin.
      if (/[a-zA-Z_]/.test(c)) {
        var idm = /^[a-zA-Z_][a-zA-Z0-9_]*/.exec(line.slice(i));
        if (idm) {
          var word = idm[0];
          var klass = "ident";
          if (expectDefName) { klass = "def"; expectDefName = false; }
          else if (STAR_KEYWORDS[word]) {
            klass = "keyword";
            if (word === "def") expectDefName = true;
          } else if (STAR_BUILTINS[word]) {
            klass = "builtin";
          }
          if (klass === "ident") {
            out += htmlEscape(word);
          } else {
            out += tok(klass, word);
          }
          i += word.length;
          continue;
        }
      }

      // Whitespace / punctuation — passthrough, escape only if meaningful.
      out += htmlEscape(c);
      i++;
    }
    return out;
  }

  // focusSpecLine expands the matching spec file, clears any prior
  // highlight, adds a yellow band on the target line, scrolls it into
  // view, and returns true on success. Callers (service links,
  // drill-down source jumps) don't need to do the heavy lifting.
  function focusSpecLine(path, line) {
    if (!specIndex || !specIndex.byPath[path]) return false;
    var file = specIndex.byPath[path].element;
    if (!file) return false;
    if (!file.open) file.open = true;

    // Clear previous hits across all spec files.
    var hits = document.querySelectorAll(".source-line.hit");
    for (var i = 0; i < hits.length; i++) hits[i].classList.remove("hit");

    var row = document.getElementById(specAnchorId(path, line));
    if (!row) return false;
    row.classList.add("hit");
    // Also bring the top-level section into view before scrolling
    // inside it, so users on a tall page don't miss the jump.
    file.scrollIntoView({ block: "start", behavior: "smooth" });
    setTimeout(function () {
      row.scrollIntoView({ block: "center" });
    }, 60);
    return true;
  }

  // specJumpLink builds a click-to-jump anchor that closes the
  // drill-down dialog and scrolls the top-level Spec section to the
  // requested file/line. Same affordance the Recent / Location
  // assertion link uses, kept consistent across the dialog.
  function specJumpLink(path, line, label) {
    return el("a", {
      class: "dd-assertion-link",
      href: "#" + specAnchorId(path, line),
      text: label,
      onclick: (function (p, l) {
        return function (e) {
          e.preventDefault();
          closeDrillDown();
          setTimeout(function () { focusSpecLine(p, l); }, 120);
        };
      })(path, line),
    });
  }

  function escapeRegExp(s) {
    return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  }

  // findDefinition searches spec files for the first line matching a
  // pattern. Returns {path, line} or null.
  function findDefinition(pattern) {
    if (!specIndex) return null;
    for (var p in specIndex.byPath) {
      var body = specIndex.byPath[p].body || "";
      var lines = body.split(/\r?\n/);
      for (var i = 0; i < lines.length; i++) {
        if (pattern.test(lines[i])) return { path: p, line: i + 1 };
      }
    }
    return null;
  }

  function findTestLine(testName) {
    var pat = new RegExp("^\\s*def\\s+" + escapeRegExp(testName) + "\\s*\\(");
    var direct = findDefinition(pat);
    if (direct) return direct;
    // Matrix-generated names have no literal `def`. Find the
    // `fault_matrix(` call that produced the test instead, and
    // record the decomposed scenario / fault names so the caller
    // can surface them as secondary anchors.
    if (/^test_matrix_/.test(testName)) {
      var matrix = findDefinition(/^\s*fault_matrix\s*\(/);
      if (matrix) {
        var parts = decomposeMatrixTestName(testName);
        return {
          path: matrix.path,
          line: matrix.line,
          generated: true,
          scenario: parts.scenario,
          fault: parts.fault,
        };
      }
    }
    return null;
  }

  // decomposeMatrixTestName splits "test_matrix_<scenario>_<fault>"
  // into its parts by trying every cut between underscores. The
  // scenario half must match a `def <name>(` in the spec, the fault
  // half a `<name> = fault_assumption(`. Returns blanks if neither
  // half resolves — caller still anchors on the matrix() line.
  function decomposeMatrixTestName(testName) {
    var rest = testName.replace(/^test_matrix_/, "");
    var tokens = rest.split("_");
    for (var split = tokens.length - 1; split >= 1; split--) {
      var scenarioName = tokens.slice(0, split).join("_");
      var faultName = tokens.slice(split).join("_");
      var scenarioHit = findDefinition(
        new RegExp("^\\s*def\\s+" + escapeRegExp(scenarioName) + "\\s*\\("));
      var faultHit = findDefinition(
        new RegExp("^\\s*" + escapeRegExp(faultName) + "\\s*=\\s*fault_assumption\\s*\\("));
      if (scenarioHit && faultHit) {
        return { scenario: { name: scenarioName, def: scenarioHit },
                 fault: { name: faultName, def: faultHit } };
      }
    }
    return { scenario: null, fault: null };
  }

  function findServiceLine(serviceName) {
    var esc = escapeRegExp(serviceName);
    var pat = new RegExp(
      "(?:^|\\b)(?:service|mock_service)\\s*\\(\\s*[\"']" + esc + "[\"']");
    return findDefinition(pat);
  }

  // ── Clipboard ───────────────────────────────────────────────────
  function copyToClipboard(text, btn) {
    function done() {
      if (!btn) return;
      btn.classList.add("copied");
      var orig = btn.textContent;
      btn.textContent = "Copied";
      setTimeout(function () {
        btn.classList.remove("copied");
        btn.textContent = orig;
      }, 1400);
    }
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(done).catch(fallback);
      return;
    }
    fallback();
    function fallback() {
      var ta = document.createElement("textarea");
      ta.value = text;
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      try { document.execCommand("copy"); } catch (e) { /* ignore */ }
      document.body.removeChild(ta);
      done();
    }
  }

  // ── Main ────────────────────────────────────────────────────────
  async function main() {
    var data = await loadData();
    if (!data || !data.manifest) {
      console.warn("faultbox: no manifest; nothing to render");
      return;
    }
    // Expose for console debugging; harmless in production.
    window.__FAULTBOX__ = data;

    indexTests(data);

    var metrics = deriveMetrics(data);
    renderHeaderMeta(data);
    renderHeroStats(data, metrics);
    renderAttention(data);
    renderMatrix(data);
    renderTestsTable(data);
    // renderSpec must run before renderCoverage because the coverage
    // table builds service-name links via findServiceLine(), which
    // reads the specIndex populated by renderSpec. Without this order
    // every service name in the coverage table falls back to plain
    // text — the links still render, but they're not actionable.
    renderSpec(data);
    renderCoverage(data);
    renderPlan(data);
    renderRepro(data);

    wireDrillDownClose();
    openFromLocationHash();
  }

  // openFromLocationHash lets users share direct-to-drill-down URLs
  // like `report.html#test=test_foo`. Each test also updates the hash
  // when opened, so a back button closes the dialog.
  function openFromLocationHash() {
    var h = window.location.hash || "";
    var m = h.match(/^#test=(.+)$/);
    if (m && m[1]) openDrillDown(decodeURIComponent(m[1]));
    window.addEventListener("hashchange", function () {
      var h2 = window.location.hash || "";
      if (!h2) { closeDrillDown(); return; }
      var mm = h2.match(/^#test=(.+)$/);
      if (mm && mm[1]) openDrillDown(decodeURIComponent(mm[1]));
    });
  }

  function wireDrillDownClose() {
    var dialog = document.getElementById("drill-down");
    if (!dialog) return;
    var closeBtn = dialog.querySelector(".drill-down-close");
    if (closeBtn) closeBtn.addEventListener("click", closeDrillDown);
    var expandBtn = dialog.querySelector(".drill-down-expand");
    if (expandBtn) {
      expandBtn.addEventListener("click", function () {
        dialog.classList.toggle("fullscreen");
        // Swap glyph: ⤢ (expand) ↔ ⤡ (contract)
        expandBtn.textContent = dialog.classList.contains("fullscreen") ? "⤡" : "⤢";
      });
    }
    // Clicking the backdrop (anything outside the surface) closes too.
    dialog.addEventListener("click", function (e) {
      if (e.target === dialog) closeDrillDown();
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", function () { main(); });
  } else {
    main();
  }
})();
