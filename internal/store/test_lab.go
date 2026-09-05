package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type TestLabSession struct {
	ID        string    `json:"id"`
	UserID    string    `json:"-"`
	CloudID   string    `json:"brand_cloud_id"`
	ProductID string    `json:"product_id"`
	DeviceID  string    `json:"device_id"`
	Devid     string    `json:"devid"`
	ExpiresAt time.Time `json:"expires_at"`
	AccountID string    `json:"account_id"`
}

func (s *Store) CreateTestLabSession(ctx context.Context, actor, cloud, product, device string, accounts ...string) (TestLabSession, error) {
	if len(accounts) != 1 || accounts[0] == "" {
		return TestLabSession{}, ErrNotFound
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return TestLabSession{}, err
	}
	defer tx.Rollback(ctx)
	// This lock serializes quota admission against other sessions and membership
	// mutations. It deliberately does not grant a platform-admin override.
	if err := lockBrandCloudCollaborationTx(ctx, tx, cloud, actor); err != nil {
		return TestLabSession{}, err
	}
	allowed, err := hasUserPermissionForResource(ctx, tx, actor, cloud, "registry_device.manage", ScopeTypeDevice, device)
	if err != nil {
		return TestLabSession{}, err
	}
	if !allowed {
		return TestLabSession{}, ErrNotFound
	}
	if _, err = labDeviceTx(ctx, tx, actor, cloud, product, device); err != nil {
		return TestLabSession{}, err
	}
	if err = labBoundTx(ctx, tx, actor, cloud, accounts[0], device, true); err != nil {
		return TestLabSession{}, err
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM test_lab_sessions WHERE user_id::text=$1 AND revoked_at IS NULL AND expires_at>now()`, actor).Scan(&count); err != nil {
		return TestLabSession{}, err
	}
	if count >= 3 {
		return TestLabSession{}, ErrRateLimited
	}
	var out TestLabSession
	err = tx.QueryRow(ctx, `INSERT INTO test_lab_sessions(user_id,brand_cloud_id,product_id,device_id,devid,expires_at,account_id)
	 SELECT $1::uuid,d.organization_id,d.device_item_profile_id,d.id,d.metadata->>'video_cloud_devid',now()+interval '5 minutes',$5::uuid
	 FROM devices d JOIN organization_members m ON m.organization_id=d.organization_id AND m.user_id::text=$1
	 WHERE d.id::text=$4 AND d.organization_id::text=$2 AND d.device_item_profile_id::text=$3
	 AND d.disabled_at IS NULL AND m.disabled_at IS NULL AND m.role<>'viewer'
	 AND COALESCE(d.metadata->>'video_cloud_devid','')<>''
	 RETURNING id::text,user_id::text,brand_cloud_id::text,product_id::text,device_id::text,devid,expires_at`, actor, cloud, product, device, accounts[0]).
		Scan(&out.ID, &out.UserID, &out.CloudID, &out.ProductID, &out.DeviceID, &out.Devid, &out.ExpiresAt)
	out.AccountID = accounts[0]
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{ActorUserID: &actor, OrganizationID: &cloud, EventType: "test_lab.session_created", SubjectType: "device", SubjectID: device}); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

func (s *Store) GetTestLabSession(ctx context.Context, id string) (TestLabSession, error) {
	var out TestLabSession
	err := s.db.QueryRow(ctx, `SELECT t.id::text,t.user_id::text,t.brand_cloud_id::text,t.product_id::text,t.device_id::text,t.devid,t.expires_at,t.account_id::text
	 FROM test_lab_sessions t JOIN devices d ON d.id=t.device_id
	 JOIN test_lab_accounts a ON a.id=t.account_id AND a.user_id=t.user_id AND a.brand_cloud_id=t.brand_cloud_id
	 JOIN end_users e ON e.id=a.end_user_id
	 JOIN device_user_bindings b ON b.device_id=d.id AND b.end_user_id=a.end_user_id AND b.brand_cloud_id=t.brand_cloud_id
	 WHERE t.id::text=$1 AND t.revoked_at IS NULL AND t.expires_at>now()
	 AND a.revoked_at IS NULL AND a.expires_at>now() AND e.disabled_at IS NULL AND e.status='active' AND b.disabled_at IS NULL AND d.metadata->>'video_cloud_activation_status'='activated'
	 AND d.organization_id=t.brand_cloud_id AND d.device_item_profile_id=t.product_id AND d.metadata->>'video_cloud_devid'=t.devid`, id).
		Scan(&out.ID, &out.UserID, &out.CloudID, &out.ProductID, &out.DeviceID, &out.Devid, &out.ExpiresAt, &out.AccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if err = s.AuthorizeUserForVideoDevice(ctx, out.UserID, out.Devid); err != nil {
		return out, err
	}
	allowed, err := hasUserPermissionForResource(ctx, s.db, out.UserID, out.CloudID, "registry_device.manage", ScopeTypeDevice, out.DeviceID)
	if err != nil {
		return out, err
	}
	if !allowed {
		return out, ErrNotFound
	}
	return out, nil
}

func (s *Store) CloseTestLabSession(ctx context.Context, id, actor, cloud string) error {
	_, err := s.db.Exec(ctx, `UPDATE test_lab_sessions SET revoked_at=now() WHERE id::text=$1 AND user_id::text=$2 AND brand_cloud_id::text=$3 AND revoked_at IS NULL`, id, actor, cloud)
	return err
}
