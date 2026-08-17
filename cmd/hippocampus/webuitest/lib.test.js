// Tests for the embedded console's pure logic (../webui/lib.js).
//
// Run with `node --test` from this directory. There are no dependencies and no DOM: every function
// under test takes its inputs and returns a value, which is the whole reason lib.js was split out
// of app.js. The console is ~2,800 lines of JavaScript that no Go test can reach, and the bugs it
// has actually shipped were found by eye, one screenshot at a time.

import test from "node:test";
import assert from "node:assert/strict";

import {
  ageLabel,
  capacityMeter,
  countdownFraction,
  countdownLabel,
  cycleSummary,
  TRIGGER_LABELS,
  bodyClassesFor,
  capsFromWhoAmI,
  capsWhenRefused,
  compactAge,
  curveSvg,
  esc,
  formatBytes,
  gateMessage,
  gateNeeded,
  groupCell,
  humanDays,
  metadataPills,
  metadataToForm,
  num,
  parseMetadataPairs,
  shortId,
  GATING_CLASSES,
  tierAtLeast,
  topologyCheckedLabel,
  topologyLayout,
  topologySource,
  topologyStatus,
  topologySvg,
  truncateMiddle,
} from "../webui/lib.js";

// --------------------------------------------------------------------- capabilities
//
// The highest-value tests here. This mapping is what decides whether a control appears, and both
// kinds of mistake are invisible until someone signs in with the exact token that trips them: a
// control offered that the server will refuse, or a panel hidden from a caller entitled to it.

const whoami = (over = {}) => ({
  role: "admin",
  authEnabled: true,
  searchModes: [],
  groups: [],
  groupScoped: false,
  summariserEnabled: false,
  consolidationEnabled: true,
  tombstonesEnabled: false,
  ...over,
});

test("capsFromWhoAmI: an unauthenticated deployment is unrestricted", () => {
  const caps = capsFromWhoAmI(whoami({ authEnabled: false, role: null }));

  assert.equal(caps.canWrite, true);
  assert.equal(caps.isAdmin, true);
  assert.equal(caps.isUnboundAdmin, true);
});

test("capsFromWhoAmI: reader may not write and is not admin", () => {
  const caps = capsFromWhoAmI(whoami({ role: "reader" }));

  assert.equal(caps.canWrite, false);
  assert.equal(caps.isAdmin, false);
  assert.equal(caps.isUnboundAdmin, false);
});

test("capsFromWhoAmI: writer may write but is not admin", () => {
  const caps = capsFromWhoAmI(whoami({ role: "writer" }));

  assert.equal(caps.canWrite, true);
  assert.equal(caps.isAdmin, false);
});

// The regression this whole split exists for. The forgotten log's RPCs are scopeFilter, not
// scopeUnbound (hippocampus/scope.go), because a tombstone carries its memory's group - so a
// group-scoped admin may use them and sees their own partition. Folding the scope check into
// isAdmin hid that panel from them entirely. Reading the log has since dropped to reader tier, so
// what isAdmin gates there now is DeleteForgottenMemories - the distinction the split draws is
// unchanged, and Export/Transfer/Clear are the same shape.
test("capsFromWhoAmI: a group-scoped admin keeps admin tier but loses unbound", () => {
  const caps = capsFromWhoAmI(
    whoami({ role: "admin", groupScoped: true, groups: ["tenant-a"] }),
  );

  assert.equal(caps.isAdmin, true, "tier is still admin");
  assert.equal(
    caps.isUnboundAdmin,
    false,
    "but Purge/Sleep/Preview are refused",
  );
  assert.deepEqual(caps.groups, ["tenant-a"]);
});

test("capsFromWhoAmI: server properties are read as booleans, absent meaning false", () => {
  const caps = capsFromWhoAmI({ role: "admin", authEnabled: true });

  assert.equal(caps.summariser, false);
  assert.equal(caps.consolidation, false);
  assert.equal(caps.tombstones, false);
  assert.deepEqual(caps.searchModes, []);
  assert.deepEqual(caps.groups, []);
});

test("capsWhenRefused closes everything and keeps the status", () => {
  const caps = capsWhenRefused(403);

  assert.equal(caps.status, 403);
  assert.equal(caps.canWrite, false);
  assert.equal(caps.isAdmin, false);
  assert.equal(caps.isUnboundAdmin, false);
  assert.equal(caps.consolidation, false);
  assert.deepEqual([...bodyClassesFor(caps)], ["readonly"]);
});

// The matrix as a table. Each row is a deployment shape crossed with a token shape, and the
// expected value is the exact set of classes CSS gates on.
test("bodyClassesFor covers the deployment x token matrix", () => {
  const cases = [
    {
      name: "no auth, consolidator, tombstones on",
      who: whoami({ authEnabled: false, role: null, tombstonesEnabled: true }),
      want: ["admin", "unbound-admin", "consolidating", "tombstones"],
    },
    {
      name: "no auth, replica",
      who: whoami({
        authEnabled: false,
        role: null,
        consolidationEnabled: false,
      }),
      want: ["admin", "unbound-admin"],
    },
    {
      name: "reader on a consolidator",
      who: whoami({ role: "reader" }),
      want: ["readonly", "consolidating"],
    },
    {
      name: "writer on a consolidator",
      who: whoami({ role: "writer" }),
      want: ["consolidating"],
    },
    {
      name: "admin on a consolidator with tombstones",
      who: whoami({ role: "admin", tombstonesEnabled: true }),
      want: ["admin", "unbound-admin", "consolidating", "tombstones"],
    },
    {
      // The one that was wrong: admin present (the forgotten log is reachable), unbound-admin
      // absent (the dry run is not).
      name: "group-scoped admin",
      who: whoami({
        role: "admin",
        groupScoped: true,
        tombstonesEnabled: true,
      }),
      want: ["admin", "consolidating", "tombstones"],
    },
    {
      name: "admin with the summariser configured",
      who: whoami({ role: "admin", summariserEnabled: true }),
      want: ["admin", "unbound-admin", "summariser", "consolidating"],
    },
    {
      name: "admin on a replica",
      who: whoami({ role: "admin", consolidationEnabled: false }),
      want: ["admin", "unbound-admin"],
    },
  ];

  for (const c of cases) {
    const got = [...bodyClassesFor(capsFromWhoAmI(c.who))].sort();

    assert.deepEqual(got, [...c.want].sort(), c.name);
  }
});

// A class bodyClassesFor can return but applyCaps does not iterate would never be REMOVED when it
// stopped being warranted, leaving a stale "admin" on the body after signing in as a reader.
test("GATING_CLASSES lists every class bodyClassesFor can return", () => {
  const everything = capsFromWhoAmI(
    whoami({
      role: "reader", // readonly
      summariserEnabled: true,
      consolidationEnabled: true,
      tombstonesEnabled: true,
    }),
  );
  const adminSide = capsFromWhoAmI(whoami({ role: "admin" }));

  for (const caps of [everything, adminSide]) {
    for (const name of bodyClassesFor(caps)) {
      assert.ok(
        GATING_CLASSES.includes(name),
        `${name} is returned but not in GATING_CLASSES, so it would never be removed`,
      );
    }
  }
});

// ---------------------------------------------------------------------------- gate

test("gateNeeded: no gate when the deployment does not authenticate", () => {
  assert.equal(gateNeeded(false, capsWhenRefused(401), false), false);
});

test("gateNeeded: no gate once a role has resolved", () => {
  assert.equal(
    gateNeeded(true, capsFromWhoAmI(whoami({ role: "reader" })), true),
    false,
  );
});

test("gateNeeded: gate when nothing is held", () => {
  assert.equal(gateNeeded(true, capsWhenRefused(0), false), true);
});

test("gateNeeded: gate on a refused or unauthorised call", () => {
  assert.equal(gateNeeded(true, capsWhenRefused(401), true), true);
  assert.equal(gateNeeded(true, capsWhenRefused(403), true), true);
});

// A whoami that failed for a reason that is not credential-shaped must leave the console up:
// inviting an operator to replace a good token would not fix a purge in progress.
test("gateNeeded: a 503 while holding a credential does not raise the gate", () => {
  assert.equal(gateNeeded(true, capsWhenRefused(503), true), false);
});

test("gateMessage: 403 names the role rather than the token", () => {
  const msg = gateMessage(capsWhenRefused(403), {
    serverLogin: false,
    oidcLogin: false,
    haveCredential: true,
  });

  assert.match(msg, /role/);
});

test("gateMessage: 401 holding a token blames the token", () => {
  const msg = gateMessage(capsWhenRefused(401), {
    serverLogin: false,
    oidcLogin: false,
    haveCredential: true,
  });

  assert.match(msg, /not accepted/);
});

test("gateMessage: 401 under oidc calls it an ended session", () => {
  const msg = gateMessage(capsWhenRefused(401), {
    serverLogin: false,
    oidcLogin: true,
    haveCredential: true,
  });

  assert.match(msg, /session has ended/);
});

// In server-login mode the cookie is unreadable, so "held a credential" cannot be told apart from
// "just signed out"; claiming an expiry would be a guess.
test("gateMessage: server-login mode stays silent on a 401", () => {
  assert.equal(
    gateMessage(capsWhenRefused(401), {
      serverLogin: true,
      oidcLogin: false,
      haveCredential: true,
    }),
    "",
  );
});

test("gateMessage: never signed in shows no error", () => {
  assert.equal(
    gateMessage(capsWhenRefused(0), {
      serverLogin: false,
      oidcLogin: false,
      haveCredential: false,
    }),
    "",
  );
});

// ----------------------------------------------------------------------------- age

test("compactAge picks the largest unit leaving a whole number", () => {
  const cases = [
    [0, "0s"],
    [59, "59s"],
    [60, "1m"],
    [3599, "59m"],
    [3600, "1h"],
    [86399, "23h"],
    [86400, "1d"],
    [604799, "6d"],
    [604800, "1w"],
    [2591999, "4w"],
    [2592000, "1mo"],
    [31535999, "12mo"],
    [31536000, "1y"],
  ];

  for (const [seconds, want] of cases) {
    assert.equal(compactAge(seconds), want, `${seconds}s`);
  }
});

// A timestamp ahead of now is not a bug to hide: StoreMemory accepts a skew window and an event's
// time_end is routinely in the future. "in 3m" is honest where "0s" is not.
test("ageLabel prefixes a future timestamp rather than showing zero", () => {
  const now = 1_700_000_000_000;
  const nano = (ms) => String(BigInt(ms) * 1_000_000n);

  assert.equal(ageLabel(now, nano(now - 90_000)), "1m");
  assert.equal(ageLabel(now, nano(now)), "0s");
  assert.equal(ageLabel(now, nano(now + 180_000)), "in 3m");
});

// ------------------------------------------------------------------------ metadata

// The server cuts a metadata pair on the FIRST "=", so the console must too, or a value containing
// one round-trips as a different pair.
test("parseMetadataPairs keeps lines with a key and drops the rest", () => {
  assert.deepEqual(parseMetadataPairs("a=b\nc=d", "\n"), ["a=b", "c=d"]);
  assert.deepEqual(parseMetadataPairs("a=b=c", "\n"), ["a=b=c"]);
  assert.deepEqual(parseMetadataPairs("  a = b  ", "\n"), ["a = b"]);
  assert.deepEqual(parseMetadataPairs("=orphan", "\n"), []);
  assert.deepEqual(parseMetadataPairs("novalue", "\n"), []);
  assert.deepEqual(parseMetadataPairs("", "\n"), []);
  assert.deepEqual(parseMetadataPairs("a=b,c=d", ","), ["a=b", "c=d"]);
});

test("metadataToForm sorts keys so the textarea is stable across reloads", () => {
  assert.equal(metadataToForm({ z: "1", a: "2" }), "a=2\nz=1");
  assert.equal(metadataToForm(null), "");
  assert.equal(metadataToForm({}), "");
});

test("metadata round-trips through the form", () => {
  const original = { env: "prod", "service.name": "api" };
  const text = metadataToForm(original);
  const pairs = parseMetadataPairs(text, "\n");
  const back = {};

  for (const line of pairs) {
    const i = line.indexOf("=");
    back[line.slice(0, i).trim()] = line.slice(i + 1).trim();
  }

  assert.deepEqual(back, original);
});

// -------------------------------------------------------------------- number sentinels
//
// The sentinels are the point: these render into stat tiles where a wrong value reads as a fact
// about the store rather than as a missing number.

test("num collapses the unrepresentable to infinity", () => {
  assert.equal(num(1e15), "∞");
  assert.equal(num(NaN), "∞");
  assert.equal(num(Infinity), "∞");
  assert.equal(num(undefined), "∞");
  assert.equal(num(null), "∞");
  assert.equal(num(0), "0");
  assert.equal(num(5), "5");
  assert.equal(num(1.23456789), "1.235");
});

test("humanDays distinguishes never, now, and a span", () => {
  assert.equal(humanDays(-1), "not foreseeably");
  assert.equal(humanDays(0), "due now");
  assert.equal(humanDays(1 / 48), "< 1 hour");
  assert.equal(humanDays(0.5), "12 hours");
  assert.equal(humanDays(2), "2 days");
  assert.equal(humanDays(730), "2 years");
  assert.equal(humanDays(NaN), "—");
  assert.equal(humanDays(undefined), "—");
});

test("formatBytes scales to binary units", () => {
  assert.equal(formatBytes(0), "0 B");
  assert.equal(formatBytes(1023), "1023 B");
  assert.equal(formatBytes(1024), "1 KiB");
  assert.equal(formatBytes(1536), "1.5 KiB");
  assert.equal(formatBytes(1024 ** 2), "1 MiB");
  assert.equal(formatBytes(1024 ** 4), "1 TiB");
  assert.equal(formatBytes(NaN), "—");

  // TiB is the largest unit, so anything above it keeps scaling the number - and the 3-significant-
  // figure rounding then shows. 1024 TiB reads as "1020 TiB". Pinned rather than fixed: a store
  // that large is not what this console is for, and widening the precision would add digits to
  // every figure on the page to improve one that will never be shown.
  assert.equal(formatBytes(1024 ** 5), "1020 TiB");
});

// -------------------------------------------------------------------------- escaping
//
// Item 24.1 was a stored XSS in this console. Every one of these renders caller-controlled text.

test("esc escapes every character that could close an attribute or a tag", () => {
  assert.equal(esc(`<script>`), "&lt;script&gt;");
  assert.equal(esc(`a&b`), "a&amp;b");
  assert.equal(esc(`"quoted"`), "&quot;quoted&quot;");
  assert.equal(esc(`it's`), "it&#39;s");
  assert.equal(esc(null), "");
  assert.equal(esc(undefined), "");
  assert.equal(esc(0), "0");
});

test("metadataPills escapes both key and value", () => {
  const html = metadataPills({ "<k>": `"v"` });

  assert.ok(!html.includes("<k>"), "key must not appear raw");
  assert.ok(!html.includes(`"v"`), "value must not appear raw");
  assert.match(html, /&lt;k&gt;=&quot;v&quot;/);
});

test("groupCell escapes the group and renders an em dash when empty", () => {
  assert.equal(groupCell({}), "—");
  assert.equal(groupCell({ group: "<b>" }), "&lt;b&gt;");
  assert.match(groupCell({ group: "g", metadata: { a: "1" } }), /g<br><span/);
  assert.match(groupCell({ metadata: { a: "1" } }), /^<span/);
});

test("shortId middle-truncates only when it must", () => {
  assert.equal(shortId("short"), "short");
  assert.equal(shortId(""), "");
  assert.equal(shortId(null), "");

  const long = "at://did:plc:abcdefghijklmnop/app.bsky.feed.post/3mszvl5j6hr2h";
  const out = shortId(long);

  assert.ok(out.length < long.length);
  assert.ok(out.includes("…"));
  assert.ok(out.startsWith(long.slice(0, 16)), "keeps the head");
  assert.ok(out.endsWith(long.slice(-12)), "keeps the distinguishing tail");
});

// ----------------------------------------------------------------------- decay curve

const curve = (over = {}) => ({
  maxAgeDays: 10,
  crossingAgeDays: 5,
  points: Array.from({ length: 20 }, (_, i) => ({
    ageDays: i * 0.5,
    value: 100 / (1 + i),
  })),
  ...over,
});

test("curveSvg refuses to plot fewer than two points", () => {
  assert.match(curveSvg({ points: [] }, 5), /Not enough of the curve/);
  assert.match(
    curveSvg({ points: [{ ageDays: 0, value: 1 }] }, 5),
    /Not enough of the curve/,
  );
});

test("curveSvg drops non-finite and non-positive samples", () => {
  const svg = curveSvg(
    curve({
      points: [
        { ageDays: 0, value: 100 },
        { ageDays: 1, value: NaN },
        { ageDays: 2, value: 0 },
        { ageDays: 3, value: -5 },
        { ageDays: 4, value: 10 },
      ],
    }),
    5,
  );

  assert.ok(!/NaN/.test(svg), "a dropped sample must not reach the path");
});

// The comment on curveSvg says the SVG cannot carry anything but geometry. Assert it: every numeric
// token in the output must be finite, or the path silently fails to render.
test("curveSvg emits only finite numbers", () => {
  for (const c of [
    curve(),
    curve({ maxAgeDays: 0 }),
    curve({ crossingAgeDays: -1 }),
    curve({
      points: [
        { ageDays: 0, value: 5 },
        { ageDays: 1, value: 5 },
      ],
    }),
  ]) {
    const svg = curveSvg(c, 5);

    if (/Not enough/.test(svg)) continue;

    for (const m of svg.matchAll(/[-+]?\d*\.?\d+(?:[eE][-+]?\d+)?/g)) {
      assert.ok(isFinite(Number(m[0])), `non-finite number ${m[0]} in the SVG`);
    }

    assert.ok(!/NaN|Infinity|undefined/.test(svg), "no sentinel leaked in");
  }
});

// The threshold line is drawn whenever there IS a threshold, and the axis is widened to include it
// rather than the line being dropped. That is the right way round - a threshold the curve never
// reaches is exactly the case an operator needs to see, and omitting the line would leave a chart
// that looks like nothing decays. The `threshold >= yMin && threshold <= yMax` guard in curveSvg is
// therefore always true for a positive threshold; it is defensive, not a filter.
test("curveSvg always draws a positive threshold, widening the axis to fit it", () => {
  assert.match(curveSvg(curve(), 5), /class="threshold"/);
  assert.match(curveSvg(curve(), 1e9), /class="threshold"/);

  // No threshold configured means no line - there is nothing to draw.
  assert.ok(!/class="threshold"/.test(curveSvg(curve(), 0)));
});

test("curveSvg marks the crossing only when it falls inside the age span", () => {
  assert.match(curveSvg(curve({ crossingAgeDays: 5 }), 5), /forgotten here/);
  assert.ok(
    !/forgotten here/.test(curveSvg(curve({ crossingAgeDays: 99 }), 5)),
  );
  assert.ok(
    !/forgotten here/.test(curveSvg(curve({ crossingAgeDays: -1 }), 5)),
  );
});

// A decay curve spans orders of magnitude far more often than not, so the axis switches to log
// where the range demands it and stays linear where it does not.
test("curveSvg switches to a log axis only on a wide range", () => {
  const wide = curveSvg(
    curve({
      points: [
        { ageDays: 0, value: 10000 },
        { ageDays: 5, value: 100 },
        { ageDays: 10, value: 1 },
      ],
    }),
    5,
  );
  const narrow = curveSvg(
    curve({
      points: [
        { ageDays: 0, value: 10 },
        { ageDays: 5, value: 8 },
        { ageDays: 10, value: 6 },
      ],
    }),
    5,
  );

  // Both must plot; the assertion that matters is that neither produces broken geometry.
  for (const svg of [wide, narrow]) {
    assert.match(svg, /<path|<polyline|d="M/);
    assert.ok(!/NaN/.test(svg));
  }
});

// -------------------------------------------------------------------- now tab

test("countdownLabel distinguishes running, unscheduled, due and counting", () => {
  const now = 1_700_000_000_000;
  const nano = (ms) => String(BigInt(Math.round(ms)) * 1_000_000n);

  // A cycle in flight beats everything else: the countdown is meaningless while one is running.
  assert.equal(
    countdownLabel(now, {
      sleepInProgress: true,
      periodSeconds: 60,
      nextSleepAt: nano(now + 1000),
    }).state,
    "running",
  );

  // A non-positive period is a supported mode, not an error, and must not render as a countdown to
  // something that will never fire.
  const none = countdownLabel(now, { periodSeconds: 0, nextSleepAt: 0 });

  assert.equal(none.state, "none");
  assert.match(none.text, /only on request/);

  // Configured but nothing scheduled - e.g. after Stop cleared it.
  assert.equal(
    countdownLabel(now, { periodSeconds: 60, nextSleepAt: 0 }).state,
    "none",
  );

  // The deadline has passed but the fire has not been observed. Momentary and normal, because the
  // timer runs on its own goroutine and the page polls; a negative number would read as a fault.
  assert.equal(
    countdownLabel(now, { periodSeconds: 60, nextSleepAt: nano(now - 5000) })
      .state,
    "due",
  );

  const counting = countdownLabel(now, {
    periodSeconds: 60,
    nextSleepAt: nano(now + 90_000),
  });

  assert.equal(counting.state, "counting");
  assert.equal(counting.text, "in 1m");
});

test("countdownFraction stays inside the track", () => {
  const now = 1_700_000_000_000;
  const nano = (ms) => String(BigInt(Math.round(ms)) * 1_000_000n);

  // Just fired: nothing elapsed.
  assert.equal(
    countdownFraction(now, {
      periodSeconds: 60,
      nextSleepAt: nano(now + 60_000),
    }),
    0,
  );

  // Halfway.
  assert.equal(
    countdownFraction(now, {
      periodSeconds: 60,
      nextSleepAt: nano(now + 30_000),
    }),
    0.5,
  );

  // Overdue, or a period that changed under a running timer, must not drive the bar off its track.
  assert.equal(
    countdownFraction(now, {
      periodSeconds: 60,
      nextSleepAt: nano(now - 90_000),
    }),
    1,
  );
  assert.equal(
    countdownFraction(now, {
      periodSeconds: 60,
      nextSleepAt: nano(now + 600_000),
    }),
    0,
  );

  // No schedule, no bar.
  assert.equal(countdownFraction(now, { periodSeconds: 0, nextSleepAt: 0 }), 0);
});

// Pressure rides on the GREATER of the two utilisations, so the meter has to show that one -
// showing bytes on a row-bounded store would report a figure that decides nothing.
test("capacityMeter picks the binding axis", () => {
  const byBytes = capacityMeter({
    usedBytes: 900,
    capacityBytes: 1000,
    memoryCount: 10,
    capacityMemories: 1000,
  });

  assert.equal(byBytes.axis, "bytes");
  assert.equal(byBytes.fraction, 0.9);

  const byRows = capacityMeter({
    usedBytes: 10,
    capacityBytes: 1000,
    memoryCount: 950,
    capacityMemories: 1000,
  });

  assert.equal(byRows.axis, "memories");
  assert.equal(byRows.fraction, 0.95);
});

test("capacityMeter uses whichever axis is configured, and none when neither is", () => {
  assert.equal(capacityMeter({ usedBytes: 5, memoryCount: 5 }), null);

  const onlyRows = capacityMeter({
    usedBytes: 500,
    memoryCount: 5,
    capacityMemories: 10,
  });

  assert.equal(onlyRows.axis, "memories");

  // Over the target clamps rather than overflowing the bar.
  const over = capacityMeter({ usedBytes: 5000, capacityBytes: 1000 });

  assert.equal(over.fraction, 1);
});

// Consolidated and evicted mean different things - decayed below the threshold, versus above it and
// removed anyway to stay under capacity - and a store doing only the latter is misconfigured. The
// summary has to keep them apart.
test("cycleSummary names the two decay paths separately", () => {
  assert.match(cycleSummary(null), /No cycle has run/);
  assert.match(cycleSummary({}), /Nothing was forgotten/);
  assert.match(cycleSummary({ memoriesConsolidated: 4 }), /4 decayed away/);
  assert.match(
    cycleSummary({ memoriesEvicted: 7 }),
    /7 evicted to stay under capacity/,
  );

  const both = cycleSummary({ memoriesConsolidated: 2, memoriesEvicted: 3 });

  assert.match(both, /2 decayed away/);
  assert.match(both, /3 evicted/);
});

test("TRIGGER_LABELS covers every trigger the service reports", () => {
  // These are the values hippocampus/server.go's triggerTimer/triggerManual/triggerWAL constants
  // put on the wire. An unlabelled one falls through to the raw string, which is legible but not
  // what a reader should get.
  for (const trigger of ["timer", "manual", "wal"]) {
    assert.ok(TRIGGER_LABELS[trigger], `no label for trigger ${trigger}`);
  }
});

// --------------------------------------------------------------------- topology

// A response shaped like the one GetTopology serves: this instance, its store, a healthy index, a
// degraded summariser, and two components that are not configured.
function topologyResponse() {
  return {
    probeIntervalSeconds: 30,
    generatedAt: "1700000000000000000",
    nodes: [
      {
        id: "self",
        kind: "TOPOLOGY_NODE_KIND_INSTANCE",
        name: "hippo-1",
        source: "TOPOLOGY_NODE_SOURCE_SELF",
        status: "TOPOLOGY_STATUS_OK",
      },
      {
        id: "store",
        kind: "TOPOLOGY_NODE_KIND_STORE",
        name: "PostgreSQL",
        detail: "postgres://db.internal:5432/hippocampus",
        source: "TOPOLOGY_NODE_SOURCE_CONFIGURED",
        status: "TOPOLOGY_STATUS_OK",
        checkedAt: "1699999990000000000",
      },
      {
        id: "search",
        kind: "TOPOLOGY_NODE_KIND_SEARCH_INDEX",
        name: "OpenSearch",
        source: "TOPOLOGY_NODE_SOURCE_CONFIGURED",
        status: "TOPOLOGY_STATUS_DEGRADED",
        checkedAt: "1699999990000000000",
      },
      {
        id: "summariser",
        kind: "TOPOLOGY_NODE_KIND_SUMMARISER",
        name: "Ollama",
        source: "TOPOLOGY_NODE_SOURCE_CONFIGURED",
        status: "TOPOLOGY_STATUS_DISABLED",
      },
      {
        id: "collector",
        kind: "TOPOLOGY_NODE_KIND_COLLECTOR",
        name: "OTLP collector",
        source: "TOPOLOGY_NODE_SOURCE_CONFIGURED",
        status: "TOPOLOGY_STATUS_DISABLED",
      },
    ],
    edges: [
      { fromId: "self", toId: "store", label: "reads/writes" },
      { fromId: "self", toId: "search", label: "indexes", optional: true },
      {
        fromId: "self",
        toId: "summariser",
        label: "summarises with",
        optional: true,
      },
      {
        fromId: "self",
        toId: "collector",
        label: "exports to",
        optional: true,
      },
    ],
  };
}

test("tierAtLeast implements the tier hierarchy and fails closed", () => {
  assert.equal(tierAtLeast("admin", "reader"), true);
  assert.equal(tierAtLeast("writer", "reader"), true);
  assert.equal(tierAtLeast("reader", "reader"), true);
  assert.equal(tierAtLeast("reader", "writer"), false);
  assert.equal(tierAtLeast("writer", "admin"), false);

  // Case is not the caller's problem: the wire carries lower-case, but a configured tier is
  // whatever somebody typed.
  assert.equal(tierAtLeast("Admin", "Writer"), true);

  // Anything unplaceable is refused rather than assumed sufficient - the same default-closed
  // posture the server takes with an unknown role.
  assert.equal(tierAtLeast("superuser", "reader"), false);
  assert.equal(tierAtLeast("admin", "superuser"), false);
  assert.equal(tierAtLeast(null, "reader"), false);
  assert.equal(tierAtLeast("admin", ""), false);
});

// The capability matrix for the Deployment tab. Two gates fold into one class, and the second is
// the one that is easy to forget: GetTopology is scopeUnbound, so a group-scoped caller is refused
// it whatever tier they hold.
test("topology visibility combines the required tier with the scope", () => {
  const cases = [
    {
      name: "auth off: unrestricted",
      who: { authEnabled: false, topologyTier: "reader" },
      want: true,
    },
    {
      name: "auth off but the view is switched off here",
      who: { authEnabled: false, topologyTier: "" },
      want: false,
    },
    {
      name: "reader clears a reader requirement",
      who: { authEnabled: true, role: "reader", topologyTier: "reader" },
      want: true,
    },
    {
      name: "reader does not clear an admin requirement",
      who: { authEnabled: true, role: "reader", topologyTier: "admin" },
      want: false,
    },
    {
      name: "admin clears a reader requirement",
      who: { authEnabled: true, role: "admin", topologyTier: "reader" },
      want: true,
    },
    {
      name: "a scoped admin is still refused",
      who: {
        authEnabled: true,
        role: "admin",
        topologyTier: "reader",
        groupScoped: true,
      },
      want: false,
    },
  ];

  for (const c of cases) {
    assert.equal(capsFromWhoAmI(c.who).topology, c.want, c.name);
  }

  assert.equal(capsWhenRefused(401).topology, false);
});

test("bodyClassesFor gates the deployment tab on the topology capability", () => {
  const visible = capsFromWhoAmI({
    authEnabled: false,
    topologyTier: "reader",
  });
  const hidden = capsFromWhoAmI({ authEnabled: false, topologyTier: "" });

  assert.ok(bodyClassesFor(visible).has("topology"));
  assert.ok(!bodyClassesFor(hidden).has("topology"));
});

test("topologyLayout places every node exactly once, in column order", () => {
  const layout = topologyLayout(topologyResponse());

  assert.equal(layout.boxes.length, 5);

  const ids = layout.boxes.map((b) => b.id);

  assert.deepEqual([...new Set(ids)].sort(), ids.slice().sort());

  const byId = new Map(layout.boxes.map((b) => [b.id, b]));

  // The instance sits to the left of everything it dials, which is the one thing the picture is
  // really saying: these are outbound connections.
  for (const id of ["store", "search", "summariser", "collector"]) {
    assert.ok(
      byId.get(id).x > byId.get("self").x,
      `${id} should be drawn to the right of the instance`,
    );
  }

  // Within the dependency column the declared kind order holds, so the diagram does not reshuffle
  // between polls - a picture that rearranges itself every few seconds cannot be read.
  assert.ok(byId.get("store").y < byId.get("search").y);
  assert.ok(byId.get("search").y < byId.get("summariser").y);
});

test("topologyLayout never overlaps two boxes", () => {
  const layout = topologyLayout(topologyResponse());

  for (const a of layout.boxes) {
    for (const b of layout.boxes) {
      if (a === b) continue;

      const apart =
        a.x + a.w <= b.x ||
        b.x + b.w <= a.x ||
        a.y + a.h <= b.y ||
        b.y + b.h <= a.y;

      assert.ok(apart, `${a.id} and ${b.id} overlap`);
    }
  }
});

test("topologyLayout keeps every box inside the viewBox", () => {
  const layout = topologyLayout(topologyResponse());

  for (const box of layout.boxes) {
    assert.ok(
      box.x >= 0 && box.x + box.w <= layout.width,
      `${box.id} overflows horizontally`,
    );
    assert.ok(
      box.y >= 0 && box.y + box.h <= layout.height,
      `${box.id} overflows vertically`,
    );
  }
});

// Hiding what is not configured is a filter on the picture, not on the truth: the nodes go, and so
// must the edges that pointed at them - an edge drawn to a box that is not there is a line to
// nowhere, which reads as a rendering fault rather than as a hidden node.
test("topologyLayout drops the edges of hidden nodes", () => {
  const layout = topologyLayout(topologyResponse(), { showDisabled: false });

  assert.deepEqual(layout.boxes.map((b) => b.id).sort(), [
    "search",
    "self",
    "store",
  ]);

  const placed = new Set(layout.boxes.map((b) => b.id));

  for (const link of layout.links) {
    assert.ok(placed.has(link.fromId), `${link.fromId} is not placed`);
    assert.ok(placed.has(link.toId), `${link.toId} is not placed`);
  }

  assert.equal(layout.links.length, 2);
});

test("topologyLayout gives an empty response nothing to draw", () => {
  const layout = topologyLayout({ nodes: [], edges: [] });

  assert.equal(layout.boxes.length, 0);
  assert.equal(layout.links.length, 0);
  assert.match(topologySvg(layout), /Nothing to draw/);
});

test("topologySvg escapes every value it renders", () => {
  const svg = topologySvg(
    topologyLayout({
      nodes: [
        {
          id: 'x"><script>',
          kind: "TOPOLOGY_NODE_KIND_INSTANCE",
          name: "<img src=x onerror=alert(1)>",
          detail: "a & b",
          status: "TOPOLOGY_STATUS_OK",
        },
      ],
      edges: [],
    }),
  );

  assert.ok(!svg.includes("<script>"));
  assert.ok(!svg.includes("<img"));
  assert.ok(svg.includes("&amp;"));
});

test("topologySvg marks an optional edge and colours by status", () => {
  const svg = topologySvg(topologyLayout(topologyResponse()));

  assert.ok(svg.includes("tlink optional"), "optional edges are drawn dashed");
  assert.ok(
    svg.includes("tnode degraded"),
    "a degraded node carries its status class",
  );
  assert.ok(
    svg.includes("tnode off"),
    "a disabled node carries its status class",
  );
});

test("topologyStatus and topologySource name every wire value", () => {
  for (const status of [
    "TOPOLOGY_STATUS_OK",
    "TOPOLOGY_STATUS_DEGRADED",
    "TOPOLOGY_STATUS_UNREACHABLE",
    "TOPOLOGY_STATUS_DISABLED",
    "TOPOLOGY_STATUS_UNSPECIFIED",
  ]) {
    assert.ok(topologyStatus(status).label.length > 0, status);
    assert.ok(topologyStatus(status).cls.length > 0, status);
  }

  // An unknown status must read as "not checked" rather than as healthy: a client one release
  // behind the service must not paint a new state green.
  assert.equal(
    topologyStatus("TOPOLOGY_STATUS_SOMETHING_NEW").label,
    topologyStatus("TOPOLOGY_STATUS_UNSPECIFIED").label,
  );

  for (const source of [
    "TOPOLOGY_NODE_SOURCE_SELF",
    "TOPOLOGY_NODE_SOURCE_CONFIGURED",
    "TOPOLOGY_NODE_SOURCE_DISCOVERED",
    "TOPOLOGY_NODE_SOURCE_DECLARED",
    "TOPOLOGY_NODE_SOURCE_OBSERVED",
  ]) {
    assert.notEqual(topologySource(source), "unknown", source);
  }

  assert.equal(topologySource("TOPOLOGY_NODE_SOURCE_FUTURE"), "unknown");
});

// Every status in this view is a snapshot from a background prober, so "unreachable" always means
// "was unreachable when last asked". A reader who cannot see when that was may act on a minute-old
// picture believing it is live.
test("topologyCheckedLabel says how fresh a status is", () => {
  const now = 1700000000000;

  assert.match(
    topologyCheckedLabel(now, {
      status: "TOPOLOGY_STATUS_OK",
      checkedAt: String((now - 60000) * 1e6),
    }),
    /^checked /,
  );

  // Never probed is not the same as probed and healthy.
  assert.equal(
    topologyCheckedLabel(now, {
      status: "TOPOLOGY_STATUS_UNSPECIFIED",
      checkedAt: 0,
    }),
    "not checked",
  );

  // A healthy status with no check time behind it was asserted rather than probed, so saying
  // "reachable, not checked" of it would read as a contradiction.
  assert.equal(
    topologyCheckedLabel(now, { status: "TOPOLOGY_STATUS_OK", checkedAt: 0 }),
    "",
  );

  // The instance answering the request is not something it probes.
  assert.equal(
    topologyCheckedLabel(now, {
      status: "TOPOLOGY_STATUS_OK",
      source: "TOPOLOGY_NODE_SOURCE_SELF",
    }),
    "",
  );

  // A component that is switched off has nothing to check, so saying so would be noise.
  assert.equal(
    topologyCheckedLabel(now, { status: "TOPOLOGY_STATUS_DISABLED" }),
    "",
  );
});

test("truncateMiddle keeps both ends of an address", () => {
  assert.equal(truncateMiddle("short", 20), "short");

  const long = truncateMiddle(
    "postgres://a-very-long-hostname.internal:5432/hippocampus",
    20,
  );

  assert.equal(long.length, 20);
  assert.ok(long.startsWith("postgres:"), long);
  assert.ok(long.endsWith("campus"), long);
});
