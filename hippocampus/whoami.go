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

	// The available search modes are a property of the deployment, not the caller, so they are
	// reported identically on both paths - including the unauthenticated one, where there is no
	// caller to describe. The summariser is reported for the same reason and on the same terms.
	modes := s.searchModes()
	summariser := s.summariser().Enabled()

	// The group scope is reported on both paths. On the unauthenticated one it is always absent,
	// which is the truth rather than a placeholder: with no token there is no scope, and a client
	// that renders "unscoped" from it is showing the right thing.
	groups, scoped := s.scopedGroups(ctx)

	tier, ok := auth.TierFromContext(ctx)

	if !ok {
		return &contract.WhoAmIResponse{
			Role:              auth.TierAdmin.String(),
			AuthEnabled:       false,
			SearchModes:       modes,
			Groups:            groups,
			GroupScoped:       scoped,
			SummariserEnabled: summariser,
		}, nil
	}

	return &contract.WhoAmIResponse{
		ClientId:          auth.ClientIDFromContext(ctx),
		Role:              tier.String(),
		AuthEnabled:       true,
		SearchModes:       modes,
		Groups:            groups,
		GroupScoped:       scoped,
		SummariserEnabled: summariser,
	}, nil
}
