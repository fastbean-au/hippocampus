// Behaviour of individual checks in the wizard's validate(), executed rather than scanned.

import test from "node:test";
import assert from "node:assert/strict";

import { loadWizard } from "./harness.js";

// A count rather than the issues themselves: an array built inside the vm context has that realm's
// Array prototype, so deepEqual against a literal [] fails however empty it is.
const raised = (wizard, fragment) =>
  wizard.validate().filter((issue) => issue.text.includes(fragment)).length;

test("an external capacity target alone bounds the store", () => {
  const wizard = loadWizard();

  wizard.setValue("consolidation.capacityMemories", 0);
  assert.equal(
    raised(wizard, "capacity axes are 0"),
    1,
    "with every axis at 0 the store is unbounded and should be told so",
  );

  wizard.setValue("consolidation.capacityExternalBytes", 1024 ** 4);
  wizard.setValue("consolidation.capacityExternalBytesFloor", 0.85 * 1024 ** 4);
  assert.equal(
    raised(wizard, "capacity axes are 0"),
    0,
    "consolidation.capacityExternalBytes is a capacity axis; the service counts it, so the wizard must",
  );
});

test("an unauthenticated instance bound to loopback is not told to bind to loopback", () => {
  const wizard = loadWizard();

  wizard.state.target = "systemd";
  wizard.setValue("gateway.port", 8080);

  const warning = "Authentication is off";

  assert.equal(raised(wizard, warning), 1, "both listeners open: warn");

  wizard.setValue("bindAddress", "127.0.0.1");
  assert.equal(
    raised(wizard, warning),
    1,
    "the gateway still serves every RPC on every interface: warn",
  );

  wizard.setValue("gateway.bindAddress", "127.0.0.1");
  assert.equal(
    raised(wizard, warning),
    0,
    "both on loopback: nothing to warn about",
  );

  wizard.setValue("gateway.bindAddress", "");
  wizard.setValue("gateway.port", 0);
  assert.equal(
    raised(wizard, warning),
    0,
    "gRPC on loopback, no gateway: nothing to warn about",
  );
});
