package bridge

import (
	"google.golang.org/grpc"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/dial"
)

// The connection to the service is the shared root package's (TODO-3 item 172): the bearer token,
// the OIDC client-credentials grant, the TLS block, the client RPC metrics and the version header,
// implemented once for every process that dials the service rather than once per integration. These
// names are kept so the broker commands read as they always have.

// ClientConfig describes how to reach the service; see dial.Config.
type ClientConfig = dial.Config

// OIDCConfig configures the client-credentials grant; see dial.OIDCConfig.
type OIDCConfig = dial.OIDCConfig

// Dial opens the connection; see dial.Dial.
func Dial(cfg ClientConfig) (*grpc.ClientConn, contract.HippocampusClient, error) {
	return dial.Dial(cfg)
}
