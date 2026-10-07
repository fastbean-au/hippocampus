// Package contract is the Hippocampus gRPC contract: hippocampus.proto and the code generated from
// it - the protobuf messages, the gRPC client and server, the grpc-gateway reverse proxy that maps
// every RPC onto a JSON/HTTP route under /v1, and the OpenAPI document (SwaggerJSON).
//
// Regenerate after editing the proto with go generate ./contract. Every message and field carries a
// comment, since protoc copies them into every generated client and the OpenAPI document.
package contract
