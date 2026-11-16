// VERSION is this package's own release, stamped from the tag by the release workflow exactly as the
// Python client's _version.py is: the package is published from the service's tag, so
// hippocampus-client@X.Y.Z is by construction the client of vX.Y.Z's contract. The placeholder is
// what a build from the tree reports, and the workflow refuses to publish one that survived.
export const VERSION = "0.0.0-dev";

// CLIENT_VERSION_HEADER is contract.ClientVersionHeader, the header the service reads on both
// transports to put a caller's build on the topology view. Restated rather than generated because it
// is a Go constant, not part of the proto; test/routes.test.ts holds the two copies together.
export const CLIENT_VERSION_HEADER = "hippocampus-client-version";

// DEFAULT_CLIENT_VERSION is what this package reports unless a caller names its own product, in the
// "<product>/<version>" form every other client uses ("hippocampus-client-python/0.53.4").
export const DEFAULT_CLIENT_VERSION = `hippocampus-client-typescript/${VERSION}`;
