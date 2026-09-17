package usercache

import (
	"context"
	"fmt"
	"rtk_account_manager/internal/store"
	"time"
)

// Recovery always reads live roles and writes the authoritative database.
func (s *Store) AdminRecovery(ctx context.Context, p store.RecoveryPrincipal, cmd store.RecoveryCommand, now time.Time) (store.AdminRecovery, error) {
	source, ok := s.Store.(interface {
		AdminRecovery(context.Context, store.RecoveryPrincipal, store.RecoveryCommand, time.Time) (store.AdminRecovery, error)
	})
	if !ok {
		return store.AdminRecovery{}, fmt.Errorf("recovery storage unavailable")
	}
	return source.AdminRecovery(ctx, p, cmd, now)
}
