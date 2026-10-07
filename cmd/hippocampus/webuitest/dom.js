// A fake DOM, just large enough to load the console's app.js under node and drive its real code
// (TODO-3 item 177). It exists because app.js - the DOM, the network and the state - had no test at
// all: lib.js runs under node because it is pure, and app.js was "the half that cannot be loaded".
//
// It is deliberately small and permissive rather than a browser. Elements remember what is done to
// them (classes, text, attributes, children, listeners) so a test can assert on it; anything a test
// does not care about is absorbed. There is still no dependency: a real DOM library would be the
// one thing in this directory that needs installing and auditing.

import { readFileSync, writeFileSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const webui = join(dirname(fileURLToPath(import.meta.url)), "..", "webui");

class FakeClassList {
  constructor() {
    this.set = new Set();
  }

  add(...names) {
    for (const n of names) this.set.add(n);
  }

  remove(...names) {
    for (const n of names) this.set.delete(n);
  }

  contains(name) {
    return this.set.has(name);
  }

  toggle(name, force) {
    const on = force === undefined ? !this.set.has(name) : !!force;

    if (on) this.set.add(name);
    else this.set.delete(name);

    return on;
  }
}

export class FakeElement {
  constructor(tagName, id) {
    this.tagName = (tagName || "div").toUpperCase();
    this.id = id || "";
    this.classList = new FakeClassList();
    this.dataset = {};
    this.style = {};
    this.attributes = new Map();
    this.children = [];
    this.parentNode = null;
    this.listeners = {};
    this.textContent = "";
    this.innerHTML = "";
    this.value = "";
    this.disabled = false;
    this.selectors = new Map();
  }

  get className() {
    return [...this.classList.set].join(" ");
  }

  set className(value) {
    this.classList = new FakeClassList();
    this.classList.add(...String(value).split(/\s+/).filter(Boolean));
  }

  setAttribute(name, value) {
    this.attributes.set(name, String(value));

    if (name.startsWith("data-")) {
      const key = name.slice(5).replace(/-([a-z])/g, (_, c) => c.toUpperCase());

      this.dataset[key] = String(value);
    }
  }

  getAttribute(name) {
    return this.attributes.has(name) ? this.attributes.get(name) : null;
  }

  hasAttribute(name) {
    return this.attributes.has(name);
  }

  removeAttribute(name) {
    this.attributes.delete(name);
  }

  addEventListener(type, fn) {
    (this.listeners[type] ||= []).push(fn);
  }

  removeEventListener() {}

  appendChild(child) {
    child.parentNode = this;
    this.children.push(child);

    return child;
  }

  append(...children) {
    for (const c of children) {
      if (c instanceof FakeElement) this.appendChild(c);
    }
  }

  remove() {
    if (!this.parentNode) return;

    this.parentNode.children = this.parentNode.children.filter((c) => c !== this);
    this.parentNode = null;
  }

  // querySelector answers with one remembered element per selector, so code that writes to
  // el.querySelector(".t-title") and a test that reads it back see the same element.
  querySelector(selector) {
    if (!this.selectors.has(selector)) this.selectors.set(selector, new FakeElement("div"));

    return this.selectors.get(selector);
  }

  querySelectorAll() {
    return [];
  }

  // closest supports the one shape the dispatcher asks: "[data-attribute]".
  closest(selector) {
    const attr = /^\[([^\]=]+)\]$/.exec(selector);

    for (let el = this; el; el = el.parentNode) {
      if (attr && el.hasAttribute(attr[1])) return el;
    }

    return null;
  }

  removeChild(child) {
    child.remove();

    return child;
  }

  select() {}
  insertAdjacentHTML() {}
  focus() {}
  blur() {}
  scrollIntoView() {}
  contains() {
    return false;
  }

  getClientRects() {
    return [];
  }

  getBoundingClientRect() {
    return { top: 0, left: 0, right: 0, bottom: 0, width: 0, height: 0 };
  }
}

class FakeStorage {
  constructor(initial) {
    this.map = new Map(Object.entries(initial || {}));
  }

  getItem(k) {
    return this.map.has(k) ? this.map.get(k) : null;
  }

  setItem(k, v) {
    this.map.set(k, String(v));
  }

  removeItem(k) {
    this.map.delete(k);
  }
}

// installDom puts a fresh fake browser on globalThis and returns its handles. routes maps a request
// path to a function returning {status, body, headers}; an unrouted request answers 404, so a test
// sees what the code asked for rather than hanging.
// Timers app.js schedules are tracked so a test can clear them: the console polls and animates, and
// one live interval keeps node running after every test has finished.
const timers = new Set();
const realSetTimeout = globalThis.setTimeout;
const realSetInterval = globalThis.setInterval;
const realClearTimeout = globalThis.clearTimeout;
const realClearInterval = globalThis.clearInterval;

// removeLoaded deletes the directory loadApp writes its copies of app.js into. Call it once, after
// every test.
export function removeLoaded() {
  if (dir) rmSync(dir, { recursive: true, force: true });

  dir = null;
}

// teardown clears every timer the loaded console scheduled. Call it after each test.
export function teardown() {
  for (const handle of timers) {
    realClearTimeout(handle);
    realClearInterval(handle);
  }

  timers.clear();
}

export function installDom({ routes = {}, local = {}, session = {} } = {}) {
  const byId = new Map();
  const requests = [];
  const clipboard = [];

  const document = {
    body: new FakeElement("body"),
    documentElement: new FakeElement("html"),
    listeners: {},
    getElementById(id) {
      if (!byId.has(id)) byId.set(id, new FakeElement("div", id));

      return byId.get(id);
    },
    createElement(tag) {
      return new FakeElement(tag);
    },
    createTextNode(text) {
      const el = new FakeElement("#text");

      el.textContent = text;

      return el;
    },
    querySelector() {
      return null;
    },
    querySelectorAll() {
      return [];
    },
    addEventListener(type, fn) {
      (this.listeners[type] ||= []).push(fn);
    },
    removeEventListener() {},
    execCommand() {
      return true;
    },
    hidden: false,
    visibilityState: "visible",
  };

  const respond = (path, init) => {
    requests.push({ path, init: init || {} });

    const route = routes[path.split("?")[0]];
    const r = route ? route(init || {}) : { status: 404, body: { message: "not found" } };
    const status = r.status || 200;
    const text = r.body === undefined ? "" : typeof r.body === "string" ? r.body : JSON.stringify(r.body);
    const headers = new Map(Object.entries(r.headers || {}));

    return {
      ok: status >= 200 && status < 300,
      status,
      statusText: String(status),
      headers: { get: (k) => (headers.has(k) ? headers.get(k) : null) },
      text: async () => text,
      json: async () => JSON.parse(text || "{}"),
    };
  };

  const globals = {
    document,
    window: {
      location: { href: "http://console.test/ui", origin: "http://console.test", pathname: "/ui", search: "", hash: "", assign() {} },
      history: { replaceState() {} },
      addEventListener() {},
      removeEventListener() {},
      matchMedia: () => ({ matches: false, addEventListener() {} }),
      scrollTo() {},
      isSecureContext: true,
      innerWidth: 1280,
      innerHeight: 800,
    },
    // The first-visit tour is marked seen unless a test says otherwise: it opens on its own the
    // first time the console loads, which is right for a reader and noise for every other test.
    localStorage: new FakeStorage({ hippocampus_tour_seen: "1", ...local }),
    sessionStorage: new FakeStorage(session),
    navigator: { clipboard: { writeText: async (text) => clipboard.push(text) } },
    fetch: async (path, init) => respond(path, init),
    ResizeObserver: class {
      observe() {}
      disconnect() {}
    },
    setTimeout: (fn, ms, ...args) => {
      const handle = realSetTimeout(fn, ms, ...args);

      timers.add(handle);

      return handle;
    },
    setInterval: (fn, ms, ...args) => {
      const handle = realSetInterval(fn, ms, ...args);

      timers.add(handle);

      return handle;
    },
    clearTimeout: (handle) => {
      timers.delete(handle);
      realClearTimeout(handle);
    },
    clearInterval: (handle) => {
      timers.delete(handle);
      realClearInterval(handle);
    },
    requestAnimationFrame: (fn) => realSetTimeout(fn, 0),
    location: undefined,
  };

  globals.location = globals.window.location;

  for (const [k, v] of Object.entries(globals)) {
    Object.defineProperty(globalThis, k, { value: v, configurable: true, writable: true });
  }

  return { document, byId, requests, clipboard };
}

// The names a test can reach, appended to the module as an export block. Only functions and the
// state the tests need: the point is to drive the real code, not to reimplement any of it.
const EXPORTS = `
export const __app = {
  ACTIONS, api, applyCaps, refreshCaps, toast, showTab,
  state: () => ({ caps, authCfg, server, oidc }),
};
`;

let loads = 0;
let dir = null;

// loadApp writes app.js beside lib.js's absolute URL with the export block appended, and imports a
// fresh instance of it - a new query string per load, since the module cache keys on the URL. Call
// installDom first: app.js runs at load, boot() included.
export async function loadApp() {
  dir ||= mkdtempSync(join(tmpdir(), "hippocampus-webui-"));

  const lib = pathToFileURL(join(webui, "lib.js")).href;
  const source = readFileSync(join(webui, "app.js"), "utf8").replace(`from "./lib.js"`, `from "${lib}"`);

  if (source.includes(`from "./lib.js"`)) {
    throw new Error("app.js still imports ./lib.js after rewriting");
  }

  const path = join(dir, "app.mjs");

  writeFileSync(path, source + EXPORTS);

  loads++;

  const mod = await import(pathToFileURL(path).href + "?load=" + loads);

  // boot() is async and unawaited at load; let it finish answering from the fake routes.
  await settle();

  return mod.__app;
}

// settle lets pending promises and zero-delay timers run.
export async function settle() {
  for (let i = 0; i < 10; i++) {
    await new Promise((resolve) => setImmediate(resolve));
  }
}
