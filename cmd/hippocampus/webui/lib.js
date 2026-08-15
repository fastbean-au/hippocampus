// lib.js — the console's pure logic: everything that computes a value or renders a string without
// reading the DOM, the network, or module state.
//
// It exists to be TESTED. The console is ~2,800 lines of JavaScript that no Go test can reach, and
// the bugs it has actually shipped (items 72 and 73 in TODO.md are both lists of them) were found by
// eye, one screenshot at a time. The functions here are the ones where being wrong is silent — an
// age that rounds the wrong way, a metadata line split on the wrong "=", a capability matrix that
// hides a control the caller is entitled to — so they are the ones worth pinning. app.js keeps
// everything that touches the page; ../webuitest exercises this file directly under node, with no
// DOM, no browser and no dependencies.
//
// Nothing here may import from app.js: the dependency runs one way, which is what keeps this file
// testable in isolation.

export function esc(s) {
  return String(s == null ? "" : s).replace(
    /[&<>"']/g,
    (c) =>
      ({
        "&": "&amp;",
        "<": "&lt;",
        ">": "&gt;",
        '"': "&quot;",
        "'": "&#39;",
      })[c],
  );
}

const idHead = 16;
const idTail = 12;

export function shortId(id) {
  const s = String(id == null ? "" : id);

  if (s.length <= idHead + idTail + 1) return s;

  return s.slice(0, idHead) + "…" + s.slice(-idTail);
}

export function nanoToText(nanoStr) {
  if (!nanoStr || nanoStr === "0") return "—";
  const ms = Number(BigInt(nanoStr) / 1000000n);

  return new Date(ms).toLocaleString();
}

// compactAge turns a count of seconds into the largest unit leaving a whole number. Coarse on
// purpose: this is a column to be scanned down, and the exact value is in the tooltip.
export function compactAge(seconds) {
  const units = [
    [31536000, "y"],
    [2592000, "mo"],
    [604800, "w"],
    [86400, "d"],
    [3600, "h"],
    [60, "m"],
  ];

  for (const [size, suffix] of units) {
    if (seconds >= size) return Math.floor(seconds / size) + suffix;
  }

  return seconds + "s";
}

// b64url base64url-encodes bytes without padding, as PKCE and JWT expect.
export function b64url(bytes) {
  return btoa(String.fromCharCode(...new Uint8Array(bytes)))
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "");
}

// randomString returns a base64url token of n random bytes, for the PKCE verifier and the state.
export function randomString(n) {
  const a = new Uint8Array(n);
  crypto.getRandomValues(a);

  return b64url(a);
}

// ------------------------------------------------------------------ memories
// metadataPills renders a memory's or event's labels as sorted key=value pills. Sorted because
// object key order is insertion order over the wire, so two memories carrying the same labels
// would otherwise display them differently depending on how each was written.
export function metadataPills(metadata) {
  if (!metadata) return "";

  return Object.keys(metadata)
    .sort()
    .map((k) => `<span class="pill meta">${esc(k)}=${esc(metadata[k])}</span>`)
    .join("");
}

// groupCell combines the group label and the metadata pills, which are one classification
// between them. A dash only when there is neither.
export function groupCell(m) {
  const pills = metadataPills(m.metadata);

  if (!m.group && !pills) return "—";

  return (
    (m.group ? esc(m.group) : "") +
    (pills ? (m.group ? "<br>" : "") + pills : "")
  );
}

// parseMetadataPairs splits a "key=value" list into trimmed, non-empty pairs. separator is
// "\n" for the write form (one per line) and "," for the single-line filter input. Splitting
// the VALUE is deliberately left to the server, which cuts on the first "=" so a value may
// contain one.
export function parseMetadataPairs(text, separator) {
  return (text || "")
    .split(separator)
    .map((line) => line.trim())
    .filter((line) => line.includes("=") && !line.startsWith("="));
}

// metadataToForm renders stored labels back into the textarea, sorted so editing a memory
// twice shows them in the same order both times.
export function metadataToForm(metadata) {
  if (!metadata) return "";

  return Object.keys(metadata)
    .sort()
    .map((k) => k + "=" + metadata[k])
    .join("\n");
}

// num formats a computed value compactly. A memory younger than one age unit values as the largest
// float there is - the service's way of saying "not yet decaying at all" - which is a symbol, not a
// number to print.
export function num(value) {
  if (value === undefined || value === null || !isFinite(value)) return "∞";

  if (Math.abs(value) >= 1e15) return "∞";

  if (value === 0) return "0";

  return Number(value.toPrecision(4)).toString();
}

// humanDays renders the projection, including its two sentinels: 0 is due now, and -1 is "no
// crossing within any span worth reporting" rather than an unknown.
export function humanDays(days) {
  if (days === undefined || days === null || !isFinite(days)) return "—";

  if (days < 0) return "not foreseeably";

  if (days === 0) return "due now";

  if (days < 1 / 24) return "< 1 hour";

  if (days < 1) return (days * 24).toPrecision(2) + " hours";

  if (days < 365) return Number(days.toPrecision(3)) + " days";

  return Number((days / 365).toPrecision(2)) + " years";
}

export function formatBytes(value) {
  // Absent and unknown are different, and the difference matters: an unset proto int64 arrives as
  // undefined and really does mean zero bytes, but a value that is present and not a number is
  // unknown, and rendering it as "0 B" would state a fact about the store rather than admit a
  // missing figure. The guard below was previously unreachable because `value || 0` folded NaN into
  // 0 before it ran.
  const n = value === undefined || value === null ? 0 : Number(value);

  if (!isFinite(n)) return "—";

  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let scaled = n;
  let unit = 0;

  while (scaled >= 1024 && unit < units.length - 1) {
    scaled /= 1024;
    unit++;
  }

  return (
    (unit === 0 ? scaled : Number(scaled.toPrecision(3))) + " " + units[unit]
  );
}

// curveSvg draws the sampled curve, the threshold it is measured against, and where the two meet.
// Every value in the markup is a number formatted here, never server text, so the SVG cannot carry
// anything but geometry.
export function curveSvg(curve, threshold) {
  const points = (curve.points || [])
    .map((p) => ({ x: Number(p.ageDays), y: Number(p.value) }))
    .filter((p) => isFinite(p.x) && isFinite(p.y) && p.y > 0);

  if (points.length < 2)
    return '<div class="empty">Not enough of the curve to plot.</div>';

  const W = 660,
    H = 240,
    L = 62,
    R = 14,
    T = 14,
    B = 30;
  const xMax = Number(curve.maxAgeDays) || points[points.length - 1].x;

  let yMin = Math.min(...points.map((p) => p.y));
  let yMax = Math.max(...points.map((p) => p.y));

  if (threshold > 0) {
    yMin = Math.min(yMin, threshold);
    yMax = Math.max(yMax, threshold);
  }

  // A decay curve spans orders of magnitude far more often than not (the first sample of a power
  // law is many times the threshold), and a linear axis spends all its height on that first spike.
  // Only switch to log where the range actually demands it, so the common gentle curve still reads
  // as the shape it is.
  const log = yMax / yMin > 50;
  const ly = (v) => (log ? Math.log10(v) : v);
  const yLo = ly(yMin),
    yHi = ly(yMax);
  const span = yHi - yLo || 1;

  const px = (x) => L + (xMax > 0 ? x / xMax : 0) * (W - L - R);
  const py = (y) => T + (1 - (ly(y) - yLo) / span) * (H - T - B);

  const path = points
    .map(
      (p, i) => (i ? "L" : "M") + px(p.x).toFixed(1) + " " + py(p.y).toFixed(1),
    )
    .join(" ");

  let marks = "";

  if (threshold > 0 && threshold >= yMin && threshold <= yMax) {
    const y = py(threshold).toFixed(1);

    marks += `<line class="threshold" x1="${L}" y1="${y}" x2="${W - R}" y2="${y}"></line>
      <text x="${W - R}" y="${Number(y) - 5}" text-anchor="end">threshold ${esc(num(threshold))}</text>`;
  }

  const crossing = Number(curve.crossingAgeDays);

  if (crossing >= 0 && crossing <= xMax) {
    const x = px(crossing).toFixed(1);

    marks += `<line class="crossing" x1="${x}" y1="${T}" x2="${x}" y2="${H - B}"></line>
      <text x="${x}" y="${T + 10}" text-anchor="middle">forgotten here</text>`;
  }

  // Four x labels and the two y extremes: enough to read the scale without turning the plot into a
  // table.
  let axes = `<line class="axis" x1="${L}" y1="${H - B}" x2="${W - R}" y2="${H - B}"></line>
    <line class="axis" x1="${L}" y1="${T}" x2="${L}" y2="${H - B}"></line>
    <text x="${L - 6}" y="${T + 10}" text-anchor="end">${esc(num(yMax))}</text>
    <text x="${L - 6}" y="${H - B}" text-anchor="end">${esc(num(yMin))}</text>`;

  for (let i = 0; i <= 4; i++) {
    const x = px((xMax / 4) * i);

    axes += `<line class="grid" x1="${x.toFixed(1)}" y1="${T}" x2="${x.toFixed(1)}" y2="${H - B}"></line>
      <text x="${x.toFixed(1)}" y="${H - B + 16}" text-anchor="middle">${esc(num((xMax / 4) * i))}d</text>`;
  }

  return `<svg viewBox="0 0 ${W} ${H}" role="img" aria-label="decay curve">
    ${axes}${marks}
    <path class="curve" d="${path}"></path>
    <text x="${L}" y="${H - 2}">age in days${log ? " · value on a log scale" : ""}</text>
  </svg>`;
}

// ---------------------------------------------------------------- capabilities

// capsFromWhoAmI turns a WhoAmI response into the capability object the console gates on. Pure, and
// separated from the fetch that produces it, because this mapping is where "show a control the
// caller cannot use" and its opposite both live - and both are invisible until someone signs in
// with the exact token that trips them.
//
// The two admin capabilities are deliberately different questions, and conflating them was a bug:
//
//   isAdmin        - the caller's TIER is admin. What it gates is admin-tier RPCs that are scoped
//                    like any other listing.
//   isUnboundAdmin - admin tier AND no group scope. What it gates is the RPCs that act on the whole
//                    store and are therefore refused outright to a scoped caller: Purge, Sleep and
//                    PreviewConsolidation (hippocampus/scope.go's scopeUnbound set).
//
// Until these were separated, a group-scoped admin was treated as not-admin throughout, which hid
// the forgotten log from them. That is wrong: the log's RPCs are scopeFilter, not scopeUnbound
// (hippocampus/scope.go), precisely because a tombstone carries its memory's group - so a scoped
// admin may use them and sees exactly their own partition's losses. The console was hiding a panel
// the service was willing to serve. Reading that log has since dropped to reader tier and so is
// gated on neither capability; DeleteForgottenMemories is what isAdmin gates there now.
export function capsFromWhoAmI(who) {
  const role = who.role || null;
  const authEnabled = !!who.authEnabled;
  const isAdmin = !authEnabled || role === "admin";

  return {
    role,
    authEnabled,
    canWrite: !authEnabled || role === "writer" || role === "admin",
    isAdmin,
    isUnboundAdmin: isAdmin && !who.groupScoped,
    // Properties of the SERVER, not the token: which search modes it can serve, whether an
    // embedded LLM is configured, whether it consolidates at all, and whether it records what it
    // forgets. Reported so a client adapts rather than discovering absence through a refusal.
    searchModes: who.searchModes || [],
    summariser: !!who.summariserEnabled,
    consolidation: !!who.consolidationEnabled,
    tombstones: !!who.tombstonesEnabled,
    // groupScoped is authoritative; groups alone cannot distinguish unscoped (the whole store)
    // from scoped-to-nothing, and the two are opposites.
    groups: who.groups || [],
    groupScoped: !!who.groupScoped,
    status: 200,
  };
}

// capsWhenRefused is the capability set for a caller whose WhoAmI did not succeed. Everything is
// closed: a console that cannot establish what it may do must offer nothing, since every control it
// showed would be a refusal waiting to happen.
export function capsWhenRefused(status) {
  return {
    role: null,
    authEnabled: true,
    canWrite: false,
    isAdmin: false,
    isUnboundAdmin: false,
    searchModes: [],
    summariser: false,
    consolidation: false,
    tombstones: false,
    groups: [],
    groupScoped: false,
    status: status || 0,
  };
}

// GATING_CLASSES is every class bodyClassesFor can put on <body>. applyCaps iterates it rather than
// the returned set, because a class must be REMOVED when it is no longer warranted and a set of
// what is wanted cannot say what is not; toggling only what was returned would leave a stale
// "admin" on the body after signing in as a reader.
export const GATING_CLASSES = [
  "readonly",
  "admin",
  "unbound-admin",
  "summariser",
  "consolidating",
  "tombstones",
];

// bodyClassesFor is the whole of the console's role gating: the set of classes on <body> from which
// CSS hides everything the caller cannot use. Returning a Set rather than mutating the element is
// what makes the matrix - seven or eight deployment and token shapes - testable as a table.
//
// "unbound-admin" is separate from "admin" for the reason capsFromWhoAmI gives.
export function bodyClassesFor(caps) {
  const classes = new Set();

  if (!caps.canWrite) classes.add("readonly");
  if (caps.isAdmin) classes.add("admin");
  if (caps.isUnboundAdmin) classes.add("unbound-admin");
  if (caps.summariser) classes.add("summariser");
  if (caps.consolidation) classes.add("consolidating");
  if (caps.tombstones) classes.add("tombstones");

  return classes;
}

// ------------------------------------------------------------------------ gate

// gateNeeded reports whether the login card should stand in front of the console: this deployment
// authenticates, no role resolved, and the reason is credential-shaped - nothing held at all, or a
// call the server refused as unauthenticated (401) or unauthorised (403). A whoami that failed for
// any other reason (a purge returns 503) deliberately leaves the console up: inviting an operator
// to replace a good token would not fix anything.
//
// authEnabled and haveCredential are parameters rather than reads of module state so the whole
// matrix can be tested. Both directions of this decision are bad in their own way - a console stuck
// behind a login card a working session should have lifted, or a console standing open whose every
// call is about to be refused - and neither shows up except with the exact token that causes it.
export function gateNeeded(authEnabled, caps, haveCredential) {
  if (!authEnabled || caps.role) {
    return false;
  }

  return !haveCredential || caps.status === 401 || caps.status === 403;
}

// gateMessage explains a refusal, and only a refusal: an operator who has just signed out, or who
// has never signed in, is shown the plain card with no error on it. A 403 is called out separately
// from a 401 because the remedy differs - the token is fine, the role it carries is not.
export function gateMessage(caps, mode) {
  if (caps.status === 403) {
    return "That sign-in is valid, but its role grants no access to this console.";
  }

  // In server-login mode the cookie is unreadable, so "held a credential" cannot be told apart from
  // "just signed out" - the card simply offers Sign in rather than claiming an expiry.
  if (caps.status === 401 && !mode.serverLogin && mode.haveCredential) {
    return mode.oidcLogin
      ? "Your session has ended. Sign in again to continue."
      : "That token was not accepted. Check it, or mint a new one.";
  }

  return "";
}

// ------------------------------------------------------------------------- age

// ageLabel is the pure core of ageCell: the compact age of a UnixNano timestamp relative to a
// given "now", including the "in " prefix for a timestamp ahead of it. now is a parameter rather
// than Date.now() so the boundaries can be tested at all - every one of them is a function of the
// difference, and a test that read the clock could only ever check one point on the curve.
//
// A timestamp ahead of now is not a bug to hide: StoreMemory accepts a small skew window and an
// event's time_end is routinely in the future. Saying "in 3m" is honest; showing "0s" is not.
export function ageLabel(nowMs, nanoStr) {
  const ms = Number(BigInt(nanoStr) / 1000000n);
  const seconds = Math.round((nowMs - ms) / 1000);

  return (seconds < 0 ? "in " : "") + compactAge(Math.abs(seconds));
}

// ------------------------------------------------------------------------- now

// countdownLabel turns the next scheduled cycle into what the Now tab shows. It is the whole answer
// to "is this thing doing anything", and it has four distinct states rather than one number,
// because a missing countdown means four different things and only one of them is a wait:
//
//   running     - a cycle is in flight right now
//   no schedule - sleep.periodSeconds <= 0, a supported mode for an instance driven only by the
//                 Sleep RPC or the WAL trigger. Not an error, and not a countdown to anything.
//   due         - the deadline has passed but the cycle has not been observed starting yet. The
//                 timer fires on its own goroutine and this page polls, so this is normal and
//                 momentary; showing a negative number would read as a fault.
//   a countdown - the ordinary case.
//
// nowMs is a parameter so every boundary is testable; the page passes Date.now().
export function countdownLabel(nowMs, status) {
  if (status.sleepInProgress) {
    return { state: "running", text: "running now…" };
  }

  const period = Number(status.periodSeconds || 0);

  if (period <= 0) {
    return {
      state: "none",
      text: "no timed cycle — this instance sleeps only on request",
    };
  }

  const next = Number(status.nextSleepAt || 0);

  if (next <= 0) {
    return { state: "none", text: "no cycle scheduled" };
  }

  const seconds = Math.round((next - nowMs * 1e6) / 1e9);

  if (seconds <= 0) {
    return { state: "due", text: "due now" };
  }

  return { state: "counting", text: "in " + compactAge(seconds), seconds };
}

// countdownFraction is how far through the current interval we are, for the progress bar: 0 just
// after a cycle, approaching 1 as the next is due. Clamped, because a period that changed under a
// running timer (or a clock that stepped) would otherwise drive the bar off the end of its track.
export function countdownFraction(nowMs, status) {
  const period = Number(status.periodSeconds || 0);
  const next = Number(status.nextSleepAt || 0);

  if (period <= 0 || next <= 0) return 0;

  const remaining = (next - nowMs * 1e6) / 1e9;
  const elapsed = period - remaining;

  return Math.max(0, Math.min(1, elapsed / period));
}

// capacityMeter reduces the two capacity axes to the one that is actually binding.
//
// Pressure rides on the GREATER of byte utilisation and row utilisation, so reporting both equally
// would bury the number that decides anything - and reporting only bytes would be silently wrong on
// a store bounded by row count. This picks the leading axis and says which it is, which is also the
// honest answer when only one of the two is configured. Returns null when neither is: an unbounded
// store has no meter to draw, and drawing an empty one would imply a limit it does not have.
export function capacityMeter(explain) {
  const axes = [];
  const usedBytes = Number(explain.usedBytes || 0);
  const capacityBytes = Number(explain.capacityBytes || 0);
  const memoryCount = Number(explain.memoryCount || 0);
  const capacityMemories = Number(explain.capacityMemories || 0);

  if (capacityBytes > 0) {
    axes.push({
      axis: "bytes",
      fraction: usedBytes / capacityBytes,
      used: formatBytes(usedBytes),
      limit: formatBytes(capacityBytes),
    });
  }

  if (capacityMemories > 0) {
    axes.push({
      axis: "memories",
      fraction: memoryCount / capacityMemories,
      used: memoryCount.toLocaleString(),
      limit: capacityMemories.toLocaleString(),
    });
  }

  if (!axes.length) return null;

  const leading = axes.reduce((a, b) => (b.fraction > a.fraction ? b : a));

  return { ...leading, fraction: Math.max(0, Math.min(1, leading.fraction)) };
}

// cycleSummary is the one-line reading of what the last cycle did. The two decay paths are named
// separately because they mean different things: consolidated fell below the value threshold,
// evicted was above it and went anyway to bring the store under its capacity target. A store
// evicting steadily while consolidating nothing is configured wrongly, and only the split shows it.
export function cycleSummary(cycle) {
  if (!cycle) return "No cycle has run since this instance started.";

  const parts = [];
  const consolidated = Number(cycle.memoriesConsolidated || 0);
  const evicted = Number(cycle.memoriesEvicted || 0);

  if (consolidated) parts.push(`${consolidated.toLocaleString()} decayed away`);
  if (evicted)
    parts.push(`${evicted.toLocaleString()} evicted to stay under capacity`);

  if (!parts.length) return "Nothing was forgotten.";

  return parts.join(", ") + ".";
}

// TRIGGER_LABELS spell out what started a cycle. The raw values are wire enums; these are what a
// person reads.
export const TRIGGER_LABELS = {
  timer: "on schedule",
  manual: "run by hand",
  wal: "triggered by the write-ahead log",
};
