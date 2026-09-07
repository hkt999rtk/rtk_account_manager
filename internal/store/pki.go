package store

import "context"

// PKIRoles deliberately resolves exact persisted role assignments. A platform
// administrator does not implicitly become a CA custodian or PKI approver.
func (s *Store) PKIRoles(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.db.Query(ctx, `SELECT DISTINCT r.name FROM role_assignments a JOIN roles r ON r.id=a.role_id JOIN users u ON u.id::text=a.actor_id WHERE a.actor_type='user' AND a.actor_id=$1 AND a.scope_type='platform' AND a.disabled_at IS NULL AND r.disabled_at IS NULL AND u.disabled_at IS NULL AND r.name IN ('platform_admin','pki_admin','security_custodian','pki_auditor')`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roles []string
	for rows.Next() {
		var role string
		if err = rows.Scan(&role); err != nil {
			return nil, err
		}
		roles = append(roles, role)
	}
	return roles, rows.Err()
}
