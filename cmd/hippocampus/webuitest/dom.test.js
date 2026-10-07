// The console's DOM layer, driven for real (TODO-3 item 177).
//
// lib.test.js covers the pure logic and actions.test.js holds the controls to their handlers as
// text; neither runs a line of app.js. These tests load the actual file against the fake DOM in
// dom.js and check the wiring between the two halves: that what lib.js decides is what lands on the
// page, that the network layer sends what the server expects and turns its refusals into words, and
// that a click reaches its handler.

import test from "node:test";
import assert from "node:assert/strict";

import { installDom, loadApp, removeLoaded, settle, teardown } from "./dom.js";

test.afterEach(teardown);
test.after(removeLoaded);

// whoAmI is the response the console's capability refresh reads.
function whoAmI(fields) {
  return () => ({ body: { authEnabled: false, version: "v0.52.0", ...fields } });
}

test("an unauthenticated deployment opens the console, ungated, with the build shown", async () => {
  const dom = installDom({
    routes: {
      "/ui/config": () => ({ body: { authMethod: "none" } }),
      "/v1/whoami": whoAmI({ consolidationEnabled: true }),
    },
  });

  await loadApp();

  assert.equal(dom.document.body.classList.contains("gated"), false, "a deployment with no auth must not be gated");
  assert.equal(dom.document.body.classList.contains("consolidating"), true, "the consolidating class follows whoami");
  assert.equal(dom.byId.get("version-pill").textContent, "v0.52.0");
  assert.equal(dom.byId.get("version-pill").classList.contains("hidden"), false);
  assert.equal(dom.byId.get("role-pill").classList.contains("hidden"), true, "no role pill without authentication");
});

test("an hmac deployment with no token raises the sign-in gate with the token form", async () => {
  const dom = installDom({
    routes: {
      "/ui/config": () => ({ body: { authMethod: "hmac" } }),
      "/v1/whoami": () => ({ status: 401, body: { message: "missing authorization metadata" } }),
    },
  });

  await loadApp();

  assert.equal(dom.document.body.classList.contains("gated"), true, "no credential on an authenticating deployment must gate");
  assert.equal(dom.byId.get("gate-title").textContent, "Sign in");
  assert.equal(dom.byId.get("gate-form").classList.contains("hidden"), false, "the manual token form is the way in");
  assert.equal(dom.byId.get("gate-oidc").classList.contains("hidden"), true);
});

test("a reader is shown its role and loses the write controls", async () => {
  const dom = installDom({
    routes: {
      "/ui/config": () => ({ body: { authMethod: "hmac" } }),
      "/v1/whoami": whoAmI({ authEnabled: true, role: "reader" }),
    },
    local: { hippocampus_token: "a-token" },
  });

  await loadApp();

  assert.equal(dom.document.body.classList.contains("gated"), false);
  assert.equal(dom.document.body.classList.contains("readonly"), true, "a reader's body must carry the readonly class");
  assert.equal(dom.byId.get("role-pill").textContent, "role: reader");
  assert.equal(dom.byId.get("role-pill").classList.contains("ro"), true);
  assert.equal(dom.byId.get("logout-btn").classList.contains("hidden"), false, "a signed-in caller can sign out");
});

test("api sends the stored token and the console's version, and parses the reply", async () => {
  const dom = installDom({
    routes: {
      "/ui/config": () => ({ body: { authMethod: "hmac" } }),
      "/v1/whoami": whoAmI({ authEnabled: true, role: "writer" }),
      "/v1/memories": () => ({ body: { memories: [], totalCount: 0 } }),
    },
    local: { hippocampus_token: "t0ken" },
  });

  const app = await loadApp();

  const res = await app.api("GET", "/v1/memories");

  assert.deepEqual(res, { memories: [], totalCount: 0 });

  const sent = dom.requests.find((r) => r.path === "/v1/memories");

  assert.equal(sent.init.headers.Authorization, "Bearer t0ken");
  assert.equal(sent.init.headers["Hippocampus-Client-Version"], "hippocampus-console/v0.52.0");
});

test("api turns refusals into words an operator can act on", async () => {
  installDom({
    routes: {
      "/ui/config": () => ({ body: { authMethod: "none" } }),
      "/v1/whoami": whoAmI({}),
      "/v1/forbidden": () => ({ status: 403, body: { message: "insufficient role" } }),
      "/v1/limited": () => ({ status: 429, body: { message: "rate limit exceeded", scope: "client" }, headers: { "Retry-After": "5" } }),
      "/v1/broken": () => ({ status: 500, body: { message: "internal error" } }),
    },
  });

  const app = await loadApp();

  await assert.rejects(app.api("GET", "/v1/forbidden"), (e) => {
    assert.equal(e.status, 403);
    assert.equal(e.message, "Forbidden — your token's role does not permit this action.");

    return true;
  });

  await assert.rejects(app.api("GET", "/v1/limited"), (e) => {
    assert.equal(e.message, "Rate limited (client limit) — try again in 5s.");

    return true;
  });

  await assert.rejects(app.api("GET", "/v1/broken"), (e) => {
    assert.equal(e.message, "internal error", "other failures keep the server's own message");

    return true;
  });
});

test("a toast renders its text as text, never as markup", async () => {
  const dom = installDom({
    routes: { "/ui/config": () => ({ body: { authMethod: "none" } }), "/v1/whoami": whoAmI({}) },
  });

  const app = await loadApp();

  app.toast("<img src=x onerror=alert(1)>", "body <b>text</b>", "err");

  const toast = dom.byId.get("toast").children.at(-1);

  assert.equal(toast.classList.contains("err"), true);
  assert.equal(toast.querySelector(".t-title").textContent, "<img src=x onerror=alert(1)>");
  assert.equal(toast.querySelector(".t-body").textContent, "body <b>text</b>");
  assert.doesNotMatch(toast.innerHTML, /onerror/, "the title must not reach innerHTML");
});

test("a click on a data-act control reaches its handler through the delegated listener", async () => {
  const dom = installDom({
    routes: { "/ui/config": () => ({ body: { authMethod: "none" } }), "/v1/whoami": whoAmI({}) },
  });

  await loadApp();

  const button = dom.document.createElement("button");

  button.setAttribute("data-act", "copy");
  button.setAttribute("data-copy", "memory-42");

  // A click on something INSIDE the control, as an icon inside a button is: the dispatcher must
  // find the control by walking up.
  const icon = dom.document.createElement("svg");

  button.appendChild(icon);

  for (const listener of dom.document.listeners.click) {
    listener({ target: icon, preventDefault() {} });
  }

  await settle();

  assert.deepEqual(dom.clipboard, ["memory-42"], "the copy action wrote the id to the clipboard");
  assert.equal(button.classList.contains("copied"), true, "the control that asked shows it worked");
});
