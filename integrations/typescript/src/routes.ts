import { getOption, toJson } from "@bufbuild/protobuf";
import type { DescMessage, DescMethod, JsonObject, JsonValue, MessageShape } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";

import { http } from "./gen/google/api/annotations_pb.js";

// The gateway's routes are not restated here. Each one is the method's google.api.http annotation,
// read off the descriptor the contract generated - the same annotation grpc-gateway compiled the
// server's routes from - so a new RPC is reachable over HTTP the moment it is generated, and a
// changed path or verb cannot drift from the server's because there is no second copy of it. The CLI
// hand-writes its gateway client and pays for that with a method per RPC; this file is the reason
// this package does not.

// Route is one method's HTTP binding: the verb, the path template, and which part of the request
// is the body ("*" all of it, "" none - the rest goes in the query string - or one field's name).
export interface Route {
  verb: string;
  template: string;
  body: string;
}

// GatewayRequest is one call rendered for the gateway.
export interface GatewayRequest {
  verb: string;
  path: string;
  query: URLSearchParams;
  body: string | undefined;
}

// routeOf reads a method's binding, or undefined for a method the gateway does not serve (Watch: it
// streams, and the gateway's calls are made in process, which cannot).
export function routeOf(method: DescMethod): Route | undefined {
  const rule = getOption(method, http);

  switch (rule.pattern.case) {

    case "get":
    case "put":
    case "post":
    case "delete":
    case "patch":
      return { verb: rule.pattern.case.toUpperCase(), template: rule.pattern.value, body: rule.body };

    default:
      return undefined;

  }
}

// gatewayRequest renders a request message the way grpc-gateway parses one: path variables are
// lifted out of the message, the body binding decides what is sent as JSON, and whatever is left
// travels as query parameters. The JSON is protojson's (camelCase names, int64 as strings, enums by
// name), which is exactly what the gateway's unmarshaler and query parser accept.
export function gatewayRequest<I extends DescMessage>(
  method: DescMethod & { input: I },
  message: MessageShape<I>,
): GatewayRequest {
  const route = routeOf(method);

  if (route === undefined) {
    throw new ConnectError(
      `${method.name} needs the gRPC transport: the HTTP gateway does not serve it`,
      Code.Unimplemented,
    );
  }

  const json = toJson(method.input, message) as JsonObject;

  const path = route.template.replace(/\{([^}=]+)(?:=[^}]*)?\}/g, (_, name: string) => {
    const key = jsonNameOf(method.input, name);
    const value = json[key];

    delete json[key];

    return encodeURIComponent(value === undefined || value === null ? "" : String(value));
  });

  if (route.body === "*") {
    return { verb: route.verb, path, query: new URLSearchParams(), body: JSON.stringify(json) };
  }

  let body: string | undefined;

  if (route.body !== "") {
    const key = jsonNameOf(method.input, route.body);

    body = JSON.stringify(json[key] ?? {});

    delete json[key];
  }

  return { verb: route.verb, path, query: queryOf(method.input, json), body };
}

// jsonNameOf maps a field's proto name, which is how a path template and a body binding name it, to
// the JSON name protojson keys it by.
function jsonNameOf(message: DescMessage, protoName: string): string {
  const field = message.fields.find((f) => f.name === protoName);

  if (field === undefined) {
    throw new Error(`${message.typeName} has no field ${protoName}, which its HTTP binding names`);
  }

  return field.jsonName;
}

// queryOf flattens what is left of a request into query parameters: a nested message becomes dotted
// names, a repeated field one parameter per element, and a map field key[name]=value - the three
// forms grpc-gateway's query parser reads.
function queryOf(message: DescMessage, json: JsonObject): URLSearchParams {
  const query = new URLSearchParams();
  const maps = new Set(message.fields.filter((f) => f.fieldKind === "map").map((f) => f.jsonName));

  for (const [key, value] of Object.entries(json)) {
    if (maps.has(key) && value !== null && typeof value === "object" && !Array.isArray(value)) {
      for (const [name, entry] of Object.entries(value)) {
        query.append(`${key}[${name}]`, scalar(entry));
      }

      continue;
    }

    flatten(query, key, value);
  }

  return query;
}

function flatten(query: URLSearchParams, key: string, value: JsonValue): void {
  if (Array.isArray(value)) {
    for (const element of value) {
      flatten(query, key, element);
    }

    return;
  }

  if (value !== null && typeof value === "object") {
    for (const [name, nested] of Object.entries(value)) {
      flatten(query, `${key}.${name}`, nested);
    }

    return;
  }

  query.append(key, scalar(value));
}

function scalar(value: JsonValue): string {
  return value === null ? "" : typeof value === "object" ? JSON.stringify(value) : String(value);
}
