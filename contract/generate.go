package contract

// The gRPC, gateway, and OpenAPI code in this package is generated from hippocampus.proto.
// Regenerate it with `go generate ./contract` (requires protoc plus the protoc-gen-go,
// protoc-gen-go-grpc, protoc-gen-grpc-gateway, and protoc-gen-openapiv2 plugins on PATH; the
// vendored protos under google/api/ and protoc-gen-openapiv2/options/ are resolved via -I=.).
//
// The openapiv2 options are vendored for one file-level option: the securityDefinitions block that
// tells a browser API explorer how to authenticate. They are copied from the grpc-gateway module at
// the version go.mod pins, and want re-copying if that version moves.
//go:generate protoc -I=. --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative --grpc-gateway_out=. --grpc-gateway_opt=paths=source_relative,logtostderr=true --openapiv2_out=. hippocampus.proto
