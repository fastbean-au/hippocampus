// Package client dials a Hippocampus gRPC service. The ingestor needs two of them - the edge it
// drains and the central instance it promotes into - so unlike the broker bridges, which have one
// connection and one set of flags, everything here is parameterised by an endpoint name.
package client

import (
	"google.golang.org/grpc"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/dial"
)

// Config describes how to reach the service; see dial.Config. The connection is the shared root
// package's (TODO-3 item 172), so the token, the TLS block, the client RPC metrics and the version
// header are implemented once for every process that dials the service.
type Config = dial.Config

// Dial opens the connection; see dial.Dial.
func Dial(cfg Config) (*grpc.ClientConn, contract.HippocampusClient, error) {
	return dial.Dial(cfg)
}
