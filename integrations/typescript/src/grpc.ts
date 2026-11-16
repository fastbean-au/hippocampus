// The native gRPC transport, for Node.
//
// A separate entry point ("hippocampus-client/grpc") rather than part of the main one, because it is
// the only half of this package that needs node:http2: a browser bundle importing the main entry
// point must not pull in @connectrpc/connect-node, and that package is an optional peer dependency
// for exactly that reason.

import type { Interceptor, Transport } from "@connectrpc/connect";
import { createGrpcTransport } from "@connectrpc/connect-node";
import type { SecureClientSessionOptions } from "node:http2";

import { identityInterceptor } from "./headers.js";
import type { CallIdentity } from "./headers.js";

// GrpcTransportOptions configures a gRPC transport to the service's main port.
export interface GrpcTransportOptions extends CallIdentity {
  // baseUrl is the service's gRPC address as a URL: "http://localhost:50051" for plaintext, or
  // "https://memory.example.com:50051" when the service runs with tls.enabled.
  baseUrl: string;

  // ca, cert and key are PEM: a private CA to trust, and a client certificate for a service that
  // requires mutual TLS. Ignored on an http:// address.
  ca?: string | Buffer;
  cert?: string | Buffer;
  key?: string | Buffer;

  // servername overrides the name the server's certificate is checked against.
  servername?: string;

  // timeoutMs bounds a call that names no timeout of its own. Undefined leaves calls unbounded.
  timeoutMs?: number;

  // interceptors run around every call, after the identity interceptor.
  interceptors?: Interceptor[];
}

// createHippocampusGrpcTransport returns a Connect transport speaking gRPC over HTTP/2 to the
// service, which is the transport to prefer from Node: it is the only one that can serve Watch, and
// it is the one the service's own clients use.
export function createHippocampusGrpcTransport(options: GrpcTransportOptions): Transport {
  const nodeOptions: SecureClientSessionOptions = {};

  if (options.ca !== undefined) {
    nodeOptions.ca = options.ca;
  }

  if (options.cert !== undefined) {
    nodeOptions.cert = options.cert;
  }

  if (options.key !== undefined) {
    nodeOptions.key = options.key;
  }

  if (options.servername !== undefined) {
    nodeOptions.servername = options.servername;
  }

  return createGrpcTransport({
    baseUrl: options.baseUrl,
    defaultTimeoutMs: options.timeoutMs,
    interceptors: [identityInterceptor(options), ...(options.interceptors ?? [])],
    nodeOptions,
  });
}
