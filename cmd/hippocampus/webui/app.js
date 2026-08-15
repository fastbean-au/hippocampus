"use strict";

// The console's pure logic lives in lib.js, which is what ../webuitest exercises under node.
// This file is everything else: the DOM, the network, and the state that ties them together.
import {
  GATING_CLASSES,
  TRIGGER_LABELS,
  ageLabel,
  b64url,
  bodyClassesFor,
  capacityMeter,
  capsFromWhoAmI,
  capsWhenRefused,
  compactAge,
  countdownFraction,
  countdownLabel,
  curveSvg,
  cycleSummary,
  esc,
  formatBytes,
  gateMessage,
  gateNeeded,
  groupCell,
  humanDays,
  metadataToForm,
  nanoToText,
  num,
  parseMetadataPairs,
  randomString,
  shortId,
} from "./lib.js";

// ------------------------------------------------------------------ helpers
const $ = (id) => document.getElementById(id);
const TOKEN_KEY = "hippocampus_token";

// oidc holds the browser login state used only when /ui/config reports auth.method "idp" with a UI
// client. In every other mode it stays disabled and the login gate's token box drives auth instead.
// The access token lives in sessionStorage (cleared when the tab closes) rather than localStorage,
// and no client secret is ever involved - this is an Authorization Code + PKCE public client.
const OIDC_ACCESS_KEY = "hippocampus_oidc_access";
const OIDC_REFRESH_KEY = "hippocampus_oidc_refresh";
const OIDC_EXPIRY_KEY = "hippocampus_oidc_expiry";
const PKCE_KEY = "hippocampus_pkce"; // {verifier, state}, kept across the provider redirect

const oidc = {
  enabled: false,
  cfg: null, // {issuer, clientId, scopes, audience} from /ui/config
  endpoints: null, // {authorization_endpoint, token_endpoint, end_session_endpoint} from discovery
  accessToken: sessionStorage.getItem(OIDC_ACCESS_KEY) || "",
  refreshTimer: null,
};

// authCfg records what /ui/config said about this deployment: whether it authenticates at all, and
// therefore whether the login gate is ever raised. It is deliberately the *configured* answer rather
// than "did the last call fail", so a refusal that has nothing to do with credentials — a purge, a
// network blip — never presents a sign-in card to an operator whose token is perfectly good. The one
// exception is in refreshCaps: a 401/403 promotes it, so a deployment whose /ui/config is unreachable
// still gets a way to enter a token.
const authCfg = { enabled: false, method: "none" };

// manualToken is the pasted bearer token in the non-OIDC modes. It lives in localStorage (so it
// survives a reload, as it always has) and is read from there rather than from a DOM input, because
// the input it is typed into only exists while the gate is up.
let manualToken = localStorage.getItem(TOKEN_KEY) || "";

// server holds the state for the server-side login mode (/ui/config loginMode "server"): the service
// hosts the OIDC flow at /auth/login and sets an HttpOnly session cookie, so this page never sees or
// stores a token. The gate's Sign in and the header's Sign out simply navigate to /auth/login and
// /auth/logout, and every /v1 call is authenticated by the cookie the browser sends automatically.
const server = { enabled: false };

// getToken returns the bearer sent on every /v1 call: nothing in server-login mode (the HttpOnly
// cookie carries auth and is not script-readable), the OIDC access token when the in-browser flow is
// active, otherwise the token entered on the login gate.
function getToken() {
  if (server.enabled) {
    return "";
  }

  if (oidc.enabled) {
    return oidc.accessToken || "";
  }

  return manualToken;
}

// haveCredential reports whether this page holds something to authenticate with. Under the
// server-hosted login the session is an HttpOnly cookie no script can read, so it answers true and
// leaves the verdict to what whoami returned.
function haveCredential() {
  if (server.enabled) {
    return true;
  }

  if (oidc.enabled) {
    return !!oidc.accessToken;
  }

  return !!manualToken;
}

// serverRefresh asks the service to swap the refresh cookie for a fresh access token (204 on
// success). Used to transparently recover an expired session before surfacing a 401.
async function serverRefresh() {
  try {
    const res = await fetch("/auth/refresh", { method: "POST" });

    return res.ok;
  } catch (e) {
    return false;
  }
}

// ------------------------------------------------------------------ login gate
// The gate is the console's signed-out state: it stands in place of the tabs whenever this
// deployment authenticates and no session has resolved. It is raised and lifted from applyCaps, so
// every route into a signed-out state — first load, sign-out, a refused token, a lapsed session
// surfaced by any failing call — goes through the same decision.

// The decision itself (gateNeeded) and the wording of a refusal (gateMessage) are in lib.js, where
// they can be tested against the whole matrix of deployment and credential shapes.

// setGateError shows or clears the line under the sign-in control.
function setGateError(msg) {
  const el = $("gate-error");

  el.textContent = msg || "";
  el.classList.toggle("hidden", !msg);
}

// applyGate raises or lifts the gate and fills it in for the active sign-in mode. Called from
// applyCaps on every capability refresh; when the gate is not needed it only drops the body class,
// leaving the card's contents alone since nothing renders them.
function applyGate() {
  const gated = gateNeeded(authCfg.enabled, caps, haveCredential());

  document.body.classList.toggle("gated", gated);

  if (!gated) {
    return;
  }

  const manual = !oidc.enabled && !server.enabled;

  $("gate-title").textContent = "Sign in";
  $("gate-lede").textContent = manual
    ? "This service requires a bearer token. It is kept in this browser and sent with every request the console makes."
    : "This service authenticates through your identity provider.";

  $("gate-form").classList.toggle("hidden", !manual);
  $("gate-oidc").classList.toggle("hidden", manual);
  $("gate-hint").classList.toggle("hidden", !manual);

  setGateError(
    gateMessage(caps, {
      serverLogin: server.enabled,
      oidcLogin: oidc.enabled,
      haveCredential: haveCredential(),
    }),
  );

  if (manual) {
    $("gate-token").focus();
  }
}

// signInManual takes the pasted token, keeps it for subsequent calls, and re-resolves capabilities.
// A token the server refuses leaves the gate up with the reason on it, rather than dropping the
// operator into a console where every action would fail.
async function signInManual(ev) {
  ev.preventDefault();

  const value = $("gate-token").value.trim();

  if (!value) {
    setGateError("Paste the bearer token issued for this service.");

    return;
  }

  manualToken = value;
  localStorage.setItem(TOKEN_KEY, manualToken);

  $("gate-submit").disabled = true;

  try {
    await refreshCaps();
  } finally {
    $("gate-submit").disabled = false;
  }
}

// signOut ends the current session by whichever route this deployment's login mode calls for, and
// so puts the gate back. Under the two OIDC modes it goes through the provider so the provider's
// own session ends too; in manual mode it forgets the token this browser stored.
function signOut() {
  if (server.enabled) {
    window.location.assign("/auth/logout");

    return;
  }

  if (oidc.enabled) {
    logout();

    return;
  }

  manualToken = "";
  localStorage.removeItem(TOKEN_KEY);
  $("gate-token").value = "";

  refreshCaps();
}

// caps holds what the current token is allowed to do, as reported by GET /v1/whoami. It gates the
// write-oriented controls in the UI (a convenience only - the server enforces authorization on
// every RPC regardless of what the console shows). canWrite defaults to true so the UI is usable
// before the first whoami resolves and when authentication is disabled.
let caps = {
  canWrite: true,
  isAdmin: true,
  role: null,
  authEnabled: false,
  searchModes: [],
  summariser: false,
  // These two start false rather than optimistic, unlike canWrite: they must agree with the
  // body classes the page ships without (.consolidating / .tombstones), or memoryRow's colspan
  // would be counting a Value column the stylesheet is hiding. Nothing renders before the
  // first whoami anyway — the page ships gated, so nav and main are both hidden until
  // applyCaps has run.
  consolidation: false,
  tombstones: false,
  groups: [],
  groupScoped: false,
  status: 0,
};

// refreshCaps asks the server who the current token is and applies the result. A 401 (no/invalid
// token while auth is on) or 403 (valid token, but a role that grants nothing) both mean "cannot
// write", handled without a toast since this runs on load and on every sign-in. The status of a
// failure is kept because the gate decides from it: only a credential-shaped refusal raises a
// sign-in card.
async function refreshCaps() {
  try {
    caps = capsFromWhoAmI(await api("GET", "/v1/whoami"));
  } catch (e) {
    caps = capsWhenRefused(e.status);

    // A refused call is also how the console learns that auth is on when /ui/config could not be
    // read (an old reverse proxy, a stripped route). Without this the gate would never be raised and
    // the token box it carries — now the only one on the page — would be unreachable.
    if (!authCfg.enabled && (e.status === 401 || e.status === 403)) {
      authCfg.enabled = true;
    }
  }

  applyCaps();
}

// SEARCH_MODE_LABELS names the modes for the picker, and its key order is the order they appear in.
// Hybrid is marked recommended because it usually is: semantic alone misses exact terms (error
// codes, identifiers, names) and keyword alone misses paraphrase.
const SEARCH_MODE_LABELS = {
  SEARCH_MODE_KEYWORD: "Keyword \u2014 match the words in the body",
  SEARCH_MODE_SEMANTIC: "Semantic \u2014 match by meaning",
  SEARCH_MODE_HYBRID: "Hybrid \u2014 both, fused (recommended)",
};

// applySearchModes fills the mode picker from what the server said it can serve, and hides it
// entirely when there is nothing to choose between. Offering a mode this deployment cannot serve
// would only produce a FAILED_PRECONDITION the user could do nothing about, which is why whoami
// reports the modes at all.
function applySearchModes() {
  const field = $("s-mode-field");
  const select = $("s-mode");

  if (!field || !select) return;

  const modes = (caps.searchModes || []).filter((m) => SEARCH_MODE_LABELS[m]);

  // One mode (or none reported, e.g. an older server) means no choice worth presenting.
  if (modes.length < 2) {
    field.hidden = true;
    select.innerHTML = "";

    return;
  }

  const previous = select.value;

  select.innerHTML = "";

  for (const mode of Object.keys(SEARCH_MODE_LABELS)) {
    if (!modes.includes(mode)) continue;

    const option = document.createElement("option");

    option.value = mode;
    option.textContent = SEARCH_MODE_LABELS[mode];

    select.appendChild(option);
  }

  // Default to hybrid where available - it is the best of the three - but keep whatever the user
  // had already chosen across a token change.
  if (previous && modes.includes(previous)) {
    select.value = previous;
  } else if (modes.includes("SEARCH_MODE_HYBRID")) {
    select.value = "SEARCH_MODE_HYBRID";
  }

  field.hidden = false;
}

// applyCaps toggles the read-only body class (which hides .writer-only controls), updates the role
// pill and the Sign out button in the header, and raises or lifts the login gate.
function applyCaps() {
  // The class set is computed in lib.js so the whole matrix - deployment shape x token shape - is
  // testable as a table rather than only by signing in with the exact token that trips it.
  const wanted = bodyClassesFor(caps);

  for (const name of GATING_CLASSES) {
    document.body.classList.toggle(name, wanted.has(name));
  }

  // Set before any table renders, so a replica never issues the explain call it would only be
  // refused. Only ever narrowed here: a refusal already seen is not un-learned by a re-check.
  if (!caps.consolidation) decayAvailable = false;

  applyConsolidation();
  applySearchModes();
  applyScope();
  applyGate();

  // Sign out is offered only where there is a session to end: a deployment with authentication on
  // and a role resolved for this caller.
  $("logout-btn").classList.toggle("hidden", !(caps.authEnabled && caps.role));

  const pill = $("role-pill");

  if (!caps.authEnabled) {
    pill.classList.add("hidden");

    return;
  }

  pill.classList.remove("hidden");
  pill.classList.toggle("ro", !caps.canWrite);
  pill.textContent = caps.role ? "role: " + caps.role : "not signed in";
}

// applyConsolidation deals with the one part of the consolidation gating that CSS cannot do.
//
// The hiding itself is the .consolidation-only / .tombstones-only classes, on the same terms as
// .writer-only and .summariser-only above: a replica runs no sleep cycle, so it refuses
// ExplainConsolidation, PreviewConsolidation and Sleep alike, and a console offering them there
// would be a Decay tab of four "unavailable" cards and a Value column of dashes on every table
// — describing a forgetting schedule this instance does not carry out.
//
// What needs script is the tab itself: hiding the Decay button leaves it *selected* if the user
// was on it when the token changed, which shows a blank page with no way back. A hidden active
// tab therefore falls back to Search.
function applyConsolidation() {
  if (caps.consolidation) return;

  const decayTab = document.querySelector('nav button[data-tab="decay"]');

  if (decayTab && decayTab.classList.contains("active")) {
    document.querySelector('nav button[data-tab="search"]').click();
  }
}

// applyScope shows which groups this token is restricted to, when it is restricted at all.
//
// It reads caps.groupScoped rather than the length of caps.groups, because an empty list is
// ambiguous in exactly the way that matters: unscoped means the whole store, and "scoped to
// nothing" would mean the opposite. The server reports the two separately for this reason.
//
// Written into textContent, never innerHTML: group labels are client-supplied strings and this
// is the console that item 24.1 fixed a stored-XSS in.
function applyScope() {
  const pill = $("scope-pill");

  if (!caps.groupScoped) {
    pill.classList.add("hidden");

    return;
  }

  pill.classList.remove("hidden");
  pill.textContent = "groups: " + caps.groups.join(", ");
  pill.title =
    "This token is restricted to " +
    caps.groups.join(", ") +
    ". Records in other groups are not shown, writes are filed under this scope, and " +
    "Purge/Sleep/Preview are unavailable.";
}

// api issues a JSON request to the gateway (same origin). Returns parsed body,
// throws Error(message) on a non-2xx response so callers can surface it. The internal _retried flag
// guards the single silent-refresh replay used in server-login mode.
// inFlight counts requests currently in flight, driving the thin progress bar at the top of the
// page. It exists for the requests no button started - the Now tab's polls, decorateValues - which
// busy() cannot cover because there is no control to disable. Together they mean the page always
// says when it is waiting on the service, which on a slow deployment is the difference between
// "loading" and "broken".
let inFlight = 0;

function markInFlight(delta) {
  inFlight = Math.max(0, inFlight + delta);
  document.body.classList.toggle("busy", inFlight > 0);
}

async function api(method, path, body, _retried) {
  const headers = { "Content-Type": "application/json" };
  const tok = getToken();

  if (tok) {
    headers["Authorization"] = "Bearer " + tok;
  }

  const opts = { method, headers };

  if (body !== undefined) {
    opts.body = JSON.stringify(body);
  }

  markInFlight(1);

  let res;

  try {
    res = await fetch(path, opts);
  } finally {
    // Decremented as soon as the response headers are in, not after the body is parsed: the bar is
    // about waiting on the network, and a retry below re-enters api() and counts itself.
    markInFlight(-1);
  }

  // In server-login mode the access token lives in a cookie that may have just expired; try one
  // silent refresh through /auth/refresh and replay the request before surfacing the 401.
  if (
    res.status === 401 &&
    server.enabled &&
    !_retried &&
    path !== "/auth/refresh"
  ) {
    if (await serverRefresh()) {
      return api(method, path, body, true);
    }
  }

  const text = await res.text();
  let data = {};

  if (text) {
    try {
      data = JSON.parse(text);
    } catch (e) {
      data = { raw: text };
    }
  }

  if (!res.ok) {
    let msg =
      data.message ||
      data.error ||
      data.raw ||
      res.status + " " + res.statusText;

    // Give the authorization failures a plain-language message rather than the terse server body:
    // 401 means the token is missing/invalid, 403 means it is valid but its role is insufficient.
    if (res.status === 401)
      msg =
        oidc.enabled || server.enabled
          ? "Session expired — please sign in again."
          : "Unauthorized — the bearer token was not accepted.";
    if (res.status === 403)
      msg = "Forbidden — your token's role does not permit this action.";

    // A 429 is the deployment's own rate limit, not a fault: say so, and say how long to wait,
    // since the terse body ("rate limit exceeded") reads like a service failure otherwise. The
    // scope names which level bit — a global ceiling is nothing this operator did.
    if (res.status === 429) {
      const after = res.headers.get("Retry-After");
      const scope = data.scope ? " (" + data.scope + " limit)" : "";

      msg =
        "Rate limited" +
        scope +
        " — try again" +
        (after ? " in " + after + "s." : " shortly.");
    }

    const err = new Error(msg);
    err.status = res.status;

    throw err;
  }

  return data;
}

function toast(title, body, kind) {
  const el = document.createElement("div");
  el.className = "toast " + (kind || "ok");
  el.innerHTML = `<div class="t-title"></div><div class="t-body"></div>`;
  el.querySelector(".t-title").textContent = title;
  el.querySelector(".t-body").textContent = body || "";
  $("toast").appendChild(el);

  setTimeout(() => el.remove(), kind === "err" ? 7000 : 4000);
}

function ok(title, body) {
  toast(title, body, "ok");
}
function fail(title, err) {
  toast(title, err && err.message ? err.message : String(err), "err");
}

// shortId middle-truncates an id for display. Ids here are caller-chosen and routinely long and
// front-loaded with a shared prefix (an at:// URI is ~70 characters of which the last dozen are
// what distinguishes it), so both ends are kept and the middle elided. Display only: every call
// site pairs it with the full value on a title and a copy button.

// copyButton renders the clipboard button that carries the full id. The icon is inline SVG: the
// page is served with a strict CSP and ships as one file, so there is nothing to fetch.
function copyButton(text) {
  return `<button class="copy" data-act="copy" data-copy="${esc(text)}" title="Copy to clipboard" aria-label="Copy to clipboard"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" aria-hidden="true"><rect x="5.5" y="5.5" width="8" height="9" rx="1.5"/><path d="M10.5 3.5v-1h-8v9h1"/></svg></button>`;
}

// idCell is the standard rendering of an id in a table: truncated, titled with the full value,
// and copyable. inner overrides the displayed element (the links table makes it a link).
function idCell(id, inner) {
  const full = String(id == null ? "" : id);

  if (!full) return "—";

  const shown =
    inner ||
    `<span class="idv" title="${esc(full)}">${esc(shortId(full))}</span>`;

  return `<span class="idc">${shown}${copyButton(full)}</span>`;
}

// copyText writes to the clipboard and flashes the button that asked. The execCommand fallback is
// not vestigial: navigator.clipboard is undefined on a plain-HTTP origin (the demo stack, and any
// deployment behind a TLS-terminating proxy reached directly), which is exactly where a console
// is most likely to be opened.
async function copyText(text, el) {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
    } else {
      const ta = document.createElement("textarea");

      ta.value = text;
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();

      const done = document.execCommand("copy");

      document.body.removeChild(ta);

      if (!done) throw new Error("the browser refused the copy");
    }
  } catch (e) {
    fail("Copy failed", e);

    return;
  }

  if (!el) return;

  el.classList.add("copied");
  setTimeout(() => el.classList.remove("copied"), 900);
}

// Row actions are wired through data-* attributes and a single delegated listener, never inline
// onclick handlers with an interpolated id/name/body. An inline handler is a JS-string-inside-an-
// HTML-attribute context, and esc() does NOT make it safe there: the browser HTML-decodes the
// attribute value before parsing it as JS, so an esc()'d quote (&#39;) decodes back to a real one
// and breaks out of the string — turning any memory body, id, or event name (all attacker-supplied,
// possibly ingested from untrusted content) into executable script, i.e. stored XSS that could
// exfiltrate the bearer token held in localStorage. A data-* attribute is a plain attribute-value
// context where esc() is correct, and the listener looks the full object up by id in these
// registries rather than embedding it in the page.
const memRegistry = new Map();
const evRegistry = new Map();

// ---------------------------------------------------------- sortable column headers
//
// The two listings sort server-side (order_by/order_dir on GetMemories/GetEvents), so a header
// click is a re-query rather than a client-side re-sort — which it has to be, since a page is
// 25 rows out of a store that can hold hundreds of thousands and sorting those 25 would be a
// lie. The sort selects in each filter form are the state; a header click just writes to them
// and reloads, so there is one place the current sort lives and the two controls cannot
// disagree.
//
// sortScopes names, per table, the two form controls and the loader to call. Both loaders
// restart from offset 0, which is what a changed sort must do: page 3 of the old ordering has
// no meaning in the new one.
const sortScopes = {
  memories: {
    by: "mf-sort",
    dir: "mf-sortdir",
    load: () => loadMemories(),
  },
  events: { by: "ef-sort", dir: "ef-sortdir", load: () => loadEvents() },
};

// sortHeader renders one <th>, clickable when the column maps to a field the service can sort
// on. Columns that do not (a memory's computed Value, an event's memory count) stay plain
// headers: they are derived rather than stored, so there is no column to ORDER BY.
function sortHeader(scope, field, label, title) {
  const titleAttr = title ? ` title="${esc(title)}"` : "";

  if (!scope || !field) return `<th${titleAttr}>${esc(label)}</th>`;

  const controls = sortScopes[scope];
  const active = $(controls.by).value === field;
  // The arrow reports what the page is actually showing. On "Default" the direction is the
  // service's choice, so it is shown as neither arrow rather than guessed at.
  const dir = $(controls.dir).value;
  const arrow = !active
    ? ""
    : dir === "SORT_DIRECTION_ASC"
      ? " ▲"
      : dir === "SORT_DIRECTION_DESC"
        ? " ▼"
        : " •";

  return `<th class="sortable${active ? " sorted" : ""}"${titleAttr}
          data-act="sort" data-sort-scope="${esc(scope)}" data-sort-field="${esc(field)}"
        >${esc(label)}${arrow}</th>`;
}

// applySort is the header click: a new column adopts the service's natural direction for it
// (Default), and clicking the column already sorted on cycles through the two explicit
// directions. Starting at Default rather than at "descending" is what keeps the page from
// having an opinion about, say, whether a Group column should read A–Z or Z–A.
function applySort(scope, field) {
  const controls = sortScopes[scope];

  if (!controls) return;

  if ($(controls.by).value !== field) {
    $(controls.by).value = field;
    $(controls.dir).value = "";
  } else {
    $(controls.dir).value =
      $(controls.dir).value === "SORT_DIRECTION_ASC"
        ? "SORT_DIRECTION_DESC"
        : "SORT_DIRECTION_ASC";
  }

  controls.load();
}

// ACTIONS is the console's whole click/change surface: every control names an action with data-act
// and this table says what that action does. Nothing is bound inline.
//
// One table rather than a switch, and rather than the onclick= attributes this replaced, for three
// reasons. It is the enumerable form - a test can compare its keys against every data-act in the
// markup and the templates, which is what stops a renamed control becoming a silently dead button
// (the failure mode items 72 and 73 kept finding by hand). It survives innerHTML, so a row rendered
// into a table needs no rebinding. And it means the page needs no inline script or handler at all,
// which is what lets the console adopt a CSP without unsafe-inline.
//
// Handlers take (el, ev): el is the element carrying data-act, ev the originating event.
const ACTIONS = {
  copy: (el) => copyText(el.dataset.copy, el),

  "open-event": (el, ev) => {
    ev.preventDefault();
    openEvent(el.dataset.event);
  },

  sort: (el) => applySort(el.dataset.sortScope, el.dataset.sortField),

  "edit-memory": (el) => {
    const m = memRegistry.get(el.dataset.id);

    if (m) editMemory(m);
  },

  "recall-memory": (el) => recallMemory(el.dataset.id),
  "delete-memory": (el) => deleteMemory(el.dataset.id),
  "plot-memory": (el) => plotMemory(el.dataset.id),
  "memory-links": (el) => openLinks("memory", el.dataset.id),
  "event-links": (el) => openLinks("event", el.dataset.id),
  // Following a link stays one action rather than branching in the markup: the destination differs
  // by kind, and the handler already knows which is open. For a memory, its own links are the useful
  // next step; for an event, opening the EVENT is - its memories are what you came for, and its
  // links are one click further.
  "follow-link": (el) =>
    linksSubject.kind === "event"
      ? openEvent(el.dataset.id)
      : openLinks("memory", el.dataset.id),
  unlink: (el) => unlink(el.dataset.id, el.dataset.target),

  "edit-event": (el) => {
    const e = evRegistry.get(el.dataset.id);

    if (e) editEvent(e);
  },

  "end-event": (el) => endEvent(el.dataset.id),
  "delete-event": (el) => deleteEvent(el.dataset.id),

  // --- Search tab
  "do-search": () => doSearch(),
  "search-more": () => searchMore(),
  "close-event": () => closeEvent(),
  "open-summary-form": () => openSummaryForm(),
  "close-summary-form": () => closeSummaryForm(),
  "replace-with-summary": () => replaceWithSummary(),
  "summarise-llm": () => summariseWithLLM(),

  // --- Memories tab
  "save-memory": () => saveMemory(),
  "reset-memory-form": () => resetMemoryForm(),
  "sync-memory-extremum": () => syncMemoryExtremum(),
  "load-memories": () => loadMemories(),
  "clear-memory-filter": () => clearMemoryFilter(),
  "memories-page": (el) => memoriesPage(Number(el.dataset.dir)),
  "close-links": () => closeLinks(),
  "add-link": () => addLink(),

  // --- Events tab
  "save-event": () => saveEvent(),
  "reset-event-form": () => resetEventForm(),
  "sync-event-extremum": () => syncEventExtremum(),
  "load-events": () => loadEvents(),
  "clear-event-filter": () => clearEventFilter(),
  "events-page": (el) => eventsPage(Number(el.dataset.dir)),
  "load-candidates": () => loadCandidates(),

  // --- Now tab
  "load-now": () => loadNow(),
  "run-sleep": () => runSleep(),
  "goto-decay": () => showTab("decay"),

  // --- Decay tab
  "load-decay": () => loadDecay(),
  "run-preview": () => runPreview(),
  "load-forgotten": () => loadForgottenFirstPage(),
  "forgotten-prev": () => forgottenPage(-1),
  "forgotten-next": () => forgottenPage(1),
  "clear-forgotten": () => clearForgotten(),
};

// Two delegated listeners, both dispatching through the one ACTIONS table. The event type is
// carried by the ATTRIBUTE rather than the table, because the same action is legitimately reached
// both ways: "load-memories" is the List button (click) and the two sort selects (change).
//
// They must not share an attribute. A <select> emits a click when its dropdown is merely opened, so
// a single data-act read by both listeners would re-run the query every time the user looked at the
// control - a wasted round trip per glance, and on the Events tab a visibly flickering table.
// busy disables a control for the life of the work it started, and restores it in a finally so a
// failure cannot leave it dead. Wired once here rather than at each call site: every click already
// funnels through this dispatcher, so one wrapper covers all forty-odd controls, where the
// alternative was forty ad-hoc disables and thirty-nine chances to forget one.
//
// A control that is not a button (a link, a table cell) has no disabled property; setting it is
// harmless and the aria-busy attribute is what carries the state for those.
async function busy(el, work) {
  if (el.disabled) return;

  el.disabled = true;
  el.setAttribute("aria-busy", "true");

  try {
    await work;
  } finally {
    el.disabled = false;
    el.removeAttribute("aria-busy");
  }
}

function dispatcher(attribute) {
  return (ev) => {
    const el = ev.target.closest(`[${attribute}]`);

    if (!el) return;

    const handler = ACTIONS[el.getAttribute(attribute)];

    if (!handler) return;

    const result = handler(el, ev);

    // Only an async handler has anything to wait for. A synchronous one (a tab switch, a form
    // reset) returns undefined and must not be disabled, or the control would flicker for nothing.
    if (result && typeof result.then === "function") {
      busy(el, result);
    }
  };
}

document.addEventListener("click", dispatcher("data-act"));
document.addEventListener("change", dispatcher("data-change"));

// ------------------------------------------------------- Enter submits the card it is typed in
//
// None of these cards is a <form>, so Enter did nothing at all in them: every one of the fields
// on the Search, Memories and Events tabs had to be left by hand to reach the button. Rather
// than wrap five cards in forms - which brings native submission, its navigation, and a
// preventDefault on each - one delegated keydown maps the card a field sits in to that card's
// primary action, mirroring the delegated click handler above.
//
// enterActions is an ALLOW-LIST rather than a lookup of window[name]: the name arrives from a
// DOM attribute, and resolving one of those to a callable is how a data attribute becomes a
// way to call anything on the page. These attributes are all in static markup and so are safe
// either way, but the map is also the readable statement of which card has a primary action.
//
// The two destructive forms are deliberately NOT in it - the summary that replaces every
// memory of an event, and the forgotten log's Clear - because a keystroke should not be able
// to delete anything, and a confirm() is no answer when the Enter that opened it can dismiss
// it too. Adding a link IS here: it writes, but it destroys nothing.
const enterActions = {
  search: () => doSearch(),
  "memory-save": () => saveMemory(),
  "memory-filter": () => loadMemories(),
  "memory-link": () => addLink(),
  "event-save": () => saveEvent(),
  "event-filter": () => loadEvents(),
};

document.addEventListener("keydown", (ev) => {
  if (ev.key !== "Enter") return;

  // isComposing is the load-bearing one: Enter is how an IME accepts a candidate, so without
  // this a Japanese or Chinese user would fire the action every time they committed a word.
  // A modifier held means the user is asking for something else, and repeat is a held key -
  // neither should submit, and repeat least of all when the action is a write.
  if (
    ev.isComposing ||
    ev.repeat ||
    ev.shiftKey ||
    ev.ctrlKey ||
    ev.altKey ||
    ev.metaKey
  ) {
    return;
  }

  const field = ev.target;

  // A textarea takes Enter as a newline - a memory body and an event description are both
  // multi-line, and swallowing that would be a worse bug than the one this fixes. Buttons and
  // links already activate on Enter natively; intercepting would fire twice.
  if (
    !field.matches ||
    !field.matches("input, select") ||
    field.closest("a, button")
  ) {
    return;
  }

  const card = field.closest("[data-enter]");
  const action = card && enterActions[card.dataset.enter];

  if (!action) return;

  ev.preventDefault();
  action();
});

// The gateway (de)serialises proto int64 fields as JSON strings. UnixNano <-> local datetime.
function nanoToLocal(nanoStr) {
  if (!nanoStr || nanoStr === "0") return "";
  const ms = Number(BigInt(nanoStr) / 1000000n);
  const d = new Date(ms);
  const pad = (n) => String(n).padStart(2, "0");

  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// ageCell renders a timestamp as a compact age ("3h", "12d") with the absolute value in the
// title, and is what every table column showing a time uses. Two reasons for the compact form
// over the absolute one. It is what the decay maths runs on — age since the last recall is the
// input to every consolidation method — so it is the number that says whether a row is close
// to being forgotten, which a datetime makes the reader work out. And a datetime is the widest
// cell in these tables while being the one nobody reads digit by digit; the exact value is one
// hover away, and still in the DOM for anyone who needs it.
//
// Returns HTML, so it must be interpolated unescaped — everything it interpolates is derived
// from a number and escaped anyway.
function ageCell(nanoStr) {
  if (!nanoStr || nanoStr === "0") return "—";

  // The arithmetic is ageLabel's, in lib.js, so the unit boundaries and the future-timestamp case
  // can be tested against a fixed clock rather than whatever Date.now() happens to be.
  const label = ageLabel(Date.now(), nanoStr);

  return `<span class="age" title="${esc(nanoToText(nanoStr))}">${esc(label)}</span>`;
}

function localToNano(val) {
  if (!val) return undefined;
  const ms = new Date(val).getTime();

  if (isNaN(ms)) return undefined;

  return String(BigInt(ms) * 1000000n);
}

function intOrUndef(id) {
  const v = $(id).value.trim();

  if (v === "") return undefined;

  return Number(v);
}

function strOrUndef(id) {
  const v = $(id).value.trim();

  return v === "" ? undefined : v;
}

// ------------------------------------------------------------------ tabs

// TAB_LOADERS says what a tab needs fetched when it becomes visible. It replaced a hard-coded check
// for the Decay tab, which was fine while Decay was the only live view; Now is another, and it is
// also the tab the console OPENS on, so its loader has to be reachable from boot() as well as from
// a click. A map is what makes those two callers share one answer.
//
// The tabs not listed here render from state the page already holds, or from a form the user has to
// fill in first, so they fetch nothing on arrival.
const TAB_LOADERS = {
  now: () => startNowPolling(),
  decay: () => {
    // A live view of the store's current standing: loaded the first time it is opened rather than
    // on page load, since it costs the server a snapshot.
    if (!decayInputs) loadDecay();
  },
};

function showTab(name) {
  document
    .querySelectorAll("nav button")
    .forEach((x) => x.classList.toggle("active", x.dataset.tab === name));
  document
    .querySelectorAll(".tab")
    .forEach((x) => x.classList.toggle("active", x.id === "tab-" + name));

  // Only the Now tab polls, and only while it is the tab you are looking at. Leaving it is as good
  // a reason to stop as hiding the window.
  if (name !== "now") stopNowPolling();

  const load = TAB_LOADERS[name];

  if (load) load();
}

document.querySelectorAll("nav button").forEach((b) => {
  b.addEventListener("click", () => showTab(b.dataset.tab));
});

// ------------------------------------------------------------------ oidc login (idp mode only)
// These implement an OpenID Connect Authorization Code + PKCE flow entirely in the browser, with no
// external library and no client secret. They run only when boot() finds auth.method "idp" in
// /ui/config; the access token they obtain is sent as the Authorization: Bearer on /v1 calls and
// verified by the service exactly like any other idp token.

// redirectUri is the console's own URL with any query/hash stripped (.../ui). It must be registered
// as an allowed callback on the identity provider's client.
function redirectUri() {
  return window.location.origin + window.location.pathname;
}

// discover fetches the provider's OIDC metadata to learn its authorization and token endpoints.
async function discover(issuer) {
  const res = await fetch(
    issuer.replace(/\/$/, "") + "/.well-known/openid-configuration",
  );

  if (!res.ok) {
    throw new Error("OIDC discovery failed (" + res.status + ")");
  }

  return res.json();
}

// login starts the flow: it mints a PKCE verifier/challenge and a state, stashes them for the
// return leg, and redirects the browser to the provider's authorization endpoint.
async function login() {
  try {
    const verifier = randomString(64);
    const state = randomString(16);
    const challenge = b64url(
      await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier)),
    );

    sessionStorage.setItem(PKCE_KEY, JSON.stringify({ verifier, state }));

    const p = new URLSearchParams({
      response_type: "code",
      client_id: oidc.cfg.clientId,
      redirect_uri: redirectUri(),
      scope: oidc.cfg.scopes || "openid profile",
      state,
      code_challenge: challenge,
      code_challenge_method: "S256",
    });

    // Providers whose access token is opaque without an API audience (Auth0) need this to mint a
    // verifiable JWT; providers that ignore it (Keycloak) are unaffected.
    if (oidc.cfg.audience) {
      p.set("audience", oidc.cfg.audience);
    }

    window.location.assign(
      oidc.endpoints.authorization_endpoint + "?" + p.toString(),
    );
  } catch (e) {
    fail("Sign in failed", e);
  }
}

// cleanUrl strips the ?code/&state (or ?error) query the provider appended, so a reload does not
// replay the callback and the address bar stays tidy.
function cleanUrl() {
  window.history.replaceState({}, document.title, redirectUri());
}

// handleRedirect completes the flow when the page has loaded as the provider's callback: it
// validates the returned state against the stashed one and exchanges the code for tokens.
async function handleRedirect() {
  const params = new URLSearchParams(window.location.search);
  const code = params.get("code");
  const err = params.get("error");

  if (err) {
    cleanUrl();
    fail("Sign in failed", new Error(params.get("error_description") || err));

    return;
  }

  if (!code) {
    return;
  }

  const saved = JSON.parse(sessionStorage.getItem(PKCE_KEY) || "null");
  sessionStorage.removeItem(PKCE_KEY);
  cleanUrl();

  if (!saved || saved.state !== params.get("state")) {
    fail("Sign in failed", new Error("state mismatch — please try again"));

    return;
  }

  await exchange(
    new URLSearchParams({
      grant_type: "authorization_code",
      code,
      redirect_uri: redirectUri(),
      client_id: oidc.cfg.clientId,
      code_verifier: saved.verifier,
    }),
  );
}

// exchange posts a grant (authorization_code or refresh_token) to the token endpoint and stores the
// resulting session, clearing it and throwing on failure.
async function exchange(body) {
  const res = await fetch(oidc.endpoints.token_endpoint, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: body.toString(),
  });

  const data = await res.json().catch(() => ({}));

  if (!res.ok) {
    clearSession();

    throw new Error(
      data.error_description || data.error || "token endpoint " + res.status,
    );
  }

  setSession(data);
}

// setSession records the tokens and schedules a silent refresh before the access token expires.
function setSession(data) {
  oidc.accessToken = data.access_token || "";
  sessionStorage.setItem(OIDC_ACCESS_KEY, oidc.accessToken);

  if (data.refresh_token) {
    sessionStorage.setItem(OIDC_REFRESH_KEY, data.refresh_token);
  }

  const expiry = Number(data.expires_in)
    ? Date.now() + Number(data.expires_in) * 1000
    : 0;
  sessionStorage.setItem(OIDC_EXPIRY_KEY, String(expiry));

  scheduleRefresh(expiry);
}

// scheduleRefresh arms a timer to refresh ~60s before expiry, but only when a refresh token is
// available; otherwise the session simply lapses and the caller must sign in again.
function scheduleRefresh(expiry) {
  if (oidc.refreshTimer) {
    clearTimeout(oidc.refreshTimer);
    oidc.refreshTimer = null;
  }

  if (!expiry || !sessionStorage.getItem(OIDC_REFRESH_KEY)) {
    return;
  }

  oidc.refreshTimer = setTimeout(
    refresh,
    Math.max(expiry - Date.now() - 60000, 5000),
  );
}

// refresh silently swaps the refresh token for a new access token; on failure it drops the session
// so the UI falls back to a Sign in prompt.
async function refresh() {
  const refreshToken = sessionStorage.getItem(OIDC_REFRESH_KEY);

  if (!refreshToken) {
    return;
  }

  try {
    await exchange(
      new URLSearchParams({
        grant_type: "refresh_token",
        refresh_token: refreshToken,
        client_id: oidc.cfg.clientId,
      }),
    );

    refreshCaps();
  } catch (e) {
    clearSession();

    // Re-resolving with no token is what raises the gate: it is the same signed-out state as a
    // sign-out, and going through refreshCaps keeps that decision in one place.
    refreshCaps();
  }
}

// clearSession forgets the tokens and cancels any pending refresh.
function clearSession() {
  oidc.accessToken = "";
  sessionStorage.removeItem(OIDC_ACCESS_KEY);
  sessionStorage.removeItem(OIDC_REFRESH_KEY);
  sessionStorage.removeItem(OIDC_EXPIRY_KEY);

  if (oidc.refreshTimer) {
    clearTimeout(oidc.refreshTimer);
    oidc.refreshTimer = null;
  }
}

// logout clears the local session and, when the provider advertises an end-session endpoint, does
// an RP-initiated logout so the provider's own session is dropped too.
function logout() {
  const end = oidc.endpoints && oidc.endpoints.end_session_endpoint;

  clearSession();

  if (end) {
    const p = new URLSearchParams({
      client_id: oidc.cfg.clientId,
      post_logout_redirect_uri: redirectUri(),
    });
    window.location.assign(end + "?" + p.toString());

    return;
  }

  refreshCaps();
}

// boot reads /ui/config, wires the login gate for whichever sign-in mode this deployment uses
// (under idp processing a provider callback if the page was loaded as one), then resolves
// capabilities — which is what lifts the gate, or leaves it standing with the right card on it.
async function boot() {
  let cfg = { authMethod: "none" };

  try {
    cfg = await (await fetch("/ui/config")).json();
  } catch (e) {
    // A missing/unreachable config endpoint leaves the console in manual-token mode; a refused /v1
    // call then still raises the gate, since refreshCaps promotes authCfg on a 401/403.
  }

  authCfg.method = cfg.authMethod || "none";
  authCfg.enabled = authCfg.method !== "none";

  if (cfg.authMethod === "idp" && cfg.loginMode === "server") {
    // Server-hosted login: the service owns the OIDC flow and the session cookie, so the page only
    // navigates to its endpoints. refreshCaps() (at the end of boot) resolves whether a session
    // exists, and applyGate (via applyCaps) presents the card if it does not.
    server.enabled = true;

    $("gate-login").addEventListener("click", () =>
      window.location.assign("/auth/login"),
    );
  } else if (cfg.authMethod === "idp" && cfg.clientId) {
    oidc.enabled = true;
    oidc.cfg = cfg;

    $("gate-login").addEventListener("click", login);

    try {
      oidc.endpoints = await discover(cfg.issuer);

      await handleRedirect();
    } catch (e) {
      fail("Sign in unavailable", e);
    }

    if (oidc.accessToken) {
      scheduleRefresh(Number(sessionStorage.getItem(OIDC_EXPIRY_KEY)) || 0);
    }
  }

  // The manual form and Sign out are wired in every mode: the form is only reachable when the gate
  // presents it (including the idp deployment that configured no UI client, where pasting a token
  // obtained elsewhere is the only way in), and signOut dispatches on the mode itself.
  $("gate-form").addEventListener("submit", signInManual);
  $("logout-btn").addEventListener("click", signOut);

  await refreshCaps();

  // The console opens on Now, and a tab's loader normally runs when it is clicked. Nothing clicks
  // the tab that is already selected, so without this the landing view renders empty on first paint
  // - which is the one thing it exists to prevent. After refreshCaps, because what it loads depends
  // on what this caller may see.
  const active = document.querySelector("nav button.active");

  if (active) {
    const load = TAB_LOADERS[active.dataset.tab];

    if (load) load();
  }
}

// Resolve config and the current session on load: until it answers the page stays gated, so an
// unauthenticated load presents a sign-in card rather than a console whose every call is refused.
boot();

// ------------------------------------------------------------------ search
// openEvent loads one event and every memory attached to it, then shows them in the event card
// on the search tab. Reached by clicking an event id in any of the three result tables, or by
// searching with only an event id filled in.
//
// It renders into its own card rather than into #search-results, which is what it used to do:
// an event opened from the Events or Memories tab then overwrote whatever search was showing,
// so returning to Search found the single event in place of the results. The card is the whole
// fix - the two views no longer share a container, so neither can destroy the other.
// scrollBehaviour picks the scroll animation, and exists because "smooth" is not a safe default.
// Under prefers-reduced-motion: reduce, Chrome does not shorten a smooth scroll - it DROPS it,
// leaving the page exactly where it was. So a hard-coded "smooth" silently fails to scroll at
// all for anyone who has reduced motion set, which is precisely the reader who most needs the
// page to have moved to what they asked for.
function scrollBehaviour() {
  return window.matchMedia("(prefers-reduced-motion: reduce)").matches
    ? "instant"
    : "smooth";
}

// revealCard scrolls a panel that has just been opened into view. Both panels it serves - the
// event card and the links card - are opened from a row on a DIFFERENT tab, and switching tabs
// does not move the scroll position, so without this the panel is reliably somewhere other than
// where the user is looking: off the top on the Search tab, off the bottom on the Memories one.
// Guarded on the element being displayed so it can be called unconditionally.
function revealCard(id) {
  const el = $(id);

  if (el && !el.classList.contains("hidden")) {
    el.scrollIntoView({ behavior: scrollBehaviour(), block: "start" });
  }
}

// eventSubject is the event the card is currently showing, so the summarise controls know what
// they are acting on without re-reading the DOM - the same shape as linksSubject.
// eventSubjectMemories is how many memories that render showed, which is what the summarise
// confirmation quotes, and eventSubjectGroup is the event's own group, which the manual
// summary form defaults to.
let eventSubject = "";
let eventSubjectMemories = 0;
let eventSubjectGroup = "";

async function openEvent(id) {
  try {
    const data = await api(
      "GET",
      "/v1/events/" + encodeURIComponent(id) + "?memories=true",
    );
    const ev = data.event || {};
    const memories = ev.memories || [];
    const end = ev.timeEnd && ev.timeEnd !== "0" ? ageCell(ev.timeEnd) : "open";

    // The links live in all three tables; surface the result on the search tab wherever it was
    // clicked from, so an event always opens in one known place.
    document.querySelector('nav button[data-tab="search"]').click();

    eventSubject = ev.id || id;
    eventSubjectMemories = memories.length;
    eventSubjectGroup = ev.group || "";

    $("event-card").classList.remove("hidden");
    $("event-view").innerHTML = `<div class="event-head">
      <h3>${esc(ev.name || ev.id)}</h3>
      <div class="muted">${esc(ev.description || "")}</div>
      <div class="kv">id ${esc(ev.id)} · significance ${esc(ev.significance ?? "")} · group ${esc(ev.group || "—")} · ${ageCell(ev.timeStart)} → ${end} · ${memories.length} memor${memories.length === 1 ? "y" : "ies"}</div>
    </div>
    <div id="event-memories"></div>`;
    renderMemories("event-memories", memories, {});
    closeSummaryForm();

    // Nothing to condense means nothing to offer: both RPCs would refuse an event holding no
    // memories, and the LLM one has no text to prompt with.
    $("event-summarise").classList.toggle("hidden", memories.length === 0);

    // The card is first on the tab, but the tab switch above does not reset the scroll
    // position, so arriving from a row halfway down the Events tab would land on the middle of
    // this one. Scroll to what was asked for rather than to wherever the last page was left.
    revealCard("event-card");

    ok("Event", (ev.name || ev.id) + " — " + memories.length + " memories");
  } catch (e) {
    fail("Open event failed", e);
  }
}

function closeEvent() {
  eventSubject = "";
  eventSubjectMemories = 0;
  eventSubjectGroup = "";
  $("event-card").classList.add("hidden");
  $("event-view").innerHTML = "";
  closeSummaryForm();
}

// ---------------------------------------------------------------- summarisation
//
// Two buttons for two RPCs that differ only in who authors the summary: the service has no
// visibility into memory content, so ReplaceMemoriesWithSummary takes one from the caller,
// while SummariseMemories has the optional embedded LLM write it. Both then go through the
// same insertSummary path server-side - one transaction, every memory of the event replaced by
// one flagged is_summary - which is why both are confirmed here first. There is no undo.

// The group defaults to the event's own. A summary is stored as a plain memory and inherits
// nothing, so leaving it blank would quietly drop the replacement out of the group every
// memory it replaced was in - and out of a scoped caller's own partition.
function openSummaryForm() {
  $("summary-form").classList.remove("hidden");
  $("summary-group").value = eventSubjectGroup;
  $("summary-body").focus();
}

function closeSummaryForm() {
  $("summary-form").classList.add("hidden");
  ["summary-body", "summary-sig", "summary-group"].forEach(
    (id) => ($(id).value = ""),
  );
}

// confirmSummarise asks once, naming the cost in the terms that matter: how many memories go.
// The count is the one openEvent rendered from, so it cannot disagree with what is on screen.
function confirmSummarise(how) {
  if (!eventSubject) return false;

  return confirm(
    "Summarise event " +
      eventSubject +
      " " +
      how +
      "?\n\nAll " +
      eventSubjectMemories +
      " of its memories are deleted and replaced by the single summary. " +
      "This cannot be undone.",
  );
}

async function replaceWithSummary() {
  const body = $("summary-body").value.trim();

  if (!body) {
    fail("Summarise", new Error("the summary body cannot be empty"));

    return;
  }

  if (!confirmSummarise("with the text you have written")) return;

  // The gateway binds this RPC's body to the summary itself (body: "summary"), so the payload
  // is a Memory, not a wrapper carrying one.
  const summary = { body };
  const significance = intOrUndef("summary-sig");

  if (significance !== undefined) summary.significance = significance;

  const group = strOrUndef("summary-group");
  if (group) summary.group = group;

  try {
    const res = await api(
      "POST",
      "/v1/events/" + encodeURIComponent(eventSubject) + "/summary",
      summary,
    );

    ok(
      "Summarised",
      (res.memoriesReplaced || 0) + " memories replaced by " + res.id,
    );

    await afterSummarise();
  } catch (e) {
    fail("Summarise failed", e);
  }
}

async function summariseWithLLM() {
  if (!confirmSummarise("with the embedded LLM")) return;

  try {
    // No significance sent: unset means the summary inherits the highest significance among
    // the memories it replaces, which is the right default and the one the RPC documents.
    const res = await api(
      "POST",
      "/v1/events/" + encodeURIComponent(eventSubject) + "/summarise",
      {},
    );

    ok(
      "Summarised",
      (res.memoriesReplaced || 0) + " memories replaced by " + res.id,
    );

    await afterSummarise();
  } catch (e) {
    fail("Summarise failed", e);
  }
}

// afterSummarise re-opens the event so the card shows the summary that now stands in for its
// memories - the point of the operation is what replaced them, so leaving the pre-replacement
// list on screen would be showing rows that no longer exist.
async function afterSummarise() {
  closeSummaryForm();

  await openEvent(eventSubject);

  // The event we just condensed is no longer a candidate (the service drops it on the way
  // through), so a loaded list is now wrong. Only refreshed if one is on screen — this is the
  // Events tab, and the user may never have opened it.
  if ($("candidates-list").querySelector("table")) await loadCandidates();
}

async function doSearch() {
  const query = $("s-query").value.trim();
  const evId = strOrUndef("s-event");

  if (!query) {
    // No text query: an event id on its own lists that event's memories.
    if (evId) {
      openEvent(evId);

      return;
    }

    fail(
      "Search",
      new Error("enter a query, or an event id to list its memories"),
    );

    return;
  }

  const body = { query };
  const limit = intOrUndef("s-limit");

  if (limit !== undefined) body.limit = limit;

  if (evId) body.event_id = evId;

  const group = strOrUndef("s-group");
  if (group) body.group = group;

  if ($("s-reinforce").checked) body.reinforce = true;

  // Only sent when the deployment offers a choice; otherwise the server's own default (keyword)
  // applies and the request looks exactly as it did before modes existed.
  const mode = $("s-mode").value;
  if (mode) body.mode = mode;

  try {
    const data = await api("POST", "/v1/memories/search", body);

    // A fresh search supersedes whatever event was open — it is a new question. The two cards
    // could coexist, but leaving a stale event above the results is what made the old shared
    // panel confusing in the first place.
    closeEvent();

    const results = data.memories || [];

    renderMemories("search-results", results, { reinforced: body.reinforce });

    // "Show more" re-runs the SAME query with a larger limit; it is deliberately not a next page.
    // Two reasons, and the second is the load-bearing one:
    //
    //   SearchMemories over-fetches and then truncates (rankingOverFetch), blending relevance with
    //   the store's own significance and recall count. A second, independently ranked query can
    //   return a different candidate set, so a "page 2" could repeat items or drop them.
    //
    //   And a REINFORCING search recalls exactly the page it returns. Paging would therefore
    //   reinforce a second set of memories - resetting their decay clocks - as a side effect of
    //   navigating a list. In a store whose whole premise is that recall is what keeps something
    //   alive, that is not a UX wrinkle; it is the console quietly changing what survives.
    //
    // Re-running with a larger limit keeps it one query, one ranking, one reinforcement set.
    searchShownLimit = results.length;
    renderSearchMore(results.length, Number(body.limit || 0));

    ok("Search", results.length + " result(s)");
  } catch (e) {
    fail("Search failed", e);
  }
}

// searchShownLimit is how many results the last search actually returned, so "Show more" knows
// whether there was a ceiling worth raising.
let searchShownLimit = 0;

const SEARCH_MORE_STEP = 25;

function renderSearchMore(returned, limit) {
  const container = $("search-more");

  // Fewer results than the limit means the query is exhausted - there is no more to show, and
  // offering the button would return the same list again.
  if (!limit || returned < limit) {
    container.innerHTML = "";

    return;
  }

  container.innerHTML = `<button class="btn ghost small" data-act="search-more"
     title="Re-runs the same query with a larger limit. Search results are ranked as one set, and a
reinforcing search recalls exactly what it returns, so paging would both re-rank and reinforce a
second set of memories.">Show more</button>`;
}

// searchMore raises the limit and re-runs. It writes the new limit into the form rather than
// keeping it beside it, so what the page shows and what the next search asks for cannot disagree.
function searchMore() {
  const current = intOrUndef("s-limit") || searchShownLimit || SEARCH_MORE_STEP;

  $("s-limit").value = current + SEARCH_MORE_STEP;

  return doSearch();
}

function memoryRow(m) {
  const flags = [];

  if (m.isSummary) flags.push('<span class="pill summary">summary</span>');
  if (m.isBinary === "TRUE")
    flags.push('<span class="pill binary">binary</span>');

  // The event id is a link: clicking it opens the event and all of its memories (openEvent).
  const eventCell = m.eventId
    ? `<a class="evlink" data-act="open-event" data-event="${esc(m.eventId)}" title="${esc(m.eventId)}">${esc(shortId(m.eventId))}</a>`
    : "—";

  // The body is the second row of the pair, spanning the table; a memory with no body renders
  // as a single row (and keeps its bottom border, hence the conditional class). The span
  // tracks whether the Value column is there: a colspan larger than the column count does not
  // clamp, it EXTENDS the table by a phantom column, so leaving it at 8 on a replica would
  // give every body row one more cell than its header.
  const body = m.body ? String(m.body) : "";

  return `<tr${body ? ' class="has-body"' : ""}>
    <td>${esc(m.significance ?? "")}</td>
    <!-- Filled in by decorateValues() once the server has valued the page; a dash until then, and
         permanently if this deployment will not answer (a replica, or a token that is refused). -->
    <td class="decay consolidation-only" data-value-for="${esc(m.id)}"><span class="muted">…</span></td>
    <td class="id">${eventCell}</td>
    <td>${groupCell(m)}</td>
    <td>${esc(m.recallCount || 0)}</td>
    <td>${ageCell(m.timeStamp)} ${flags.join(" ")}</td>
    <td class="id">${idCell(m.id)}</td>
    <td class="actions-col">
      <div class="actions">
        <button class="btn small ghost writer-only" data-act="edit-memory" data-id="${esc(m.id)}">Edit</button>
        <button class="btn small ghost" data-act="recall-memory" data-id="${esc(m.id)}">Recall</button>
        <button class="btn small ghost" data-act="memory-links" data-id="${esc(m.id)}">Links</button>
        <button class="btn small danger writer-only" data-act="delete-memory" data-id="${esc(m.id)}">Delete</button>
      </div>
    </td>
  </tr>${
    body
      ? `<tr><td class="bodycell" colspan="${caps.consolidation ? 8 : 7}"><div class="bodytext">${esc(body)}</div></td></tr>`
      : ""
  }`;
}

function renderMemories(target, memories, opts) {
  opts = opts || {};

  if (!memories.length) {
    $(target).innerHTML = '<div class="empty">No memories.</div>';

    return;
  }

  // Register the displayed rows so the delegated edit handler can resolve the full object by id
  // instead of it being embedded in the markup.
  memories.forEach((m) => memRegistry.set(m.id, m));

  // Only the Memories tab's own table sorts by header: the search results are ordered by
  // relevance blended with the store's own ranking (see search.significanceWeight), and an
  // event's memories are one event's whole set. Re-sorting either through GetMemories would
  // replace the result set rather than reorder it.
  const scope = target === "memories-list" ? "memories" : "";
  const rows = memories.map(memoryRow).join("");
  $(target).innerHTML = `<div class="tablewrap"><table>
    <thead><tr>
      ${sortHeader(scope, "significance", "Sig")}
      <th class="consolidation-only" title="computed value against the deletion threshold">Value</th>
      <th>Event</th>
      ${sortHeader(scope, "group", "Group")}
      ${sortHeader(scope, "recall_count", "Recalls")}
      ${sortHeader(scope, "timestamp", "Age", "how long ago the memory was stored — hover a value for the exact timestamp")}
      ${sortHeader(scope, "id", "Id")}
      <th class="actions-col"></th>
    </tr></thead>
    <tbody>${rows}</tbody>
  </table></div>`;

  // Deliberately not awaited: the table is useful the moment it renders, and the valuation is an
  // extra round trip that must not hold it back (or fail it).
  decorateValues(memories);
}

// Offset of the currently displayed memories page. loadMemories() restarts from the top;
// memoriesPage() steps by one page. Both funnel through fetchMemories(), which reads sort + page
// size from the form. Paging is essential here: the store can hold hundreds of thousands of rows.
let memoriesOffset = 0;

function memoriesPageSize() {
  return intOrUndef("mf-pagesize") || 25;
}

async function loadMemories() {
  memoriesOffset = 0;

  await fetchMemories();
}

async function memoriesPage(dir) {
  const next = memoriesOffset + dir * memoriesPageSize();

  memoriesOffset = next < 0 ? 0 : next;

  await fetchMemories();
}

// syncMemoryExtremum greys out the significance min/max inputs while an extremum (highest/lowest) is
// selected: the server rejects significance_extremum combined with significance_min/max, so the two
// are mutually exclusive in the form too.
function syncMemoryExtremum() {
  const on = $("mf-extremum").value !== "";

  ["mf-sigmin", "mf-sigmax"].forEach((id) => {
    $(id).disabled = on;

    if (on) $(id).value = "";
  });
}

async function fetchMemories() {
  const params = new URLSearchParams();
  const extremum = $("mf-extremum").value;

  // significance_extremum ("only the memories tied at the highest/lowest significance") replaces the
  // min/max range rather than combining with it — sending both is an InvalidArgument.
  if (extremum) {
    params.set("significance_extremum", extremum);
  } else {
    const sigMin = intOrUndef("mf-sigmin");
    const sigMax = intOrUndef("mf-sigmax");

    if (sigMin !== undefined) params.set("significance_min", sigMin);
    if (sigMax !== undefined) params.set("significance_max", sigMax);
  }

  const group = strOrUndef("mf-group");
  if (group) params.set("group", group);

  // One parameter per pair (append, not set): the filter is a conjunction, and the gateway
  // parses a repeated field as repeated parameters rather than one comma-joined value.
  parseMetadataPairs($("mf-metadata").value, ",").forEach((pair) =>
    params.append("metadata", pair),
  );

  const recalled = $("mf-recalled").value;
  if (recalled) params.set("recalled", recalled);

  const from = localToNano($("mf-from").value);
  if (from) params.set("timestamp_min", from);

  const to = localToNano($("mf-to").value);
  if (to) params.set("timestamp_max", to);

  params.set("order_by", $("mf-sort").value);

  // Omitted rather than sent empty when the direction is "Default": the service reads an
  // absent order_dir as the sort field's own natural direction, which is knowledge this page
  // deliberately does not duplicate — the same reason the Decay tab computes no decay maths.
  const sortDir = $("mf-sortdir").value;
  if (sortDir) params.set("order_dir", sortDir);

  params.set("limit", memoriesPageSize());
  params.set("offset", memoriesOffset);

  try {
    const data = await api("GET", "/v1/memories?" + params.toString());
    const memories = data.memories || [];
    const total = Number(data.totalCount || 0);

    renderMemories("memories-list", memories);
    updateMemoriesPager(memories.length, total);
    ok("Memories", memories.length + " of " + total + " shown");
  } catch (e) {
    fail("Load memories failed", e);
  }
}

function updateMemoriesPager(count, total) {
  const from = total === 0 ? 0 : memoriesOffset + 1;
  const to = memoriesOffset + count;

  $("mem-range").textContent =
    total === 0 ? "no memories" : from + "–" + to + " of " + total;
  $("mem-prev").disabled = memoriesOffset <= 0;
  $("mem-next").disabled = to >= total;
}

function clearMemoryFilter() {
  [
    "mf-sigmin",
    "mf-sigmax",
    "mf-group",
    "mf-metadata",
    "mf-from",
    "mf-to",
  ].forEach((id) => ($(id).value = ""));
  $("mf-extremum").value = "";
  $("mf-recalled").value = "";
  // The sort is part of the filter form, so Clear resets it too — back to the service's own
  // default ordering rather than to whatever column was last clicked.
  $("mf-sort").value = "significance";
  $("mf-sortdir").value = "";
  syncMemoryExtremum();
}

// metadataFromForm turns the write form's textarea into the object the API takes. Returns
// undefined when empty, so an omitted field means "leave unchanged" on a PATCH rather than
// sending an empty map (which the server could not tell apart from an absent one anyway -
// clearing is what clear_metadata is for).
// metadataFromForm reads a metadata textarea into the object the wire wants. Parameterised by
// element id rather than hard-coded to the memory form, so the event form uses the same parser
// rather than a second copy of the first-"=" rule that would drift from it.
function metadataFromForm(id) {
  const pairs = parseMetadataPairs($(id).value, "\n");

  if (!pairs.length) return undefined;

  const out = {};

  pairs.forEach((pair) => {
    const at = pair.indexOf("=");

    out[pair.slice(0, at).trim()] = pair.slice(at + 1).trim();
  });

  return out;
}

async function saveMemory() {
  const id = $("mem-id").value;
  const metadata = metadataFromForm("mem-metadata");
  const body = {
    body: $("mem-body").value,
    significance: intOrUndef("mem-sig"),
    event_id: strOrUndef("mem-event"),
    group: strOrUndef("mem-group"),
    metadata: metadata,
  };

  try {
    if (id) {
      // Emptying the textarea on an edit means "remove the labels", which the empty map cannot
      // say - an absent map and an empty one are the same on the wire, and every other field
      // reads unset as "leave unchanged". clear_metadata is what carries the intent.
      if (metadata === undefined) body.clear_metadata = true;

      await api("PATCH", "/v1/memories/" + encodeURIComponent(id), body);
      ok("Memory updated", id);
    } else {
      const res = await api("POST", "/v1/memories", body);

      if (res.rejected) {
        toast(
          "Memory rejected",
          "significance below the configured minimum — not stored",
          "err",
        );
      } else {
        ok("Memory created", res.id);
      }
    }

    resetMemoryForm();
    loadMemories();
  } catch (e) {
    fail("Save memory failed", e);
  }
}

function editMemory(m) {
  $("mem-id").value = m.id;
  $("mem-body").value = m.body || "";
  $("mem-sig").value = m.significance ?? "";
  $("mem-event").value = m.eventId || "";
  $("mem-group").value = m.group || "";
  $("mem-metadata").value = metadataToForm(m.metadata);
  $("mem-form-title").textContent = "Edit memory";
  $("mem-save").textContent = "Update";
  $("mem-edit-hint").classList.remove("hidden");
  document.querySelector('nav button[data-tab="memories"]').click();
  window.scrollTo({ top: 0, behavior: scrollBehaviour() });
}

function resetMemoryForm() {
  [
    "mem-id",
    "mem-body",
    "mem-sig",
    "mem-event",
    "mem-group",
    "mem-metadata",
  ].forEach((id) => ($(id).value = ""));
  $("mem-form-title").textContent = "Create memory";
  $("mem-save").textContent = "Create";
  $("mem-edit-hint").classList.add("hidden");
}

async function recallMemory(id) {
  try {
    await api("POST", "/v1/memories/recall", { ids: [id] });
    ok("Recalled", id);
    loadMemories();
  } catch (e) {
    fail("Recall failed", e);
  }
}

// ---------------------------------------------------------------------- links
//
// The console's view of the associative graph. Everything here is served: the links, their
// direction, and the summed significance the decay maths damps - the page computes none of it, for
// the same reason the Decay tab does not compute its own curve.

// linksSubject is what the card is currently showing: {kind, id}, where kind is "memory" or
// "event". One card serves both because they are one mechanism - item 65 folded event relationships
// onto memory links, unifying the vocabulary on Link, and the two RPC surfaces are identical in
// shape (GET/POST /v1/{memories,events}/{id}/links, both returning GetLinksResponse). Cloning the
// card would have been two things to explain forever, and two places for the next fix to miss.
let linksSubject = { kind: "memory", id: "" };

// linksPath builds the collection path for whichever kind is open. The suffix is "" for the
// listing, "/delete" to unlink.
function linksPath(subject, suffix) {
  const collection = subject.kind === "event" ? "/v1/events/" : "/v1/memories/";

  return (
    collection + encodeURIComponent(subject.id) + "/links" + (suffix || "")
  );
}

async function openLinks(kind, id) {
  linksSubject = { kind, id };

  // innerHTML rather than textContent because the id is rendered through idCell (which escapes
  // it) so the heading carries the same truncation and copy button as the tables.
  $("links-subject").innerHTML = idCell(id);
  $("links-kind").textContent = kind === "event" ? "Event" : "Memory";
  $("links-card").classList.remove("hidden");
  $("links-list").innerHTML = '<div class="empty">Loading…</div>';
  $("link-target").placeholder =
    kind === "event" ? "event id to link to" : "memory id to link to";

  // Show the card on the tab the subject belongs to, so the row it was opened from is still on
  // screen behind it.
  document
    .querySelector(
      `nav button[data-tab="${kind === "event" ? "events" : "memories"}"]`,
    )
    .click();

  // The card sits below the list it is opened from. Below a full page of rows it is also off the
  // bottom of the screen, and reached from another tab there is nothing to say it opened at all.
  revealCard("links-card");

  try {
    renderLinks(await api("GET", linksPath(linksSubject)));
  } catch (e) {
    $("links-list").innerHTML =
      '<div class="empty">Could not load links.</div>';

    fail("Links failed", e);
  }
}

function closeLinks() {
  linksSubject = { kind: "memory", id: "" };
  $("links-card").classList.add("hidden");
}

function renderLinks(data) {
  const links = data.links || [];

  const isEvent = linksSubject.kind === "event";

  if (!links.length) {
    $("links-list").innerHTML =
      `<div class="empty">No links. This ${isEvent ? "event" : "memory"}
      decays on its own significance and recall history alone.</div>`;

    return;
  }

  // A link's direction is shown but does not change what it is worth: storage is directed so the
  // graph can be read either way, while value is symmetric.
  const rows = links
    .map(
      (l) => `<tr>
        <td class="id">${idCell(
          l.id,
          `<a class="evlink idv" data-act="follow-link" data-id="${esc(l.id)}"
             title="${esc(l.id)}">${esc(shortId(l.id))}</a>`,
        )}</td>
        <td>${esc(String(l.significance ?? 0))}</td>
        <td>${esc(l.direction === "LINK_DIRECTION_OUTBOUND" ? "→ outbound" : "← inbound")}</td>
        <td>${ageCell(l.created)}</td>
        <td class="actions-col">
          <div class="actions">
            <button class="btn small danger writer-only" data-act="unlink"
              data-id="${esc(linksSubject.id)}" data-target="${esc(l.id)}">Unlink</button>
          </div>
        </td>
      </tr>`,
    )
    .join("");

  $("links-list").innerHTML =
    `<div class="muted gap-bottom">Total link significance
    ${esc(String(data.linkSignificance ?? 0))} — damped before it counts toward the decay maths.</div>
    <div class="tablewrap"><table><thead><tr><th>${isEvent ? "Event" : "Memory"}</th><th>Significance</th><th>Direction</th><th>Created</th><th class="actions-col"></th></tr></thead>
    <tbody>${rows}</tbody></table></div>`;
}

async function addLink() {
  if (!linksSubject.id) return;

  const target = $("link-target").value.trim();

  if (!target) {
    fail("Link failed", `a target ${linksSubject.kind} id is required`);

    return;
  }

  const significance = Number($("link-sig").value || 0);

  try {
    await api("POST", linksPath(linksSubject), {
      links: [{ id: target, significance }],
    });

    ok("Linked", linksSubject.id + " → " + target);

    $("link-target").value = "";
    $("link-sig").value = "";

    openLinks(linksSubject.kind, linksSubject.id);
  } catch (e) {
    fail("Link failed", e);
  }
}

// unlink removes one edge from whichever subject the card is showing. It takes the ids rather than
// reading linksSubject so the button carries its own target, which is what keeps a row's action
// correct if the card is re-rendered underneath it.
async function unlink(id, target) {
  if (!confirm("Remove the link between " + id + " and " + target + "?"))
    return;

  try {
    await api("POST", linksPath({ kind: linksSubject.kind, id }, "/delete"), {
      ids: [target],
    });
    ok("Unlinked", id + " ✕ " + target);
    openLinks(linksSubject.kind, id);
  } catch (e) {
    fail("Unlink failed", e);
  }
}

async function deleteMemory(id) {
  if (!confirm("Delete memory " + id + "?")) return;

  try {
    await api("POST", "/v1/memories/delete", { ids: [id] });
    ok("Memory deleted", id);
    loadMemories();
  } catch (e) {
    fail("Delete failed", e);
  }
}

// ------------------------------------------------------------------ decay
// The console never computes a decayed value of its own. The maths lives in exactly one place - the
// service's consolidation code - and both the per-row values and the plotted curve are served from
// it (POST /v1/consolidation/explain), so what is shown here cannot drift from what the service
// will actually do. That is also why the curve is sampled server-side rather than drawn from the
// configuration: a second implementation is a second answer.

// EXPLAIN_MAX_IDS mirrors the server's per-call bound (explainMaxMemoryIds), so a page larger than
// it asks about what it can rather than having the whole call rejected.
const EXPLAIN_MAX_IDS = 200;

// decayValues holds the last valuation per memory id, so clicking a value can plot that memory's own
// curve without asking again.
const decayValues = new Map();

// decayAvailable goes false the first time the RPC is refused. That is a property of the deployment
// (a replica has no forgetting of its own to describe) or of the token, never of one row, so the
// column stops asking rather than failing once per render.
//
// whoami now says so up front (caps.consolidation), so applyCaps sets this before anything is
// rendered and a replica never makes the refused call at all. The first-refusal path stays as
// the backstop for the other reason it can be refused - the token, which whoami cannot answer
// for, since ExplainConsolidation is reader-tier but a scope check still applies per id.
let decayAvailable = true;

// decayInputs is the last set of decision inputs seen, from whichever call produced them - the
// per-page valuation or the Decay tab's own refresh. They are the same snapshot either way.
let decayInputs = null;

// decorateValues fills the value column of a rendered table. It is best-effort by design: the rows
// are already useful, and a deployment that will not answer must cost a dash rather than an error.
async function decorateValues(memories) {
  if (!decayAvailable) {
    markValuesUnavailable();

    return;
  }

  const ids = memories
    .map((m) => m.id)
    .filter(Boolean)
    .slice(0, EXPLAIN_MAX_IDS);

  if (!ids.length) return;

  try {
    const data = await api("POST", "/v1/consolidation/explain", {
      memory_ids: ids,
    });

    decayInputs = data;

    for (const valuation of data.valuations || []) {
      decayValues.set(valuation.id, valuation);
    }

    document.querySelectorAll("[data-value-for]").forEach((cell) => {
      const valuation = decayValues.get(cell.dataset.valueFor);

      if (valuation) cell.innerHTML = valueCell(valuation);
    });
  } catch (e) {
    decayAvailable = false;

    markValuesUnavailable();
  }
}

// markValuesUnavailable blanks the column when the server will not value memories for this caller.
function markValuesUnavailable() {
  document.querySelectorAll("[data-value-for]").forEach((cell) => {
    cell.innerHTML =
      '<span class="muted" title="this instance does not report consolidation values">—</span>';
  });
}

// valueCell renders one memory's standing: the computed value, then the single fact that decides
// its fate - already due, held by the retention floor, or how long it has left. The value is
// clickable because the obvious next question ("why, and what does that curve look like?") is one
// the Decay tab answers.
function valueCell(valuation) {
  const value = Number(valuation.value);
  const threshold = Number(valuation.threshold);

  let pill = '<span class="pill safe">safe</span>';

  if (valuation.retained) {
    pill = '<span class="pill held">held</span>';
  } else if (valuation.wouldConsolidate) {
    pill = '<span class="pill due">due now</span>';
  } else if (threshold > 0 && value < threshold * 1.5) {
    pill = '<span class="pill soon">near</span>';
  }

  const left =
    valuation.belowMinimumAge && !valuation.retained
      ? "min age"
      : humanDays(Number(valuation.daysUntilForgotten));

  return `<span class="val" data-act="plot-memory" data-id="${esc(valuation.id)}"
    title="value ${esc(num(value))} against a threshold of ${esc(num(threshold))}; forgotten in ${esc(left)}">${esc(num(value))}</span>
    ${pill}<div class="muted fs-11">${esc(left)}</div>`;
}

// METHOD_NAMES labels consolidation.method, so the tab says what shape is being applied rather than
// only its number. See docs/consolidation.md.
const METHOD_NAMES = {
  1: "power law",
  2: "linear (exponential factor)",
  3: "linear (logarithmic factor)",
  4: "exponential half-life",
  5: "logarithmic long tail",
  6: "sigmoid consolidation window",
};

// ==================================================================== NOW TAB
//
// The landing view. It answers, in the order a visitor reads: what is this store holding, when does
// it next forget, what did it forget last, and what has just gone.
//
// It computes nothing about decay. Every number is served - by GetConsolidationStatus (the
// schedule), ExplainConsolidation (the capacity figures) and GetForgottenMemories (the feed) - for
// the same reason the Decay tab does not do its own maths: a second implementation of the decay
// model in the page is a second implementation to drift.

// nowState holds the last response from each of the three sources, so a re-render (the countdown
// ticks once a second) does not need a re-fetch.
let nowState = { status: null, explain: null, forgotten: null };

// The poll timers. Kept apart because the three sources have completely different costs: the status
// RPC reads in-memory atomics, the forgotten log reads an indexed page, and the explain snapshot
// costs a UsedBytes plus a CountMemories - both full scans on the server drivers, which is why the
// service caches it and reports the TTL for us to pace by.
let nowTimers = [];
let nowTicker = null;
let nowBackoff = 1;

// NOW_POLL_SECONDS are the base intervals. explain is a floor, not the interval: the real one comes
// from the server's own snapshot_ttl_seconds, so if that cache is retuned this page follows without
// a change here.
const NOW_POLL_SECONDS = { status: 5, forgotten: 10, explainFloor: 10 };
const NOW_BACKOFF_CEILING = 60;

// startNowPolling begins the refresh loop, or restarts it if it was already running. Called when the
// Now tab becomes visible - from a nav click and from boot(), since Now is the tab the console opens
// on and would otherwise render empty on first paint, which is the one thing it exists to prevent.
function startNowPolling() {
  stopNowPolling();

  // The first load happens even in a hidden tab, so a console opened in the background (or restored
  // with the session) has content the moment it is looked at rather than a flash of empty panels.
  // What a hidden tab must not do is keep polling - scheduleNow declines to re-arm while hidden, so
  // this costs one round of fetches per page load rather than one every few seconds forever.
  loadNow();

  if (document.visibilityState !== "visible") return;

  // The countdown moves between polls, so it is redrawn locally once a second from the last status
  // response rather than by asking the server what time it is.
  nowTicker = setInterval(renderNow, 1000);
}

function stopNowPolling() {
  nowTimers.forEach(clearTimeout);
  nowTimers = [];

  if (nowTicker) {
    clearInterval(nowTicker);
    nowTicker = null;
  }
}

// nowVisible reports whether the loop should be running at all: the Now tab selected, and the
// window actually being looked at.
function nowVisible() {
  return (
    document.visibilityState === "visible" &&
    $("tab-now").classList.contains("active")
  );
}

document.addEventListener("visibilitychange", () => {
  if (nowVisible()) {
    startNowPolling();

    return;
  }

  stopNowPolling();
});

// schedule re-arms one source. Each source re-arms itself after its own fetch rather than all three
// sharing a timer, so a slow one cannot delay the others and a failing one backs off alone.
function scheduleNow(seconds, fn) {
  if (!nowVisible()) return;

  nowTimers.push(setTimeout(fn, seconds * 1000 * nowBackoff));
}

// loadNow fetches all three sources once and re-arms each. A failure doubles the backoff for every
// source up to a minute: a demo host restarting must not be hammered by every browser tab pointed
// at it, and the console recovers on its own when the service comes back.
async function loadNow() {
  let failed = false;

  try {
    nowState.status = await api("GET", "/v1/consolidation/status");
  } catch (e) {
    failed = true;
    nowState.status = null;
  }

  const status = nowState.status;

  // A replica refuses ExplainConsolidation, which is where the memory count comes from everywhere
  // else - so the headline would show a dash for the one figure a visitor most wants. Ask the
  // listing for its total instead, ONCE per page load rather than on the poll loop: it is a count
  // over the store with no cache behind it, and repeating it every few seconds is precisely the
  // per-poll scan the explain snapshot is cached to avoid.
  if (
    status &&
    !status.consolidationEnabled &&
    nowState.replicaCount === undefined
  ) {
    try {
      const page = await api("GET", "/v1/memories?limit=1");

      nowState.replicaCount = Number(page.totalCount || 0);
    } catch (e) {
      nowState.replicaCount = null;
    }
  }

  // A replica consolidates nothing, so the capacity snapshot and the forgotten log are both
  // refused or empty there. Skip them outright rather than issuing calls whose only outcome is a
  // FAILED_PRECONDITION.
  if (status && status.consolidationEnabled) {
    if (caps.consolidation) {
      try {
        nowState.explain = await api("POST", "/v1/consolidation/explain", {});
      } catch (e) {
        failed = true;
      }
    }

    // The forgotten log is admin-tier. A scoped admin may read it - it is scopeFilter, so they see
    // their own partition - which is why this checks the tier and not the unbound flag.
    if (caps.isAdmin && caps.tombstones) {
      try {
        nowState.forgotten = await api(
          "GET",
          "/v1/memories/forgotten?limit=" + NOW_FEED_ROWS,
        );
      } catch (e) {
        failed = true;
      }
    }
  }

  nowBackoff = failed ? Math.min(nowBackoff * 2, NOW_BACKOFF_CEILING / 5) : 1;

  renderNow();

  const explainEvery = Math.max(
    Number(status && status.snapshotTtlSeconds) || 0,
    NOW_POLL_SECONDS.explainFloor,
  );

  scheduleNow(NOW_POLL_SECONDS.status, loadNow);
  scheduleNow(explainEvery, () => {});
}

// NOW_FEED_ROWS is how much of the forgotten log the feed shows. Deliberately short: this is the
// "what just went" glance, and the full log with its filters and paging is on the Decay tab.
const NOW_FEED_ROWS = 12;

// renderNow redraws from nowState. Called once a second by the ticker as well as after each fetch,
// because the countdown has to move between polls - the alternative, polling every second, would
// put the whole store's status load on a display that changes by one digit.
function renderNow() {
  const status = nowState.status;

  if (!status) {
    $("now-headline").innerHTML =
      '<div class="empty">Waiting for the service…</div>';
    $("now-capacity").innerHTML = "";
    $("now-cycle").innerHTML = "";

    return;
  }

  renderNowHeadline(status);
  renderNowCapacity(status);
  renderNowCycle(status);
  renderNowForgotten();
  applyMeterWidths();
}

// applyMeterWidths sets each meter's fill through the CSSOM. A width is genuinely dynamic so it
// cannot be a class, but a style="" attribute in rendered HTML is blocked by the console's CSP
// (style-src without unsafe-inline) - silently, since a CSP violation is not an exception. Assigning
// el.style is not covered by that policy, so the value travels as a data attribute and is applied
// here.
function applyMeterWidths() {
  document.querySelectorAll("#tab-now [data-fill]").forEach((el) => {
    el.style.width = el.dataset.fill + "%";
  });
}

// renderNowHeadline is the 60-second payload: how much is held, what went last cycle, when the next
// one is. Everything else on the tab elaborates on these three.
function renderNowHeadline(status) {
  const explain = nowState.explain;
  const held = explain ? Number(explain.memoryCount || 0) : null;
  const countdown = countdownLabel(Date.now(), status);
  const last = status.lastCycle;

  // A replica is not a broken consolidator and must not read as one. Its store is forgotten by
  // whichever instance holds the single-consolidator lock, under THAT instance's configuration -
  // so there is no schedule here to show, and saying so is the answer.
  if (!status.consolidationEnabled) {
    const replicaHeld = nowState.replicaCount;

    $("now-headline").innerHTML = `<div class="stats">
      <div class="stat">
        <div class="k">Memories held</div>
        <div class="v">${esc(replicaHeld === null || replicaHeld === undefined ? "—" : replicaHeld.toLocaleString())}</div>
        <div class="n">in this store</div>
      </div>
      <div class="stat">
        <div class="k">Consolidation</div>
        <div class="v">elsewhere</div>
        <div class="n">this instance is a replica</div>
      </div>
    </div>
    <p class="muted gap-top">This instance serves reads and writes but runs no sleep cycle. Its
     store is consolidated by whichever instance holds the single-consolidator lock, under that
     instance's configuration — so the schedule and thresholds that decide what is forgotten here
     are not this one's to report.</p>`;

    return;
  }

  const forgottenLast = last
    ? Number(last.memoriesConsolidated || 0) + Number(last.memoriesEvicted || 0)
    : null;

  $("now-headline").innerHTML = `<div class="stats">
    <div class="stat">
      <div class="k">Memories held</div>
      <div class="v">${esc(held === null ? "—" : held.toLocaleString())}</div>
      <div class="n">right now</div>
    </div>
    <div class="stat">
      <div class="k">Forgotten last cycle</div>
      <div class="v">${esc(forgottenLast === null ? "—" : forgottenLast.toLocaleString())}</div>
      <div class="n">${esc(last ? cycleSummary(last) : "none since this instance started")}</div>
    </div>
    <div class="stat">
      <div class="k">Next cycle</div>
      <div class="v">${esc(countdown.text)}</div>
      <div class="n">${esc(nextCycleNote(status))}</div>
    </div>
  </div>
  ${countdownBar(status)}`;
}

// nextCycleNote qualifies the countdown. The WAL trigger is the part that would otherwise mislead:
// it does not reset the timer, so a cycle can run before the countdown reaches zero and the
// schedule is still correct afterwards.
function nextCycleNote(status) {
  const period = Number(status.periodSeconds || 0);

  if (period <= 0) {
    return (
      "driven by the Sleep RPC" +
      (status.walTriggerEnabled ? " or the write-ahead log" : "")
    );
  }

  const every = "every " + compactAge(period);

  return status.walTriggerEnabled
    ? every + ", or sooner if the write-ahead log fills"
    : every;
}

// countdownBar draws how far through the interval we are. Omitted where there is no interval, since
// a bar with nothing to measure would imply a schedule this instance does not have.
function countdownBar(status) {
  if (Number(status.periodSeconds || 0) <= 0) return "";

  const fraction = countdownFraction(Date.now(), status);

  return `<div class="meter gap-top" title="progress through the current sleep interval">
    <div class="meter-fill" data-fill="${(fraction * 100).toFixed(1)}"></div>
  </div>`;
}

// renderNowCapacity draws the axis that is actually binding. Pressure rides on the greater of the
// two utilisations, so showing both equally would bury the one that decides anything.
function renderNowCapacity(status) {
  const explain = nowState.explain;

  if (!status.consolidationEnabled || !explain) {
    $("now-capacity").innerHTML = "";

    return;
  }

  const pressure = Number(explain.capacityPressure || 0);
  const meter = capacityMeter(explain);

  if (!meter) {
    $("now-capacity").innerHTML =
      `<p class="muted gap-top">No capacity target is configured, so
     nothing is evicted to make room — memories are forgotten only as they decay below the
     threshold of ${esc(num(Number(explain.deletionThreshold || 0)))}.</p>`;

    return;
  }

  const percent = Math.round(meter.fraction * 100);

  $("now-capacity").innerHTML =
    `<div class="meter gap-top ${meter.fraction > 0.9 ? "meter-hot" : ""}"
     title="${esc(meter.used)} of ${esc(meter.limit)}">
    <div class="meter-fill" data-fill="${percent}"></div>
  </div>
  <p class="muted fs-12">${esc(meter.used)} of ${esc(meter.limit)} (${percent}% by
   ${esc(meter.axis)}) — capacity pressure ×${esc(num(pressure))}, which multiplies the deletion
   threshold to ${esc(num(Number(explain.deletionThreshold || 0)))} as the store fills.</p>`;
}

// renderNowCycle reports the last cycle. The two decay paths are shown separately because they are
// different failures when they are wrong: consolidation is the decay model working, eviction is the
// store being over its capacity target.
function renderNowCycle(status) {
  if (!status.consolidationEnabled) {
    $("now-cycle").innerHTML = "";

    return;
  }

  const last = status.lastCycle;

  if (!last) {
    $("now-cycle").innerHTML =
      `<div class="empty">No cycle has run since this instance
     started.${status.sleepInProgress ? " One is running now." : ""}</div>
     <p class="muted fs-12">This report is held in memory, so a restart clears it — anything the
      feed below shows was forgotten by an earlier run of this process.</p>`;

    return;
  }

  const failed = !last.success;

  return void ($("now-cycle").innerHTML = `<div class="stats">
    <div class="stat">
      <div class="k">Ran</div>
      <div class="v">${ageCell(last.startedAt)}</div>
      <div class="n">${esc(TRIGGER_LABELS[last.trigger] || last.trigger || "")}</div>
    </div>
    <div class="stat">
      <div class="k">Decayed away</div>
      <div class="v">${esc(Number(last.memoriesConsolidated || 0).toLocaleString())}</div>
      <div class="n">fell below the threshold</div>
    </div>
    <div class="stat">
      <div class="k">Evicted</div>
      <div class="v">${esc(Number(last.memoriesEvicted || 0).toLocaleString())}</div>
      <div class="n">${esc(formatBytes(Number(last.bytesFreed || 0)))} reclaimed</div>
    </div>
    <div class="stat">
      <div class="k">Took</div>
      <div class="v">${esc(Number(last.durationMs || 0).toLocaleString())} ms</div>
      <div class="n">${esc(Number(last.eventsConsolidated || 0) + Number(last.eventsEvicted || 0))} event(s) went too</div>
    </div>
  </div>
  ${failed ? `<p class="muted warn gap-top">That cycle failed: ${esc(last.failure)}. The counts above are what it managed before it did.</p>` : ""}`);
}

// renderNowForgotten draws the feed. Three empty states, because they call for different responses:
// not recording (turn it on), recording but nothing gone yet (wait), and no permission to see it.
function renderNowForgotten() {
  const card = $("now-forgotten-card");
  const status = nowState.status;

  if (!status || !status.consolidationEnabled) {
    card.classList.add("hidden");

    return;
  }

  card.classList.remove("hidden");

  const data = nowState.forgotten;

  if (!data) {
    $("now-forgotten").innerHTML = '<div class="empty">Not loaded.</div>';

    return;
  }

  if (!data.enabled) {
    $("now-forgotten").innerHTML =
      `<p class="muted warn">Nothing is being recorded, so this store
     cannot say what it has forgotten. Set <code class="inline">consolidation.tombstones.enabled</code>
     to keep a log.</p>`;

    return;
  }

  const records = data.memories || [];

  if (!records.length) {
    $("now-forgotten").innerHTML =
      '<div class="empty">Nothing forgotten yet.</div>';

    return;
  }

  const rows = records
    .map(
      (m) => `<tr data-seq="${esc(m.seq)}">
    <td>${ageCell(m.forgottenAt)}</td>
    <td class="id">${idCell(m.id)}</td>
    <td>${esc(num(Number(m.value)))}</td>
    <td>${esc(num(Number(m.threshold)))}</td>
    <td>${esc(m.recallCount || 0)}</td>
    <td>${m.rule === "FORGET_RULE_EVICTION" ? "over capacity" : "decayed"}</td>
  </tr>`,
    )
    .join("");

  $("now-forgotten").innerHTML = `<div class="tablewrap"><table>
    <!-- Value and threshold are separate columns rather than "value < threshold", because for an
     EVICTION the value is above the threshold and went anyway: the store was over its capacity
     target and something had to. Rendering a "<" between them would assert a comparison that is
     false on exactly the rows where the distinction matters most. -->
    <thead><tr><th>When</th><th>Id</th><th>Value</th><th>Threshold</th><th>Recalls</th><th>Why</th></tr></thead>
    <tbody>${rows}</tbody>
  </table></div>
  <p class="muted fs-12 gap-top">The ${esc(records.length)} most recent of
   ${esc(Number(data.total || 0).toLocaleString())} recorded.</p>`;
}

// runSleep triggers a cycle from the Now tab. The confirmation is not ceremonial: this deletes
// memories, and the dry run next door is the non-destructive way to ask the same question.
async function runSleep() {
  if (
    !confirm(
      "Run a consolidation cycle now?\n\nMemories below the threshold will be DELETED. " +
        'This is not a dry run — the Decay tab\'s "what would be forgotten now" answers the ' +
        "same question without deleting anything.",
    )
  ) {
    return;
  }

  try {
    await api("POST", "/v1/sleep", {});

    // sleepOnce is behind a singleflight, so this may have JOINED a cycle already running rather
    // than started one. Say "a cycle", never "your cycle".
    ok("A cycle completed", "");

    // Wait for it to actually finish before refreshing: the RPC returns when the cycle it joined
    // does, but a concurrent one may still be in flight.
    for (let i = 0; i < 10; i++) {
      const status = await api("GET", "/v1/consolidation/status");

      nowState.status = status;

      if (!status.sleepInProgress) break;

      await new Promise((r) => setTimeout(r, 500));
    }

    // Force the capacity snapshot out of band, ignoring the TTL pacing: a cycle has just changed
    // the numbers that snapshot describes, and a stale one is precisely the wrong thing to show at
    // that moment.
    nowBackoff = 1;
    await loadNow();
  } catch (e) {
    fail("Sleep failed", e);
  }
}

// loadDecay refreshes the Decay tab: the decision inputs, and the curve for the significance in the
// form. Both come from one call, since the curve is meaningless without the threshold it is drawn
// against.
async function loadDecay() {
  const body = {};
  const significance = Number($("d-sig").value);

  if (significance > 0) {
    body.curve = { significance };

    const days = Number($("d-days").value);

    if (days > 0) body.curve.max_age_days = days;
  }

  try {
    const data = await api("POST", "/v1/consolidation/explain", body);

    decayAvailable = true;
    decayInputs = data;

    renderDecayStatus(data);
    renderCurve(data);
  } catch (e) {
    decayAvailable = false;

    $("decay-status").innerHTML =
      '<div class="empty">This instance does not report consolidation values (a replica has no cycle of its own), or this token may not ask.</div>';
    $("decay-curve").innerHTML = '<div class="empty">No curve available.</div>';

    fail("Decay unavailable", e);
  }
}

// renderDecayStatus shows the numbers behind every decision the service is about to make. Capacity
// pressure leads because it is the one figure that explains a store suddenly forgetting faster
// without anything having been reconfigured.
function renderDecayStatus(data) {
  const pressure = Number(data.capacityPressure || 0);
  const threshold = Number(data.deletionThreshold || 0);
  const configured = pressure > 0 ? threshold / pressure : threshold;
  const usedBytes = Number(data.usedBytes || 0);
  const capacityBytes = Number(data.capacityBytes || 0);
  const memoryCount = Number(data.memoryCount || 0);
  const capacityMemories = Number(data.capacityMemories || 0);

  const bytesNote =
    capacityBytes > 0
      ? `${Math.round((usedBytes / capacityBytes) * 100)}% of ${formatBytes(capacityBytes)}`
      : "no byte capacity configured";

  const countNote =
    capacityMemories > 0
      ? `${Math.round((memoryCount / capacityMemories) * 100)}% of ${capacityMemories.toLocaleString()}`
      : "no row capacity configured";

  const floors = [];

  if (Number(data.minimumAgeInDays || 0) > 0)
    floors.push(data.minimumAgeInDays + " day minimum age");
  if (Number(data.minimumRetentionInDays || 0) > 0)
    floors.push(data.minimumRetentionInDays + " day retention");

  $("decay-status").innerHTML = `<div class="stats">
    <div class="stat">
      <div class="k">Capacity pressure</div>
      <div class="v">×${esc(num(pressure))}</div>
      <div class="n">multiplies the threshold as the store fills</div>
    </div>
    <div class="stat">
      <div class="k">Deletion threshold</div>
      <div class="v">${esc(num(threshold))}</div>
      <div class="n">${esc(num(configured))} configured, scaled by the pressure</div>
    </div>
    <div class="stat">
      <div class="k">Stored</div>
      <div class="v">${esc(formatBytes(usedBytes))}</div>
      <div class="n">${esc(bytesNote)}</div>
    </div>
    <div class="stat">
      <div class="k">Memories</div>
      <div class="v">${esc(memoryCount.toLocaleString())}</div>
      <div class="n">${esc(countNote)}</div>
    </div>
    <div class="stat">
      <div class="k">Algorithm</div>
      <div class="v">${esc(METHOD_NAMES[data.method] || "method " + data.method)}</div>
      <div class="n">aggressiveness ${esc(num(Number(data.aggressiveness)))} · ${esc(num(Number(data.unitsOfAgeInDays)))} day(s) per age unit</div>
    </div>
    <div class="stat">
      <div class="k">Floors</div>
      <div class="v">${esc(floors.length ? floors.length : "none")}</div>
      <div class="n">${esc(floors.length ? floors.join(" · ") : "nothing is protected by age alone")}</div>
    </div>
  </div>`;
}

function renderCurve(data) {
  const curve = data.curve;

  if (!curve || !(curve.points || []).length) {
    $("decay-curve").innerHTML =
      '<div class="empty">Enter a significance and plot to see how it decays.</div>';

    return;
  }

  const crossing = Number(curve.crossingAgeDays);

  const note =
    crossing >= 0
      ? `Significance ${esc(num(Number(curve.significance)))} crosses the threshold at <strong>${esc(humanDays(crossing))}</strong> of age.`
      : `Significance ${esc(num(Number(curve.significance)))} does not cross the threshold within the projected span.`;

  $("decay-curve").innerHTML =
    `<div class="chart">${curveSvg(curve, Number(data.deletionThreshold))}</div>
    <p class="muted">${note} Age is measured from the memory's creation, or from its most recent
    recall — recalling a memory puts it back at the start of this curve.</p>`;
}

// plotMemory answers the question a value in the table invites: it takes that memory's own effective
// significance - the number the decay actually acts on, not the stored significance - and plots the
// curve it is riding down.
function plotMemory(id) {
  const valuation = decayValues.get(id);

  if (!valuation) return;

  $("d-sig").value = Number(
    Number(valuation.effectiveSignificance).toPrecision(6),
  );
  $("d-days").value = "";

  showBreakdown(valuation);

  document.querySelector('nav button[data-tab="decay"]').click();

  loadDecay();
}

// showBreakdown accounts for the effective significance the curve is plotted against. It exists
// because links made that number stop being self-evident: a memory's links and its event's links are
// damped before they are weighted, so a link significance of 5000 contributes a little over eight,
// and an operator tuning consolidation.linkSignificanceWeight has no way to reconcile the total
// without seeing the parts. Everything shown is served - the console computes no decay maths of its
// own - so this cannot drift from what the sleep cycle will actually do.
function showBreakdown(valuation) {
  const el = $("d-breakdown");

  if (!el) return;

  const parts = [
    `stored significance ${esc(num(Number(valuation.significance)))}`,
  ];

  const linkSig = Number(valuation.linkSignificance || 0);
  const eventLinkSig = Number(valuation.eventLinkSignificance || 0);

  if (linkSig > 0) {
    parts.push(
      `links ${esc(num(linkSig))} &rarr; ${esc(num(Number(valuation.linkContribution || 0)))}`,
    );
  }

  if (eventLinkSig > 0) {
    parts.push(
      `event links ${esc(num(eventLinkSig))} &rarr; ${esc(num(Number(valuation.eventLinkContribution || 0)))}`,
    );
  }

  if (Number(valuation.recallCount) > 0) {
    parts.push(`${esc(String(valuation.recallCount))} recalls`);
  }

  el.innerHTML = `Effective significance ${esc(num(Number(valuation.effectiveSignificance)))}
    &mdash; ${parts.join(", ")}. Link totals are damped before weighting, so a large total adds
    little.`;
}

// runPreview asks what a cycle running now would forget. Admin only (the server refuses it below
// that tier), which is why the card carrying it is hidden for everyone else.
async function runPreview() {
  const params = new URLSearchParams();
  const limit = intOrUndef("p-limit");

  if (limit !== undefined) params.set("limit", limit);

  try {
    const data = await api("GET", "/v1/sleep/preview?" + params.toString());

    renderPreview(data);
    ok("Dry run", "nothing was deleted");
  } catch (e) {
    $("preview-results").innerHTML =
      '<div class="empty">The dry run was refused.</div>';

    fail("Dry run failed", e);
  }
}

function renderPreview(data) {
  const consolidated = Number(data.memoriesConsolidated || 0);
  const evicted = Number(data.memoriesEvicted || 0);
  const retained = Number(data.memoriesRetained || 0);
  const retainedBytes = Number(data.retainedBytes || 0);
  const capacityBytes = Number(data.capacityBytes || 0);

  // Retention overrides the capacity target, so a retained set approaching the capacity is why a
  // store can sit above its target indefinitely - the one failure mode a dry run exposes that
  // nothing else does.
  const retentionWarning =
    retained > 0 && capacityBytes > 0 && retainedBytes > capacityBytes * 0.8
      ? `<p class="muted warn">Retained memories hold ${esc(formatBytes(retainedBytes))}
       against a capacity of ${esc(formatBytes(capacityBytes))}: retention overrides the capacity
       target, so eviction cannot bring the store back under it.</p>`
      : "";

  const rows = (data.candidates || [])
    .map(
      (c) => `<tr>
    <td class="id">${idCell(c.id)}</td>
    <td>${esc(num(Number(c.value)))}</td>
    <td>${esc(c.significance ?? "")}</td>
    <td class="id">${c.eventId ? idCell(c.eventId) : "—"}</td>
    <td>${esc(c.group || "—")}</td>
    <td>${c.rule === "FORGET_RULE_EVICTION" ? "over capacity" : "decayed"}</td>
  </tr>`,
    )
    .join("");

  const table = rows
    ? `<div class="tablewrap"><table>
        <thead><tr><th>Id</th><th>Value</th><th>Sig</th><th>Event</th><th>Group</th><th>Why</th></tr></thead>
        <tbody>${rows}</tbody>
      </table></div>
      ${data.truncated ? '<p class="muted">Sample truncated — raise the limit to see more.</p>' : ""}`
    : '<div class="empty">Nothing would be forgotten right now.</div>';

  $("preview-results").innerHTML = `<div class="stats gap-top">
    <div class="stat">
      <div class="k">Would be forgotten</div>
      <div class="v">${esc((consolidated + evicted).toLocaleString())}</div>
      <div class="n">${esc(consolidated.toLocaleString())} decayed · ${esc(evicted.toLocaleString())} over capacity</div>
    </div>
    <div class="stat">
      <div class="k">Events removed</div>
      <div class="v">${esc(Number(data.eventsDeleted || 0).toLocaleString())}</div>
      <div class="n">those left holding no memories</div>
    </div>
    <div class="stat">
      <div class="k">Reclaimed</div>
      <div class="v">${esc(formatBytes(data.bytesFreed))}</div>
      <div class="n">estimated, on the same basis as used bytes</div>
    </div>
    <div class="stat">
      <div class="k">Held by retention</div>
      <div class="v">${esc(retained.toLocaleString())}</div>
      <div class="n">${esc(formatBytes(retainedBytes))} that cannot be forgotten yet</div>
    </div>
  </div>
  ${retentionWarning}
  ${table}`;
}

// ------------------------------------------------------------------ forgotten log
// The dry run above says what would go; this says what did. It is the only view in the console
// that can speak about a memory that no longer exists, which is also why it is the only one whose
// rows cannot be clicked through to the record they name.
// The forgotten log pages by KEYSET, not offset: GetForgottenMemories takes after_seq ("records
// below this seq") and returns next_seq, 0 on the last page. That is the right shape for a log
// being appended to while you read it - an offset would shift under you every time a cycle ran.
//
// Keyset paging has no cheap "previous", so forgottenCursors is a stack of the cursors we came
// from: Next pushes, Prev pops. Being explicit about that is the honest way to offer a back button
// without silently falling back to offsets.
let forgottenCursors = [];
let forgottenNext = 0;

async function loadForgotten(cursor) {
  const params = new URLSearchParams();
  const set = (key, value) => {
    if (value !== undefined && value !== "") params.set(key, value);
  };

  set("memoryId", $("f-memory-id").value.trim());
  set("group", $("f-group").value.trim());
  set("rule", $("f-rule").value);
  set("limit", intOrUndef("f-limit"));
  set("afterSeq", cursor || undefined);

  try {
    const data = await api(
      "GET",
      "/v1/memories/forgotten?" + params.toString(),
    );

    forgottenNext = Number(data.nextSeq || 0);

    renderForgotten(data);
  } catch (e) {
    $("forgotten-results").innerHTML =
      '<div class="empty">The forgotten log could not be read.</div>';

    fail("Forgotten log", e);
  }
}

// A new filter run starts a new sequence, so the stack it would page back through no longer applies.
function loadForgottenFirstPage() {
  forgottenCursors = [];

  return loadForgotten(0);
}

function forgottenPage(direction) {
  if (direction > 0) {
    if (!forgottenNext) return;

    // Push the cursor that produced the page we are leaving, so Prev can return to it.
    forgottenCursors.push(forgottenNext);

    return loadForgotten(forgottenNext);
  }

  if (!forgottenCursors.length) return;

  // The top of the stack is the cursor that produced the CURRENT page; discard it, and the one
  // beneath produced the page before.
  forgottenCursors.pop();

  return loadForgotten(forgottenCursors[forgottenCursors.length - 1] || 0);
}

function renderForgotten(data) {
  const records = data.memories || [];

  // An empty log is ambiguous - nothing forgotten, or nothing written down - and the enabled
  // flag is the only thing that tells them apart. Say which.
  const disabled = data.enabled
    ? ""
    : `<p class="muted warn">The forgotten log is not enabled, so nothing new is being
       recorded. Set <code class="inline">consolidation.tombstones.enabled</code> to start keeping
       one; what is already here stays until it is cleared.</p>`;

  if (!records.length) {
    $("forgotten-results").innerHTML =
      disabled + '<div class="empty">Nothing has been forgotten.</div>';

    return;
  }

  const rows = records
    .map(
      (m) => `<tr>
    <td class="id">${esc(m.id)}</td>
    <td>${ageCell(m.forgottenAt)}</td>
    <td>${esc(num(Number(m.value)))}</td>
    <td>${esc(num(Number(m.threshold)))}</td>
    <td>${esc(m.significance ?? "")}</td>
    <td>${esc(m.eventId || "—")}</td>
    <td>${esc(m.group || "—")}</td>
    <td>${m.rule === "FORGET_RULE_EVICTION" ? "over capacity" : "decayed"}</td>
  </tr>`,
    )
    .join("");

  const page = forgottenCursors.length + 1;

  $("forgotten-results").innerHTML =
    disabled +
    `<p class="muted gap-top">Showing ${esc(records.length.toLocaleString())} of
     ${esc(Number(data.total || 0).toLocaleString())} record(s), most recent first — page
     ${esc(page)}.</p>
   <div class="tablewrap"><table>
     <thead><tr><th>Id</th><th>Forgotten</th><th>Value</th><th>Threshold</th><th>Sig</th><th>Event</th><th>Group</th><th>Why</th></tr></thead>
     <tbody>${rows}</tbody>
   </table></div>
   <div class="actions gap-top">
     <button class="btn ghost small" data-act="forgotten-prev"
       ${forgottenCursors.length ? "" : "disabled"}>Prev</button>
     <button class="btn ghost small" data-act="forgotten-next"
       ${Number(data.nextSeq || 0) ? "" : "disabled"}>Next</button>
   </div>`;
}

// clearForgotten empties the log. It is the one action here that destroys a record of something
// already destroyed, so it confirms - the caps configured on the service never do this.
async function clearForgotten() {
  if (
    !confirm(
      "Delete every record in the forgotten log? The memories are already gone; this removes the record that they existed.",
    )
  ) {
    return;
  }

  try {
    const data = await api("POST", "/v1/memories/forgotten/delete", {
      all: true,
    });

    ok("Forgotten log cleared", `${data.deleted || 0} record(s) deleted`);
    loadForgottenFirstPage();
  } catch (e) {
    fail("Clearing the forgotten log failed", e);
  }
}

// ------------------------------------------------------------------ events
function eventRow(e) {
  // Served, never derived: fetchEvents asks for memory_counts, so the count is the store's
  // own and is right whether or not the memories themselves were transferred. Falling back to
  // the attached memories keeps the column honest if a future caller asks for those alone.
  const memCount =
    Number(e.memoryCount || 0) || (e.memories && e.memories.length) || 0;

  // The name is a link: clicking it opens the event and lists all of its memories (openEvent).
  const nameCell = e.id
    ? `<a class="evlink" data-act="open-event" data-event="${esc(e.id)}">${esc(e.name || "(unnamed)")}</a>`
    : esc(e.name || "—");

  // The description is the event's free text, so it takes a row of its own beneath the metadata
  // for the same reason a memory's body does.
  const description = e.description ? String(e.description) : "";

  return `<tr${description ? ' class="has-body"' : ""}>
    <td>${nameCell}</td>
    <td>${esc(e.significance ?? "")}</td>
    <td>${groupCell(e)}</td>
    <td>${ageCell(e.timeStart)}</td>
    <td>${e.timeEnd && e.timeEnd !== "0" ? ageCell(e.timeEnd) : '<span class="pill">open</span>'}</td>
    <td>${memCount || "—"}</td>
    <td class="id">${idCell(e.id)}</td>
    <td class="actions-col">
      <div class="actions">
        <button class="btn small ghost writer-only" data-act="edit-event" data-id="${esc(e.id)}">Edit</button>
        <button class="btn small ghost" data-act="event-links" data-id="${esc(e.id)}">Links</button>
        <button class="btn small ghost writer-only" data-act="end-event" data-id="${esc(e.id)}">End</button>
        <button class="btn small danger writer-only" data-act="delete-event" data-id="${esc(e.id)}">Delete</button>
      </div>
    </td>
  </tr>${
    description
      ? `<tr><td class="bodycell" colspan="8"><div class="bodytext">${esc(description)}</div></td></tr>`
      : ""
  }`;
}

function renderEvents(events) {
  if (!events.length) {
    $("events-list").innerHTML = '<div class="empty">No events.</div>';

    return;
  }

  // Register the displayed rows so the delegated edit handler can resolve the full object by id
  // instead of it being embedded in the markup.
  events.forEach((e) => evRegistry.set(e.id, e));

  const rows = events.map(eventRow).join("");
  $("events-list").innerHTML = `<div class="tablewrap"><table>
    <thead><tr>
      ${sortHeader("events", "name", "Name")}
      ${sortHeader("events", "significance", "Sig")}
      ${sortHeader("events", "group", "Group")}
      ${sortHeader("events", "timestamp", "Start")}
      ${sortHeader("events", "time_end", "End", "an event that has not ended sorts as the oldest-ended, not as the most recent")}
      <th title="memories this event currently holds">Mem</th>
      ${sortHeader("events", "id", "Id")}
      <th class="actions-col"></th>
    </tr></thead>
    <tbody>${rows}</tbody>
  </table></div>`;
}

// Offset of the currently displayed events page. loadEvents() restarts from the top; eventsPage()
// steps by one page. Both funnel through fetchEvents(), which reads sort + page size from the form.
let eventsOffset = 0;

function eventsPageSize() {
  return intOrUndef("ef-pagesize") || 25;
}

// ---------------------------------------------------------- summarisation candidates
//
// The list the sleep cycle's scan produced: events holding enough quiet, unsummarised memories
// to be worth condensing. It is a point-in-time snapshot refreshed by the cycle, not a live
// query, which is why this loads on demand and offers a Refresh rather than following the
// events list.
async function loadCandidates() {
  $("candidates-list").innerHTML = '<div class="empty">Loading…</div>';

  try {
    const data = await api("GET", "/v1/summarisation/candidates");

    renderCandidates(data);
    ok("Candidates", (data.candidates || []).length + " event(s)");
  } catch (e) {
    $("candidates-list").innerHTML =
      '<div class="empty">Could not load candidates.</div>';

    fail("Candidates failed", e);
  }
}

function renderCandidates(data) {
  const candidates = data.candidates || [];

  if (!candidates.length) {
    // An empty list means one of two opposite things, and only the server can say which:
    // nothing is due yet, or this instance does not scan at all (the threshold is unset, or it
    // is a replica and runs no sleep cycle). Reading it off the list would guess.
    $("candidates-list").innerHTML = data.scanEnabled
      ? '<div class="empty">No candidates right now — nothing has accumulated enough quiet memories yet.</div>'
      : '<div class="empty">This instance does not scan for candidates. It needs ' +
        '<code class="inline">consolidation.summarisationMinMemories</code> above zero and ' +
        '<code class="inline">consolidation.enabled</code> — a replica runs no sleep cycle, ' +
        "so it never produces this list.</div>";

    return;
  }

  // The event name opens the event, which is where the summarise buttons are — the list says
  // what is worth condensing, the event view is where it is done.
  const rows = candidates
    .map(
      (c) => `<tr>
        <td><a class="evlink" data-act="open-event" data-event="${esc(c.eventId)}">${esc(c.eventName || "(unnamed)")}</a></td>
        <td>${esc(String(c.memoryCount ?? 0))}</td>
        <td class="id">${idCell(c.eventId)}</td>
      </tr>`,
    )
    .join("");

  $("candidates-list").innerHTML = `<div class="tablewrap"><table>
    <thead><tr><th>Event</th>
      <th title="unsummarised memories the scan counted">Mem</th><th>Id</th></tr></thead>
    <tbody>${rows}</tbody></table></div>`;
}

// loadEvents lists from the first page — used by the "List events" button and refreshed after a
// mutation so the user lands back at the top of the (possibly re-sorted) list.
async function loadEvents() {
  eventsOffset = 0;

  await fetchEvents();
}

async function eventsPage(dir) {
  const next = eventsOffset + dir * eventsPageSize();

  eventsOffset = next < 0 ? 0 : next;

  await fetchEvents();
}

// syncEventExtremum greys out the significance min/max inputs while an extremum (highest/lowest) is
// selected: the server rejects significance_extremum combined with significance_min/max, so the two
// are mutually exclusive in the form too.
function syncEventExtremum() {
  const on = $("ef-extremum").value !== "";

  ["ef-sigmin", "ef-sigmax"].forEach((id) => {
    $(id).disabled = on;

    if (on) $(id).value = "";
  });
}

async function fetchEvents() {
  const params = new URLSearchParams();
  const extremum = $("ef-extremum").value;

  // significance_extremum ("only the events tied at the highest/lowest significance") replaces the
  // min/max range rather than combining with it — sending both is an InvalidArgument.
  if (extremum) {
    params.set("significance_extremum", extremum);
  } else {
    const sigMin = intOrUndef("ef-sigmin");
    const sigMax = intOrUndef("ef-sigmax");

    if (sigMin !== undefined) params.set("significance_min", sigMin);
    if (sigMax !== undefined) params.set("significance_max", sigMax);
  }

  const group = strOrUndef("ef-group");
  if (group) params.set("group", group);

  // One parameter per pair (append, not set), exactly as the memories filter does: the filter is a
  // conjunction and the gateway parses a repeated field as repeated parameters.
  parseMetadataPairs($("ef-metadata").value, ",").forEach((pair) =>
    params.append("metadata", pair),
  );

  if ($("ef-memories").checked) params.set("memories", "true");

  // Always asked for: how much an event holds is what a listing is for, and the count is one
  // aggregate query that reads no bodies — unlike `memories`, which transfers all of them.
  params.set("memory_counts", "true");

  params.set("order_by", $("ef-sort").value);

  // Omitted when "Default" — see fetchMemories for why.
  const sortDir = $("ef-sortdir").value;
  if (sortDir) params.set("order_dir", sortDir);

  params.set("limit", eventsPageSize());
  params.set("offset", eventsOffset);

  try {
    const data = await api("GET", "/v1/events?" + params.toString());
    const events = data.events || [];
    const total = Number(data.totalCount || 0);

    renderEvents(events);
    updateEventsPager(events.length, total);
    ok("Events", events.length + " of " + total + " shown");
  } catch (e) {
    fail("Load events failed", e);
  }
}

function updateEventsPager(count, total) {
  const from = total === 0 ? 0 : eventsOffset + 1;
  const to = eventsOffset + count;

  $("ev-range").textContent =
    total === 0 ? "no events" : from + "–" + to + " of " + total;
  $("ev-prev").disabled = eventsOffset <= 0;
  $("ev-next").disabled = to >= total;
}

function clearEventFilter() {
  ["ef-sigmin", "ef-sigmax", "ef-group", "ef-metadata"].forEach(
    (id) => ($(id).value = ""),
  );
  $("ef-extremum").value = "";
  $("ef-memories").checked = false;
  $("ef-sort").value = "significance";
  $("ef-sortdir").value = "";
  syncEventExtremum();
}

async function saveEvent() {
  const id = $("ev-id").value;

  if (id) {
    // The gateway only exposes significance and end-time updates for an existing event.
    const sig = intOrUndef("ev-sig");

    try {
      if (sig !== undefined) {
        await api(
          "PATCH",
          "/v1/events/" + encodeURIComponent(id) + "/significance",
          { id, significance: sig },
        );
      }

      const end = localToNano($("ev-end").value);

      if (end) {
        await api("POST", "/v1/events/" + encodeURIComponent(id) + "/end", {
          id,
          time_end: end,
        });
      }

      ok("Event updated", id);
      resetEventForm();
      loadEvents();
    } catch (e) {
      fail("Update event failed", e);
    }

    return;
  }

  const body = {
    name: $("ev-name").value,
    description: $("ev-desc").value,
    significance: intOrUndef("ev-sig"),
    group: strOrUndef("ev-group"),
    metadata: metadataFromForm("ev-metadata"),
    time_start: localToNano($("ev-start").value),
    time_end: localToNano($("ev-end").value),
  };

  try {
    const res = await api("POST", "/v1/events", body);

    if (res.rejected) {
      toast(
        "Event rejected",
        "significance below the configured minimum — not stored",
        "err",
      );
    } else {
      ok(
        "Event created",
        res.id + " (" + (res.memoryCount || 0) + " memories)",
      );
    }

    resetEventForm();
    loadEvents();
  } catch (e) {
    fail("Save event failed", e);
  }
}

function editEvent(e) {
  $("ev-id").value = e.id;
  $("ev-name").value = e.name || "";
  $("ev-desc").value = e.description || "";
  $("ev-sig").value = e.significance ?? "";
  $("ev-group").value = e.group || "";
  $("ev-metadata").value = metadataToForm(e.metadata);
  $("ev-start").value = nanoToLocal(e.timeStart);
  $("ev-end").value = nanoToLocal(e.timeEnd);
  $("ev-form-title").textContent = "Edit event";
  $("ev-save").textContent = "Update";
  const hint = $("ev-edit-hint");
  hint.classList.remove("hidden");
  hint.textContent = "Only significance and end-time are updatable via the API";
  document.querySelector('nav button[data-tab="events"]').click();
  window.scrollTo({ top: 0, behavior: scrollBehaviour() });
}

function resetEventForm() {
  [
    "ev-id",
    "ev-name",
    "ev-desc",
    "ev-sig",
    "ev-group",
    "ev-metadata",
    "ev-start",
    "ev-end",
  ].forEach((id) => ($(id).value = ""));
  $("ev-form-title").textContent = "Create event";
  $("ev-save").textContent = "Create";
  $("ev-edit-hint").classList.add("hidden");
}

async function endEvent(id) {
  if (!confirm("End event " + id + " now?")) return;

  try {
    await api("POST", "/v1/events/" + encodeURIComponent(id) + "/end", {
      id,
      time_end: String(BigInt(Date.now()) * 1000000n),
    });
    ok("Event ended", id);
    loadEvents();
  } catch (e) {
    fail("End event failed", e);
  }
}

async function deleteEvent(id) {
  // confirm() is binary, so ask in two steps: first whether to also delete the memories, then
  // (if not) whether to go ahead detaching them, which also gives a way to abort entirely.
  const withMemories = confirm(
    "Delete event " +
      id +
      "?\n\nOK  = delete the event AND its memories.\nCancel = keep the memories (you'll confirm next).",
  );

  if (!withMemories) {
    if (
      !confirm(
        "Delete event " + id + ", detaching its memories (they are kept)?",
      )
    )
      return;
  }

  try {
    await api(
      "DELETE",
      "/v1/events/" +
        encodeURIComponent(id) +
        "?memories=" +
        (withMemories ? "true" : "false"),
    );
    ok("Event deleted", id);
    loadEvents();
  } catch (e) {
    fail("Delete event failed", e);
  }
}
