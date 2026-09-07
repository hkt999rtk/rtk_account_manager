package usercache

import (
	"context"
	"fmt"
)

// PKIRoles is intentionally never cached: revocation of a custody role takes
// effect on the next operation, regardless of the user's cached login state.
func (s *Store) PKIRoles(ctx context.Context, userID string) ([]string, error) {
	source, ok := s.Store.(interface {
		PKIRoles(context.Context, string) ([]string, error)
	})
	if !ok {
		return nil, fmt.Errorf("PKI role store unavailable")
	}
	return source.PKIRoles(ctx, userID)
}
