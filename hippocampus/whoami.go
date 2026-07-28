package hippocampus

import (
	"context"

	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/auth"
	"github.com/fastbean-au/hippocampus/contract"
)

// WhoAmI reports the caller's identity and effective authorisation tier so a client (the web
// console) can tailor what it offers rather than guessing at the token's roles. The tier and
// client id are read from the request context, where the authorisation layer stashed them on a
// successful check; their absence means authorisation never ran (authentication is disabled), which
// is reported as an unrestricted admin tier with auth_enabled false so the client shows everything.
func (s *Server) WhoAmI(ctx context.Context, _ *contract.EmptyRequest) (*contract.WhoAmIResponse, error) {
	log.Trace("func() WhoAmI")

	tier, ok := auth.TierFromContext(ctx)

	if !ok {
		return &contract.WhoAmIResponse{Role: auth.TierAdmin.String(), AuthEnabled: false}, nil
	}

	return &contract.WhoAmIResponse{
		ClientId:    auth.ClientIDFromContext(ctx),
		Role:        tier.String(),
		AuthEnabled: true,
	}, nil
}
