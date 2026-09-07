package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrRecoveryDenied = errors.New("administrator recovery denied")

type RecoveryPrincipal struct {
	UserID   string
	MFA      bool
	AuthTime time.Time
}
type RecoveryCommand struct {
	Action string `json:"-"`
	ID     string `json:"-"`
	Key    string `json:"-"`
	Target string `json:"target_user_id,omitempty"`
	Reason string `json:"reason,omitempty"`
	Role   string `json:"role,omitempty"`
	Digest string `json:"request_sha256,omitempty"`
}
type RecoveryApproval struct {
	UserID string    `json:"principal_id"`
	Role   string    `json:"role"`
	At     time.Time `json:"approved_at"`
}
type AdminRecovery struct {
	ID        string             `json:"request_id"`
	Target    string             `json:"target_user_id"`
	Requester string             `json:"requested_by"`
	Reason    string             `json:"reason"`
	Digest    string             `json:"request_sha256"`
	Status    string             `json:"status"`
	CreatedAt time.Time          `json:"created_at"`
	ExpiresAt time.Time          `json:"expires_at"`
	Approvals []RecoveryApproval `json:"approvals"`
}

// Read exact live assignments under locks, not cached role claims. Holding these
// locks through execution prevents concurrent revocation from racing a grant.
func recoveryRole(ctx context.Context, tx pgx.Tx, userID string, wants ...string) error {
	rows, err := tx.Query(ctx, `SELECT r.name FROM users u JOIN role_assignments a ON a.actor_id=u.id::text JOIN roles r ON r.id=a.role_id
 WHERE u.id::text=$1 AND u.disabled_at IS NULL AND u.email_verified AND NOT u.signup_pending_verification
 AND a.actor_type='user' AND a.scope_type='platform' AND a.disabled_at IS NULL AND r.disabled_at IS NULL
 AND r.name=ANY($2) FOR SHARE OF u,a,r`, userID, wants)
	if err != nil {
		return err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		found = true
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if !found {
		return ErrRecoveryDenied
	}
	return nil
}

func recoveryRow(ctx context.Context, tx pgx.Tx, id string) (AdminRecovery, error) {
	var r AdminRecovery
	err := tx.QueryRow(ctx, `SELECT id::text,target_user_id::text,requested_by::text,reason,digest,status,created_at,expires_at FROM platform_admin_recovery WHERE id::text=$1 FOR UPDATE`, id).Scan(&r.ID, &r.Target, &r.Requester, &r.Reason, &r.Digest, &r.Status, &r.CreatedAt, &r.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// AdminRecovery performs the sealed recovery workflow. The caller's identity
// and MFA assurance must come from a verified access token at the API boundary.
func (s *Store) AdminRecovery(ctx context.Context, p RecoveryPrincipal, cmd RecoveryCommand, now time.Time) (AdminRecovery, error) {
	var r AdminRecovery
	if !p.MFA || p.UserID == "" || p.AuthTime.IsZero() || p.AuthTime.After(now.Add(30*time.Second)) || p.AuthTime.Before(now.Add(-5*time.Minute)) {
		return r, ErrRecoveryDenied
	}
	if cmd.Action != "create" && (cmd.Target != "" || cmd.Reason != "") {
		return r, ErrRecoveryDenied
	}
	if cmd.Action != "approve" && (cmd.Role != "" || cmd.Digest != "") {
		return r, ErrRecoveryDenied
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return r, err
	}
	defer tx.Rollback(ctx)
	var sealed bool
	if err = tx.QueryRow(ctx, `SELECT sealed_at IS NOT NULL FROM platform_bootstrap WHERE singleton`).Scan(&sealed); err != nil {
		return r, err
	}
	if !sealed {
		return r, ErrRecoveryDenied
	}
	if err = recoveryRole(ctx, tx, p.UserID, "platform_admin", "pki_admin", "security_custodian", "pki_auditor"); err != nil {
		return r, err
	}
	if cmd.Action == "access" {
		return AdminRecovery{Status: "authorized"}, tx.Commit(ctx)
	}
	if cmd.Action == "create" {
		if err = recoveryRole(ctx, tx, p.UserID, "platform_admin", "pki_admin"); err != nil {
			return r, err
		}
		if cmd.Target == p.UserID || cmd.Target == "" || strings.TrimSpace(cmd.Reason) == "" || len(cmd.Reason) > 2048 || cmd.Key == "" || len(cmd.Key) > 200 {
			return r, ErrRecoveryDenied
		}
		payload, _ := json.Marshal(struct {
			Target string `json:"target_user_id"`
			Reason string `json:"reason"`
		}{cmd.Target, cmd.Reason})
		sum := sha256.Sum256(payload)
		digest := hex.EncodeToString(sum[:])
		// Serialize idempotency keys before resolving or creating the target grant.
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, p.UserID+":"+cmd.Key); err != nil {
			return r, err
		}
		var existing string
		err = tx.QueryRow(ctx, `SELECT id::text FROM platform_admin_recovery WHERE requested_by::text=$1 AND idempotency_key=$2`, p.UserID, cmd.Key).Scan(&existing)
		if err == nil {
			r, err = recoveryRow(ctx, tx, existing)
			if err != nil {
				return r, err
			}
			if r.Digest != digest {
				return r, ErrConflict
			}
			cmd.Action = "get"
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return r, err
		} else {
			if err = recoveryTarget(ctx, tx, cmd.Target); err != nil {
				return r, err
			}
			if err = tx.QueryRow(ctx, `INSERT INTO platform_admin_recovery(target_user_id,requested_by,reason,digest,idempotency_key,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id::text`, cmd.Target, p.UserID, cmd.Reason, digest, cmd.Key, now, now.Add(time.Hour)).Scan(&existing); err != nil {
				return r, err
			}
			r, err = recoveryRow(ctx, tx, existing)
			if err != nil {
				return r, err
			}
			if err = recoveryAudit(ctx, tx, r.ID, p.UserID, "requested"); err != nil {
				return r, err
			}
			cmd.Action = "get"
		}
	} else {
		r, err = recoveryRow(ctx, tx, cmd.ID)
		if err != nil {
			return r, err
		}
	}
	if cmd.Action != "get" && (r.Status != "requested" || !r.ExpiresAt.After(now)) {
		if !(cmd.Action == "execute" && r.Status == "completed") {
			return r, ErrConflict
		}
	}
	switch cmd.Action {
	case "get":
	case "approve":
		if p.UserID == r.Requester || p.UserID == r.Target || cmd.Digest != r.Digest || (cmd.Role != "pki_admin" && cmd.Role != "security_custodian") {
			return r, ErrRecoveryDenied
		}
		if err = recoveryRole(ctx, tx, p.UserID, cmd.Role); err != nil {
			return r, err
		}
		var priorUser, priorRole string
		err = tx.QueryRow(ctx, `SELECT principal_id::text,role FROM platform_admin_recovery_approvals WHERE request_id=$1 AND (principal_id::text=$2 OR role=$3)`, r.ID, p.UserID, cmd.Role).Scan(&priorUser, &priorRole)
		if err == nil {
			if priorUser != p.UserID || priorRole != cmd.Role {
				return r, ErrConflict
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return r, err
		} else {
			if _, err = tx.Exec(ctx, `INSERT INTO platform_admin_recovery_approvals(request_id,principal_id,role,digest,auth_time,approved_at) VALUES($1,$2,$3,$4,$5,$6)`, r.ID, p.UserID, cmd.Role, r.Digest, p.AuthTime, now); err != nil {
				return r, err
			}
			if err = recoveryAudit(ctx, tx, r.ID, p.UserID, "approved:"+cmd.Role); err != nil {
				return r, err
			}
		}
	case "cancel":
		if p.UserID != r.Requester {
			return r, ErrRecoveryDenied
		}
		if _, err = tx.Exec(ctx, `UPDATE platform_admin_recovery SET status='cancelled' WHERE id=$1`, r.ID); err != nil {
			return r, err
		}
		r.Status = "cancelled"
		if err = recoveryAudit(ctx, tx, r.ID, p.UserID, "cancelled"); err != nil {
			return r, err
		}
	case "execute":
		if err = recoveryRole(ctx, tx, p.UserID, "pki_admin"); err != nil {
			return r, err
		}
		if p.UserID == r.Target {
			return r, ErrRecoveryDenied
		}
		if r.Status == "completed" {
			break
		}
		approvals, e := recoveryApprovals(ctx, tx, r.ID)
		if e != nil {
			return r, e
		}
		if len(approvals) != 2 {
			return r, ErrRecoveryDenied
		}
		for _, a := range approvals {
			if err = recoveryRole(ctx, tx, a.UserID, a.Role); err != nil {
				return r, err
			}
		}
		if err = recoveryTarget(ctx, tx, r.Target); err != nil {
			return r, err
		}
		var roleID string
		if err = tx.QueryRow(ctx, `SELECT id::text FROM roles WHERE name='platform_admin' AND disabled_at IS NULL FOR SHARE`).Scan(&roleID); err != nil {
			return r, err
		}
		if _, err = tx.Exec(ctx, `UPDATE users SET platform_admin=true,updated_at=now() WHERE id::text=$1`, r.Target); err != nil {
			return r, err
		}
		if _, err = tx.Exec(ctx, `UPDATE role_assignments SET disabled_at=NULL WHERE role_id::text=$1 AND actor_type='user' AND actor_id=$2 AND scope_type='platform'`, roleID, r.Target); err != nil {
			return r, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO role_assignments(role_id,actor_type,actor_id,scope_type) VALUES($1,'user',$2,'platform') ON CONFLICT DO NOTHING`, roleID, r.Target); err != nil {
			return r, err
		}
		if _, err = tx.Exec(ctx, `UPDATE platform_admin_recovery SET status='completed' WHERE id=$1`, r.ID); err != nil {
			return r, err
		}
		r.Status = "completed"
		if err = createACLAuditEventTx(ctx, tx, ACLAuditEventInput{EventType: "platform_admin_recovered", ActorUserID: &p.UserID, SubjectType: "user", SubjectID: r.Target, Payload: map[string]any{"recovery_request_id": r.ID, "request_sha256": r.Digest}}); err != nil {
			return r, err
		}
		if err = recoveryAudit(ctx, tx, r.ID, p.UserID, "administrator_granted"); err != nil {
			return r, err
		}
	default:
		return r, ErrRecoveryDenied
	}
	r.Approvals, err = recoveryApprovals(ctx, tx, r.ID)
	if err != nil {
		return r, err
	}
	return r, tx.Commit(ctx)
}

func recoveryTarget(ctx context.Context, tx pgx.Tx, id string) error {
	var actual string
	err := tx.QueryRow(ctx, `SELECT id::text FROM users WHERE id::text=$1 AND disabled_at IS NULL AND email_verified AND NOT signup_pending_verification FOR UPDATE`, id).Scan(&actual)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRecoveryDenied
	}
	return err
}
func recoveryAudit(ctx context.Context, tx pgx.Tx, id, actor, event string) error {
	_, err := tx.Exec(ctx, `INSERT INTO platform_admin_recovery_audit(request_id,actor_id,event) VALUES($1,$2,$3)`, id, actor, event)
	return err
}
func recoveryApprovals(ctx context.Context, tx pgx.Tx, id string) ([]RecoveryApproval, error) {
	rows, err := tx.Query(ctx, `SELECT principal_id::text,role,approved_at FROM platform_admin_recovery_approvals WHERE request_id=$1 ORDER BY role`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []RecoveryApproval{}
	for rows.Next() {
		var a RecoveryApproval
		if err = rows.Scan(&a.UserID, &a.Role, &a.At); err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}
