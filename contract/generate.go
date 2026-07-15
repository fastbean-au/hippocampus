package contract

// The gRPC, gateway, and OpenAPI code in this package is generated from hippocampus.proto.
// Regenerate it with `go generate ./contract` (requires protoc plus the protoc-gen-go,
// protoc-gen-go-grpc, protoc-gen-grpc-gateway, and protoc-gen-openapiv2 plugins on PATH; the
// vendored google/api protos under google/api/ are resolved via -I=.).
//go:generate protoc -I=. --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative --grpc-gateway_out=. --grpc-gateway_opt=paths=source_relative,logtostderr=true --openapiv2_out=. hippocampus.proto
