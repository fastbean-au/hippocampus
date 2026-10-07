// Package client dials the Hippocampus gRPC service and wraps the three RPCs this integration
// uses.
//
// Those three are the whole surface, and the list is a statement rather than an accident:
// RecallMemories to reinforce what is being read, ExplainConsolidation to ask which ids the store
// still holds, and GetForgottenMemories to catch up on what it forgot while nobody was listening.
// Nothing here writes a memory, deletes one, or purges anything - the only destructive thing this
// integration does is in the bucket, and the store is strictly the authority it reads.
//
// A consequence worth knowing when a token is minted for it: the tap needs WRITER tier, because
// RecallMemories reinforces and a reader-tier token gets a plain non-reinforcing read unless the
// deployment sets auth.readerRecallReinforces. The reaper needs only READER. Both want an UNSCOPED
// token, for the reason Recall's own doc gives.
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
