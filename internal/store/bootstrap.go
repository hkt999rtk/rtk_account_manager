package store

import (
	"context"
	"strings"
)

// BootstrapPlatformAdmin creates the first administrator and permanently seals
// bootstrap in the same transaction. Repeated starts perform no user mutation.
// Recovery must use a separately authorized workflow; it cannot unseal this row.
func (s *Store) BootstrapPlatformAdmin(ctx context.Context, email, passwordHash string, displayName *string) (bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var sealed bool
	if err = tx.QueryRow(ctx, `SELECT sealed_at IS NOT NULL FROM platform_bootstrap WHERE singleton=true FOR UPDATE`).Scan(&sealed); err != nil {
		return false, err
	}
	if sealed {
		return false, nil
	}
	var existing bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE platform_admin) OR EXISTS(SELECT 1 FROM role_assignments a JOIN roles r ON r.id=a.role_id WHERE r.name='platform_admin' AND a.scope_type='platform')`).Scan(&existing); err != nil {
		return false, err
	}
	if existing {
		if _, err = tx.Exec(ctx, `UPDATE platform_bootstrap SET sealed_at=now(),reason='existing_installation' WHERE singleton=true`); err != nil {
			return false, err
		}
		return false, tx.Commit(ctx)
	}
	if strings.TrimSpace(email) == "" || passwordHash == "" {
		return false, ErrConflict
	}
	// A preexisting ordinary user must not be promoted by a stale bootstrap value.
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE email=$1)`, strings.ToLower(strings.TrimSpace(email))).Scan(&existing); err != nil {
		return false, err
	}
	if existing {
		return false, ErrConflict
	}
	user, err := ensurePlatformAdminTx(ctx, tx, email, passwordHash, displayName, false)
	if err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE platform_bootstrap SET sealed_at=now(),initial_user_id=$1,reason='initial_bootstrap' WHERE singleton=true`, user.ID); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// PlatformBootstrapSealed allows startup to ignore retired bootstrap secrets.
func (s *Store) PlatformBootstrapSealed(ctx context.Context) (bool, error) {
	var sealed bool
	err := s.db.QueryRow(ctx, `SELECT sealed_at IS NOT NULL FROM platform_bootstrap WHERE singleton=true`).Scan(&sealed)
	return sealed, err
}
