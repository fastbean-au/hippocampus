package contract

import _ "embed"

// SwaggerJSON is the OpenAPI/Swagger description of the HTTP gateway, generated alongside the
// gRPC/gateway code by `go generate ./contract`. main.go serves it at /v1/openapi.json.
//
//go:embed hippocampus.swagger.json
var SwaggerJSON []byte
