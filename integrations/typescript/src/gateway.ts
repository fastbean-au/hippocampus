import { create, fromJson, isMessage } from "@bufbuild/protobuf";
import type {
  DescMessage,
  DescMethodStreaming,
  DescMethodUnary,
  MessageInitShape,
  MessageShape,
} from "@bufbuild/protobuf";
import { Code, ConnectError, createContextValues } from "@connectrpc/connect";
import type { ContextValues, Interceptor, StreamResponse, Transport, UnaryResponse } from "@connectrpc/connect";
import { runUnaryCall } from "@connectrpc/connect/protocol";

import { identityInterceptor } from "./headers.js";
import type { CallIdentity } from "./headers.js";
import { gatewayRequest } from "./routes.js";

// GatewayTransportOptions configures a transport over the service's JSON/HTTP `/v1` gateway.
export interface GatewayTransportOptions extends CallIdentity {
  // baseUrl is the gateway's origin, e.g. "http://localhost:8080" - the server's gateway.port, which
  // is 0 (off) by default.
  baseUrl: string;

  // fetch replaces globalThis.fetch. It is how a host with its own HTTP stack plugs in: Obsidian's
  // requestUrl, which is not subject to the renderer's CORS policy, or a fetch with a proxy agent.
  fetch?: typeof globalThis.fetch;

  // timeoutMs bounds a call that names no timeout of its own. Undefined leaves calls unbounded.
  timeoutMs?: number;

  // interceptors run around every call, after the identity interceptor, exactly as they would on
  // the gRPC transport.
  interceptors?: Interceptor[];
}

// createGatewayTransport returns a Connect transport that speaks the service's JSON gateway instead
// of gRPC, so the generated client works unchanged in a browser, a worker, Deno, or any host that
// can fetch but cannot open an HTTP/2 connection. It is the same split the CLI makes with its
// --transport flag, and for the same reason: one interface, two wires, every caller written once.
//
// Errors come back as ConnectError carrying the gRPC code the service chose - the gateway renders a
// failure as {"code": <grpc code>, "message": ...} - so a caller branching on Code.NotFound branches
// the same way on either transport.
export function createGatewayTransport(options: GatewayTransportOptions): Transport {
  const baseUrl = options.baseUrl.replace(/\/+$/, "");
  const doFetch = options.fetch ?? globalThis.fetch.bind(globalThis);
  const interceptors = [identityInterceptor(options), ...(options.interceptors ?? [])];

  return {
    async unary<I extends DescMessage, O extends DescMessage>(
      method: DescMethodUnary<I, O>,
      signal: AbortSignal | undefined,
      timeoutMs: number | undefined,
      header: HeadersInit | undefined,
      input: MessageInitShape<I>,
      contextValues?: ContextValues,
    ): Promise<UnaryResponse<I, O>> {
      const message = (isMessage(input, method.input) ? input : create(method.input, input)) as MessageShape<I>;
      const first = gatewayRequest(method, message);

      return runUnaryCall<I, O>({
        interceptors,
        signal,
        timeoutMs: timeoutMs ?? options.timeoutMs,
        req: {
          stream: false,
          service: method.parent,
          method,
          requestMethod: first.verb,
          url: urlOf(baseUrl, first.path, first.query),
          header: new Headers(header),
          contextValues: contextValues ?? createContextValues(),
          message,
        },
        next: async (req) => {
          // Rendered again from the message the interceptors passed on, since one may have changed
          // it - and the path and query are a function of the message.
          const rendered = gatewayRequest(method, req.message);
          const headers = new Headers(req.header);

          if (rendered.body !== undefined) {
            headers.set("content-type", "application/json");
          }

          let response: Response;

          try {
            response = await doFetch(urlOf(baseUrl, rendered.path, rendered.query), {
              method: rendered.verb,
              headers,
              body: rendered.body,
              signal: req.signal,
            });
          } catch (cause) {
            throw ConnectError.from(cause, Code.Unavailable);
          }

          const text = await response.text();

          if (!response.ok) {
            throw errorOf(response.status, text, response.headers);
          }

          let parsed: unknown;

          try {
            parsed = text === "" ? {} : JSON.parse(text);
          } catch (cause) {
            throw new ConnectError(`the gateway returned a body that is not JSON: ${cause}`, Code.Internal);
          }

          return {
            stream: false,
            service: method.parent,
            method,
            header: response.headers,
            trailer: new Headers(),
            message: fromJson(method.output, parsed as never, { ignoreUnknownFields: true }),
          };
        },
      });
    },

    async stream<I extends DescMessage, O extends DescMessage>(
      method: DescMethodStreaming<I, O>,
    ): Promise<StreamResponse<I, O>> {
      throw new ConnectError(
        `${method.name} needs the gRPC transport: the HTTP gateway cannot stream`,
        Code.Unimplemented,
      );
    },
  };
}

function urlOf(baseUrl: string, path: string, query: URLSearchParams): string {
  const search = query.toString();

  return search === "" ? baseUrl + path : `${baseUrl}${path}?${search}`;
}

// errorOf turns a non-2xx gateway response back into the error the gRPC transport would have raised.
// When the body is not the gateway's status shape - a proxy's page, an empty 502 - the code is taken
// from the HTTP status instead, by the same table grpc-gateway maps codes onto statuses with.
export function errorOf(status: number, body: string, headers?: Headers): ConnectError {
  try {
    const parsed = JSON.parse(body) as { code?: unknown; message?: unknown };

    if (typeof parsed.code === "number" && typeof parsed.message === "string") {
      return new ConnectError(parsed.message, parsed.code as Code, headers);
    }
  } catch {
    // Not the gateway's shape; fall through to the status.
  }

  const message = body.trim() === "" ? `the gateway answered HTTP ${status}` : body.trim();

  return new ConnectError(message, codeOfStatus(status), headers);
}

function codeOfStatus(status: number): Code {
  switch (status) {

    case 400:
      return Code.InvalidArgument;

    case 401:
      return Code.Unauthenticated;

    case 403:
      return Code.PermissionDenied;

    case 404:
      return Code.NotFound;

    case 409:
      return Code.AlreadyExists;

    case 429:
      return Code.ResourceExhausted;

    case 501:
      return Code.Unimplemented;

    case 502:
    case 503:
      return Code.Unavailable;

    case 504:
      return Code.DeadlineExceeded;

    default:
      return Code.Unknown;

  }
}
