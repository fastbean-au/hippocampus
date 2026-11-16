import type { Interceptor } from "@connectrpc/connect";

import { CLIENT_VERSION_HEADER, DEFAULT_CLIENT_VERSION } from "./version.js";

// CallIdentity is what every call carries regardless of transport: the bearer token, when the
// service authenticates, and the client's own build for the topology view.
export interface CallIdentity {
  // token is a signed JWT, sent as "authorization: Bearer <token>". Omit it for a service running
  // with auth.method "none".
  token?: string;

  // clientVersion replaces the default "hippocampus-client-typescript/<version>". An application
  // built on this package should name itself ("my-agent/1.4.0"), since that is what an operator
  // reading the topology view wants to see. An empty string sends nothing.
  clientVersion?: string;
}

// identityInterceptor attaches a CallIdentity to every call, unary or streaming. It is an interceptor
// rather than transport code so both transports share one statement of it - the gateway transport
// runs interceptors exactly as the gRPC one does - and so it composes with a caller's own.
export function identityInterceptor(identity: CallIdentity): Interceptor {
  const token = identity.token ?? "";
  const clientVersion = identity.clientVersion ?? DEFAULT_CLIENT_VERSION;

  return (next) => async (req) => {
    if (token !== "") {
      req.header.set("authorization", `Bearer ${token}`);
    }

    if (clientVersion !== "") {
      req.header.set(CLIENT_VERSION_HEADER, clientVersion);
    }

    return next(req);
  };
}
