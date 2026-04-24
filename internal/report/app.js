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
  function loadData() {
    var node = document.getElementById("faultbox-data");
    if (!node) return null;
    try {
      return JSON.parse(node.textContent || "{}");
    } catch (err) {
      console.error("faultbox: failed to parse embedded data", err);
      return null;
    }
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

  // RFC-027: map the four manifest outcomes to pill / matrix-cell
  // classes. Unknown outcomes fall back to "warn" so a future
  // schema_version that adds a tag we haven't shipped still renders
  // visibly (rather than vanishing into default text).
  function outcomeClass(outcome) {
    switch (outcome) {
      case "passed": return "pass";
      case "failed": return "fail";
      case "expectation_violated": return "violated";
      case "errored": return "errored";
      default: return "warn";
    }
  }

  // outcomeFromTrace collapses the legacy trace.json result field plus
  // the RFC-027 expectation_violated refinement into the canonical
  // manifest string. Kept in one place so the drill-down header, the
  // tests table, and the matrix stay in lockstep.
  function outcomeFromTrace(test) {
    if (!test) return "";
    if (test.result === "pass") return "passed";
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
      case "errored": icon = "!"; break;
      default: icon = "?";
    }
    var title = cell.scenario + " × " + cell.fault + "\n" + outcome;
    if (cell.expectation) title += " (" + cell.expectation + ")";
    if (outcome !== "passed" && cell.reason) title += "\n" + cell.reason;
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
      var tr = el("tr", {
        "data-test": r.name,
        onclick: (function (n) { return function () { openDrillDown(n); }; })(r.name),
      }, [
        el("td", { text: r.name }),
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
    // the breakdowns and track tests touching each service.
    var byService = {};
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
      }
    }

    var services = Object.keys(byService);
    if (!services.length) return;
    services.sort();
    section.hidden = false;
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
    // Reason / assertion message (shown whether or not the test failed
    // — a passing test with a reason is rare, but we want it visible).
    if (test.reason) {
      body.appendChild(el("h4", { text: "Reason" }));
      body.appendChild(el("div", {
        class: "drill-down-reason " + (test.result === "pass" ? "pass" : ""),
        text: test.reason,
      }));
    }

    // Faults applied.
    if (test.faults && test.faults.length) {
      body.appendChild(el("h4", { text: "Faults applied" }));
      body.appendChild(faultsTable(test.faults));
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
    var summaryMeta = def
      ? "<span>" + def.path.replace(/^spec\//, "") + " · line " + def.line + "</span>"
      : "<span>no matching spec def</span>";
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

    var lanes = {};
    var order = [];
    for (var i = 0; i < events.length; i++) {
      var svc = events[i].service || "test";
      if (!lanes[svc]) { lanes[svc] = []; order.push(svc); }
      lanes[svc].push(events[i]);
    }

    var seqs = events.map(function (e) { return e.seq || 0; });
    var minSeq = Math.min.apply(null, seqs);
    var maxSeq = Math.max.apply(null, seqs);
    if (minSeq === maxSeq) maxSeq = minSeq + 1;

    var host = el("div", { class: "trace-viewer" });
    var lanesWrap = el("div", { class: "trace-lanes" });
    var grid = el("div", { class: "trace-grid" });
    grid.style.gridTemplateColumns = "max-content 1fr";

    var markerNodes = {}; // seq -> marker node
    var laneIndexFor = {}; // service -> zero-based lane row index

    for (var j = 0; j < order.length; j++) {
      var svc = order[j];
      laneIndexFor[svc] = j;
      grid.appendChild(el("div", { class: "trace-lane-label", text: svc }));
      var lane = el("div", { class: "trace-lane" });
      var laneEvents = lanes[svc];
      for (var k = 0; k < laneEvents.length; k++) {
        var ev = laneEvents[k];
        var m = renderMarker(ev, minSeq, maxSeq);
        markerNodes[ev.seq] = m;
        lane.appendChild(m);
      }
      grid.appendChild(lane);
    }
    lanesWrap.appendChild(grid);

    // SVG overlay for causal lines. Mounted inside the lanes wrapper
    // so its coordinate system shares the grid's layout — avoids
    // reflow math when the viewer resizes.
    var svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    svg.setAttribute("class", "trace-causal-svg");
    lanesWrap.appendChild(svg);

    // Side-mounted hover balloon — small, stays out of the causal
    // line's direct path because it offsets to one side of the
    // marker instead of straight above.
    var tooltip = el("div", { class: "trace-tooltip", role: "tooltip" });
    lanesWrap.appendChild(tooltip);

    host.appendChild(lanesWrap);
    host.appendChild(el("div", { class: "trace-axis" }, [
      el("span", { text: "seq " + minSeq }),
      el("span", { text: "seq " + maxSeq }),
    ]));
    host.appendChild(traceLegend());

    // Detail panel (click-to-pin) below the axis/legend.
    var detail = el("div", { class: "trace-detail empty",
      text: "Click any point in the timeline to see its details here." });
    host.appendChild(detail);

    // Track the pinned event inside this closure so hover teardown
    // can redraw the pinned overlay instead of clearing it (keeping
    // the selected ring + causal lines visible between hovers).
    var pinned = { ev: null };

    // Event log table (collapsible, default-open) — forensic companion.
    var eventLog = buildEventLog(events, order, function (ev) {
      pinSelection(ev, host, markerNodes, detail, events, svg,
        order, laneIndexFor, pinned);
    });
    host.appendChild(eventLog);

    // Wire marker events. A closure per marker captures its event.
    var lookup = {};
    for (var i2 = 0; i2 < events.length; i2++) lookup[events[i2].seq] = events[i2];
    for (var s in markerNodes) {
      (function (ev, node) {
        node.addEventListener("mouseenter", function () {
          showTraceHover(ev, events, lanesWrap, node, tooltip, svg,
            markerNodes, order, laneIndexFor);
        });
        node.addEventListener("focus", function () {
          showTraceHover(ev, events, lanesWrap, node, tooltip, svg,
            markerNodes, order, laneIndexFor);
        });
        node.addEventListener("mouseleave", function () {
          hideTraceHover(lanesWrap, tooltip, svg, markerNodes,
            events, pinned);
        });
        node.addEventListener("blur", function () {
          hideTraceHover(lanesWrap, tooltip, svg, markerNodes,
            events, pinned);
        });
        node.addEventListener("click", function (e) {
          e.stopPropagation();
          pinSelection(ev, host, markerNodes, detail, events, svg,
            order, laneIndexFor, pinned);
        });
        node.addEventListener("keydown", function (e) {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            pinSelection(ev, host, markerNodes, detail, events, svg,
              order, laneIndexFor, pinned);
          }
        });
      })(lookup[s], markerNodes[s]);
    }

    return host;
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
    detail.appendChild(buildDetailContent(ev));

    // Open the event log if closed so scroll + highlight land in view.
    var log = host.querySelector(".trace-log");
    if (log && !log.open) log.open = true;

    // Highlight matching log row and scroll it into view.
    var row = host.querySelector('.trace-log-table tr[data-seq="' + ev.seq + '"]');
    if (row) {
      row.classList.add("selected");
      row.scrollIntoView({ block: "nearest" });
    }

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

  function renderMarker(ev, minSeq, maxSeq) {
    var pct = ((ev.seq || 0) - minSeq) / (maxSeq - minSeq) * 100;
    // Clamp to a narrow inner band so the first and last markers don't
    // overhang the lane's rounded corners. 2.5% ≈ enough clearance for
    // the 14px violation square on an ~800px-wide lane.
    if (pct < 2.5) pct = 2.5;
    if (pct > 97.5) pct = 97.5;
    var kind = markerKind(ev);
    var label = markerShortLabel(ev);
    var m = el("div", {
      class: "trace-marker " + kind,
      tabindex: "0",
      role: "button",
      "aria-label": label,
      "data-seq": String(ev.seq || 0),
    });
    m.style.left = pct.toFixed(2) + "%";
    return m;
  }

  function markerKind(ev) {
    var t = ev.type || "";
    if (t === "fault_applied" || t === "fault_removed") return "fault";
    if (t === "violation") return "violation";
    if (t === "syscall") return "syscall";
    if (t === "step_send" || t === "step_recv") return "step";
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

    // Clamp horizontally after layout if needed.
    requestAnimationFrame(function () {
      var tr = tooltip.getBoundingClientRect();
      var overshoot = tr.right - lanesRect.right;
      if (overshoot > 0) {
        tooltip.style.left = (cx - offsetX - tr.width) + "px";
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
    var head = el("div", { class: "trace-tooltip-head", text: ev.type || "event" });
    var sub = el("div", { class: "trace-tooltip-sub" });
    var bits = [];
    if (ev.service) bits.push(ev.service);
    if (ev.fields && ev.fields.syscall) bits.push(ev.fields.syscall);
    if (ev.fields && ev.fields.decision) bits.push("[" + ev.fields.decision + "]");
    bits.push("seq " + (ev.seq || 0));
    sub.textContent = bits.join(" · ");
    return el("div", null, [head, sub]);
  }

  // buildDetailContent is the pinned-selection view shown below the
  // lanes. Headline fields (type, service, primary summary) sit at
  // the top; everything else tucks into collapsibles so the common
  // case stays compact.
  function buildDetailContent(ev) {
    var head = el("div", { class: "trace-detail-head" }, [
      el("span", { class: "trace-detail-title", text: ev.type || "event" }),
      el("span", { class: "trace-detail-meta",
        text: (ev.service || "—") + " · seq " + (ev.seq || 0) +
              (ev.timestamp ? " · " + fmtTimestamp(ev.timestamp) : "") }),
    ]);

    var primary = el("div");
    primary.appendChild(detailRow("service", ev.service || "—", true));
    var summary = eventHeadline(ev);
    if (summary) primary.appendChild(detailRow("summary", summary, true));
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
    var vcChildren = (ev.vector_clock && Object.keys(ev.vector_clock).length)
      ? [el("div", { class: "trace-detail-kv", text: formatVectorClock(ev.vector_clock) })]
      : [el("div", { class: "trace-detail-kv trace-detail-value", text: "(not recorded)" })];
    container.appendChild(collapsible("Vector clock", vcChildren));
    return container;
  }

  function detailRow(label, value, emphasis) {
    var v = el("div", { class: "trace-detail-value" + (emphasis ? " emphasis" : ""),
      text: String(value) });
    return el("div", { class: "trace-detail-row" }, [
      el("div", { class: "trace-detail-label", text: label }),
      v,
    ]);
  }

  function collapsible(title, children) {
    var det = document.createElement("details");
    det.className = "trace-detail-collapsible";
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
  function eventHeadline(ev) {
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
        var q = truncate(f.query || "query", 80);
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
      var arrow = t === "step_send" ? "→" : "←";
      var parts = [arrow, f.target || "?"];
      if (f.method) parts.push(f.method);
      if (f.path) parts.push(f.path);
      else if (f.interface) parts.push("[" + f.interface + "]");
      if (t === "step_recv" && f.status) parts.push("→ " + f.status);
      return parts.join(" ");
    }

    if (t === "fault_applied" || t === "fault_removed") {
      var pre = t === "fault_applied" ? "+" : "−";
      var body = (f.syscall || "?") + " " + (f.action || "");
      if (f.errno) body += " " + f.errno;
      if (f.label) body += " [" + f.label + "]";
      return pre + " " + body;
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

  // buildEventLog returns a collapsible <details> containing the
  // event log table with type-filter chips. Clicking a row invokes
  // onSelect — the same flow as clicking a marker.
  function buildEventLog(events, order, onSelect) {
    var typesPresent = {};
    for (var i = 0; i < events.length; i++) {
      var t = events[i].type || "other";
      typesPresent[t] = true;
    }
    var knownTypes = ["syscall", "fault_applied", "fault_removed",
      "service_started", "service_ready", "service_stopped",
      "step_send", "step_recv", "violation"];
    var filterOpts = ["all"];
    for (var j = 0; j < knownTypes.length; j++) {
      if (typesPresent[knownTypes[j]]) filterOpts.push(knownTypes[j]);
    }
    // Append any unknown types so no events are unreachable by filter.
    for (var kt in typesPresent) {
      if (knownTypes.indexOf(kt) < 0 && kt !== "other") filterOpts.push(kt);
    }

    var wrap = document.createElement("details");
    wrap.className = "trace-log";
    // Default-open so the forensic view is discoverable; users who
    // prefer a compact modal can collapse it with one click.
    wrap.open = true;
    var sum = document.createElement("summary");
    sum.innerHTML = "<span>Event log — " + events.length + " entries</span>"
      + "<span>click to toggle</span>";
    wrap.appendChild(sum);

    var body = el("div", { class: "trace-log-body" });
    var filters = el("div", { class: "trace-log-filters", role: "toolbar" });
    var chips = {};
    for (var f = 0; f < filterOpts.length; f++) {
      (function (type) {
        var chip = el("button", {
          class: "trace-log-chip" + (type === "all" ? " active" : ""),
          type: "button", "data-filter": type, text: type,
          onclick: function () { applyFilter(type); },
        });
        chips[type] = chip;
        filters.appendChild(chip);
      })(filterOpts[f]);
    }
    body.appendChild(filters);

    var scroll = el("div", { class: "trace-log-scroll" });
    var table = el("table", { class: "trace-log-table" });
    var thead = el("thead", null, [el("tr", null, [
      el("th", { class: "caret-col", "aria-label": "expand" }),
      el("th", { class: "type", text: "type" }),
      el("th", { class: "service", text: "service" }),
      el("th", { text: "summary" }),
    ])]);
    var tbody = el("tbody");
    for (var r = 0; r < events.length; r++) {
      var pair = logRow(events[r], onSelect);
      tbody.appendChild(pair[0]);
      tbody.appendChild(pair[1]);
    }
    table.appendChild(thead);
    table.appendChild(tbody);
    scroll.appendChild(table);
    body.appendChild(scroll);
    wrap.appendChild(body);

    function applyFilter(type) {
      for (var k in chips) chips[k].classList.remove("active");
      if (chips[type]) chips[type].classList.add("active");
      // Each event now contributes two rows (header + expansion); we
      // key on data-seq / data-seq-expand so a filter match hides
      // or shows both in lockstep.
      var headers = tbody.querySelectorAll("tr[data-seq]");
      for (var i = 0; i < headers.length; i++) {
        var row = headers[i];
        var rType = row.getAttribute("data-type") || "";
        var seq = row.getAttribute("data-seq");
        var matched = (type === "all" || rType === type);
        row.style.display = matched ? "" : "none";
        var expand = tbody.querySelector('tr[data-seq-expand="' + seq + '"]');
        if (expand) {
          // Only the header row toggles visibility when filtered —
          // the expansion's own `hidden` attribute controls open/close.
          expand.style.display = matched ? "" : "none";
        }
      }
    }

    return wrap;
  }

  // logRow builds one <tr> + a sibling expansion <tr> that toggles on
  // click. The expansion carries the grouped detail view (Request /
  // Response / Fault / System / Other) so users can inspect rich
  // per-event fields right where they clicked, without scrolling back
  // to the trace-viewer detail panel. Clicking the row also drives
  // onSelect — the standard "pin this event on the lanes" behaviour.
  function logRow(ev, onSelect) {
    var caret = el("span", { class: "trace-log-caret", text: "▸" });
    var tr = el("tr", {
      "data-seq": String(ev.seq || 0),
      "data-type": ev.type || "",
    });
    // The sequence number used to live in this first column alongside
    // the caret, but the two competed for a 1%-wide column and wrapped
    // on narrow viewports. The seq is still a useful anchor — moved
    // into the expansion's Meta group below so it stays discoverable
    // without cluttering the row.
    tr.appendChild(el("td", { class: "caret-col" }, [caret]));
    tr.appendChild(el("td", { class: "type" }, [
      el("span", { class: "trace-log-type type-" + markerKind(ev), text: ev.type || "" }),
    ]));
    tr.appendChild(el("td", { class: "service", text: ev.service || "" }));
    tr.appendChild(el("td", { text: eventHeadline(ev) || "" }));

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
    var row = el("div", { class: "log-expand-kv-row" }, [
      el("div", { class: "log-expand-kv-key", text: k }),
      el("div", { class: "log-expand-kv-val", text: String(v) }),
    ]);
    return row;
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

  // findCausalAncestors returns, for each other lane, the latest
  // event Y such that Y happens-before target. This keeps the
  // visualisation legible: at most (N-1) lines per hovered marker,
  // not the full transitive set.
  function findCausalAncestors(target, allEvents) {
    var perService = {};
    var targetSvc = target.service || "test";
    for (var i = 0; i < allEvents.length; i++) {
      var e = allEvents[i];
      if (e === target) continue;
      var svc = e.service || "test";
      if (svc === targetSvc) continue;
      if (!happensBefore(e, target)) continue;
      var existing = perService[svc];
      if (!existing || (e.seq || 0) > (existing.seq || 0)) perService[svc] = e;
    }
    var out = [];
    for (var s in perService) out.push(perService[s]);
    return out;
  }

  function drawCausalLines(svg, rootNode, ancestors, host, markerNodes) {
    svg.innerHTML = "";
    if (!ancestors.length) return;
    var hostRect = host.getBoundingClientRect();
    var rootRect = rootNode.getBoundingClientRect();
    var rx = rootRect.left - hostRect.left + rootRect.width / 2;
    var ry = rootRect.top - hostRect.top + rootRect.height / 2;

    for (var i = 0; i < ancestors.length; i++) {
      var a = ancestors[i];
      var ancNode = markerNodes[a.seq];
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
    return findDefinition(pat);
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
  function main() {
    var data = loadData();
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
    // Clicking the backdrop (anything outside the surface) closes too.
    dialog.addEventListener("click", function (e) {
      if (e.target === dialog) closeDrillDown();
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", main);
  } else {
    main();
  }
})();
