package store

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
	"time"
)

type LabAccount struct {
	ID        string    `json:"id"`
	EndUserID string    `json:"end_user_id"`
	Email     string    `json:"email"`
	ExpiresAt time.Time `json:"expires_at"`
}

func authorizeLabCloudTx(ctx context.Context, tx pgx.Tx, actor, cloud string) error {
	if err := lockBrandCloudCollaborationTx(ctx, tx, cloud, actor); err != nil {
		return err
	}
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization_members m WHERE m.user_id::text=$1 AND m.organization_id::text=$2 AND m.disabled_at IS NULL AND m.role<>'viewer' AND user_can_access_brand_cloud($1,$2))`, actor, cloud).Scan(&ok)
	if err == nil && !ok {
		return ErrNotFound
	}
	return err
}

// ConsoleLabAccount reuses the authenticated human identity for dev testing.
// The App identity is internal and passwordless, and cannot adopt an email account.
func (s *Store) ConsoleLabAccount(ctx context.Context, actor, cloud string) (LabAccount, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return LabAccount{}, err
	}
	defer tx.Rollback(ctx)
	if err = authorizeLabCloudTx(ctx, tx, actor, cloud); err != nil {
		return LabAccount{}, err
	}
	var out LabAccount
	if err = tx.QueryRow(ctx, `SELECT email FROM users WHERE id::text=$1`, actor).Scan(&out.Email); err != nil {
		return out, err
	}
	err = tx.QueryRow(ctx, `SELECT m.end_user_id::text FROM test_lab_console_users m JOIN end_users e ON e.id=m.end_user_id WHERE m.user_id::text=$1 AND e.status='active' AND e.disabled_at IS NULL`, actor).Scan(&out.EndUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM test_lab_console_users WHERE user_id::text=$1)`, actor).Scan(&exists); err != nil {
			return out, err
		}
		if exists {
			return out, ErrNotFound
		} // Do not recreate a disabled identity.
		if err = tx.QueryRow(ctx, `INSERT INTO end_users(primary_email,password_hash,display_name,status) VALUES('test-lab-'||gen_random_uuid()::text||'@console.invalid','!','Console Test Lab','active') RETURNING id::text`).Scan(&out.EndUserID); err != nil {
			return out, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO test_lab_console_users(user_id,end_user_id) VALUES($1,$2)`, actor, out.EndUserID); err != nil {
			return out, err
		}
		if err = createAuditEventTx(ctx, tx, AuditEventInput{ActorUserID: &actor, OrganizationID: &cloud, EventType: "test_lab.console_identity_created", SubjectType: "end_user", SubjectID: out.EndUserID}); err != nil {
			return out, err
		}
	} else if err != nil {
		return out, err
	}
	err = tx.QueryRow(ctx, `SELECT id::text FROM test_lab_accounts WHERE user_id::text=$1 AND brand_cloud_id::text=$2 AND end_user_id::text=$3 AND revoked_at IS NULL AND expires_at>now() ORDER BY expires_at DESC LIMIT 1 FOR UPDATE`, actor, cloud, out.EndUserID).Scan(&out.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `INSERT INTO test_lab_accounts(user_id,brand_cloud_id,end_user_id) VALUES($1,$2,$3) RETURNING id::text,expires_at`, actor, cloud, out.EndUserID).Scan(&out.ID, &out.ExpiresAt)
	} else if err == nil {
		err = tx.QueryRow(ctx, `UPDATE test_lab_accounts SET expires_at=now()+interval '30 minutes' WHERE id=$1 RETURNING expires_at`, out.ID).Scan(&out.ExpiresAt)
	}
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

func labAccountTx(ctx context.Context, tx pgx.Tx, actor, cloud, account string) (LabAccount, error) {
	var a LabAccount
	err := tx.QueryRow(ctx, `SELECT a.id::text,a.end_user_id::text,e.primary_email,a.expires_at FROM test_lab_accounts a JOIN end_users e ON e.id=a.end_user_id WHERE a.id::text=$1 AND a.user_id::text=$2 AND a.brand_cloud_id::text=$3 AND a.revoked_at IS NULL AND a.expires_at>now() AND e.disabled_at IS NULL AND e.status='active' FOR UPDATE OF a`, account, actor, cloud).Scan(&a.ID, &a.EndUserID, &a.Email, &a.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return a, err
}

func (s *Store) CloseLabAccount(ctx context.Context, actor, cloud, account string) error {
	_, err := s.db.Exec(ctx, `UPDATE test_lab_accounts SET revoked_at=now() WHERE id::text=$1 AND user_id::text=$2 AND brand_cloud_id::text=$3`, account, actor, cloud)
	return err
}

// Metadata alone is NOT proof of test issuance. Require a completed reservation
// from the existing Developer Console test factory run in this exact Product.
func labDeviceTx(ctx context.Context, tx pgx.Tx, actor, cloud, product, device string) (model.Device, error) {
	if err := authorizeDeviceUserMutationTx(ctx, tx, actor, cloud, device, "registry_device.manage"); err != nil {
		return model.Device{}, err
	}
	d, err := getDeviceForUpdateTx(ctx, tx, cloud, device)
	if err != nil {
		return d, err
	}
	var issued bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM factory_enrollment_reservations r JOIN factory_production_runs p ON p.id=r.production_run_id WHERE r.device_id=$1 AND r.status='issued' AND p.brand_cloud_id::text=$2 AND p.device_item_profile_id::text=$3 AND p.factory_id='developer-console' AND p.batch_id LIKE 'pki-test-%')`, device, cloud, product).Scan(&issued)
	if err != nil {
		return d, err
	}
	if !issued || d.DisabledAt != nil || stringValue(d.DeviceItemProfileID) != product || d.Metadata["purpose"] != "test" {
		return d, ErrNotFound
	}
	return d, nil
}

type LabDevice struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Bound     bool   `json:"bound"`
	Bindable  bool   `json:"bindable"`
	Provision string `json:"provision_status"`
	Status    string `json:"connection_status"`
}

func (s *Store) LabDeviceReady(ctx context.Context, actor, cloud, product, account, device string) (bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	d, err := labDeviceTx(ctx, tx, actor, cloud, product, device)
	if err != nil {
		return false, err
	}
	if err = labBoundTx(ctx, tx, actor, cloud, account, device, false); err != nil {
		return false, err
	}
	return d.Metadata[model.DeviceMetadataVideoCloudActivationStatus] == "activated", nil
}

func (s *Store) ListLabDevices(ctx context.Context, actor, cloud, product, account string, limit, offset int) ([]LabDevice, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err = lockBrandCloudCollaborationTx(ctx, tx, cloud, actor); err != nil {
		return nil, err
	}
	a, err := labAccountTx(ctx, tx, actor, cloud, account)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT d.id::text,d.name,EXISTS(SELECT 1 FROM device_user_bindings b WHERE b.device_id=d.id AND b.end_user_id::text=$4 AND b.disabled_at IS NULL),NOT EXISTS(SELECT 1 FROM device_user_bindings b WHERE b.device_id=d.id AND b.disabled_at IS NULL),COALESCE(d.metadata->>'video_cloud_activation_status','not_provisioned'),d.status FROM devices d WHERE d.organization_id::text=$1 AND d.device_item_profile_id::text=$2 AND d.metadata->>'purpose'='test' AND d.disabled_at IS NULL AND user_can_access_brand_cloud_product($3,$1,$2) AND EXISTS(SELECT 1 FROM factory_enrollment_reservations r JOIN factory_production_runs p ON p.id=r.production_run_id WHERE r.device_id=d.id::text AND r.status='issued' AND p.brand_cloud_id=d.organization_id AND p.device_item_profile_id=d.device_item_profile_id AND p.factory_id='developer-console' AND p.batch_id LIKE 'pki-test-%') ORDER BY d.created_at DESC,d.id LIMIT $5 OFFSET $6`, cloud, product, actor, a.EndUserID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LabDevice{}
	for rows.Next() {
		var d LabDevice
		if err = rows.Scan(&d.ID, &d.Name, &d.Bound, &d.Bindable, &d.Provision, &d.Status); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) LabBindingAction(ctx context.Context, actor, cloud, product, account, device, action, hash string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Use the same cloud -> device -> account order for bind, unbind and provision.
	d, err := labDeviceTx(ctx, tx, actor, cloud, product, device)
	if err != nil {
		return err
	}
	a, err := labAccountTx(ctx, tx, actor, cloud, account)
	if err != nil {
		return err
	}
	if action == "grant" {
		var other bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM device_user_bindings WHERE device_id=$1 AND disabled_at IS NULL AND end_user_id::text<>$2)`, d.ID, a.EndUserID).Scan(&other)
		if err != nil {
			return err
		}
		if other {
			return ErrConflict
		}
		_, err = tx.Exec(ctx, `INSERT INTO test_lab_bind_grants(token_hash,account_id,device_id) VALUES($1,$2,$3)`, hash, a.ID, d.ID)
	} else if action == "bind" {
		var consumed *time.Time
		err = tx.QueryRow(ctx, `SELECT consumed_at FROM test_lab_bind_grants WHERE token_hash=$1 AND account_id=$2 AND device_id=$3 AND expires_at>now() FOR UPDATE`, hash, a.ID, d.ID).Scan(&consumed)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var other bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM device_user_bindings WHERE device_id=$1 AND disabled_at IS NULL AND end_user_id::text<>$2)`, d.ID, a.EndUserID).Scan(&other)
		if err != nil {
			return err
		}
		if other {
			return ErrConflict
		}
		_, err = tx.Exec(ctx, `INSERT INTO brand_cloud_end_users(brand_cloud_id,end_user_id,status,last_seen_at) VALUES($1,$2,'active',now()) ON CONFLICT ON CONSTRAINT brand_cloud_end_users_key DO UPDATE SET status='active',last_seen_at=now()`, cloud, a.EndUserID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO device_user_bindings(device_id,brand_cloud_id,end_user_id,role) VALUES($1,$2,$3,'owner') ON CONFLICT ON CONSTRAINT device_user_bindings_device_end_user_key DO UPDATE SET disabled_at=NULL,updated_at=now()`, d.ID, cloud, a.EndUserID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE test_lab_bind_grants SET consumed_at=COALESCE(consumed_at,now()) WHERE token_hash=$1`, hash)
	} else if action == "unbind" {
		if d.Metadata[model.DeviceMetadataVideoCloudActivationStatus] == "pending" {
			return ErrConflict
		}
		_, err = tx.Exec(ctx, `UPDATE device_user_bindings SET disabled_at=COALESCE(disabled_at,now()),updated_at=now() WHERE device_id=$1 AND end_user_id=$2`, d.ID, a.EndUserID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM test_lab_bind_grants g USING test_lab_accounts a WHERE g.account_id=a.id AND g.device_id=$1 AND a.end_user_id=$2`, d.ID, a.EndUserID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE test_lab_sessions t SET revoked_at=now() FROM test_lab_accounts a WHERE t.account_id=a.id AND t.device_id=$1 AND a.end_user_id=$2 AND t.revoked_at IS NULL`, d.ID, a.EndUserID)
	} else {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if err = createAuditEventTx(ctx, tx, AuditEventInput{ActorUserID: &actor, OrganizationID: &cloud, EventType: "test_lab.device_" + action, SubjectType: "device", SubjectID: device, Payload: map[string]any{"end_user_id": a.EndUserID}}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func labBoundTx(ctx context.Context, tx pgx.Tx, actor, cloud, account, device string, ready bool) error {
	a, err := labAccountTx(ctx, tx, actor, cloud, account)
	if err != nil {
		return err
	}
	var ok bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM device_user_bindings b JOIN devices d ON d.id=b.device_id WHERE b.device_id::text=$1 AND b.end_user_id::text=$2 AND b.brand_cloud_id::text=$3 AND b.disabled_at IS NULL AND d.disabled_at IS NULL AND (NOT $4 OR d.metadata->>'video_cloud_activation_status'='activated'))`, device, a.EndUserID, cloud, ready).Scan(&ok)
	if err == nil && !ok {
		return ErrNotFound
	}
	return err
}

func (s *Store) ProvisionLabDevice(ctx context.Context, actor, cloud, product, account, device, operation, activity, publicKey string) (DeviceLifecycleOperationResult, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return DeviceLifecycleOperationResult{}, err
	}
	defer tx.Rollback(ctx)
	d, err := labDeviceTx(ctx, tx, actor, cloud, product, device)
	if err != nil {
		return DeviceLifecycleOperationResult{}, err
	}
	if err = labBoundTx(ctx, tx, actor, cloud, account, device, false); err != nil {
		return DeviceLifecycleOperationResult{}, err
	}
	allowed, err := hasUserPermissionForResource(ctx, tx, actor, cloud, "lifecycle_operation.provision", ScopeTypeDevice, device)
	if err != nil {
		return DeviceLifecycleOperationResult{}, err
	}
	if !allowed {
		return DeviceLifecycleOperationResult{}, ErrNotFound
	}
	profile, err := getDeviceItemProfileByID(ctx, tx, product)
	if err != nil {
		return DeviceLifecycleOperationResult{}, err
	}
	// Never change activation keys for an already activated or pending device.
	state, _ := d.Metadata[model.DeviceMetadataVideoCloudActivationStatus].(string)
	var replay bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM device_operations WHERE operation_id=$1 AND device_id::text=$2 AND organization_id::text=$3 AND operation_type='provision')`, operation, device, cloud).Scan(&replay); err != nil {
		return DeviceLifecycleOperationResult{}, err
	}
	if (state == "activated" || state == "pending") && !replay {
		return DeviceLifecycleOperationResult{}, ErrConflict
	}
	if saved, _ := d.Metadata[model.DeviceMetadataVideoCloudClipPublicKey].(string); saved != "" && saved != publicKey {
		return DeviceLifecycleOperationResult{}, ErrConflict
	}
	if saved, _ := d.Metadata[model.DeviceMetadataVideoCloudActivityID].(string); saved != "" && saved != activity {
		return DeviceLifecycleOperationResult{}, ErrConflict
	}
	in := DeviceLifecycleOperationInput{OperationID: operation, CorrelationID: operation, MessageID: operation, OrganizationID: cloud, DeviceID: device, OperationType: model.DeviceOperationTypeProvision, RequestedBy: &actor,
		RequestPayload:    map[string]any{"video_cloud_devid": device, "activity_id": activity, "clip_public_key": publicKey, "service_options": profile.ServiceOptions},
		OutboxMessageType: string(channel.MessageTypeDeviceProvisionRequested), OutboxPayload: map[string]any{"org_id": cloud, "account_device_id": device, "video_cloud_devid": device, "activity_id": activity, "clip_public_key": publicKey, "service_options": profile.ServiceOptions, "requested_by": actor}, MetadataPatch: PendingProvisionMetadata(device, activity, publicKey, profile.ServiceOptions), Now: time.Now().UTC()}
	return startDeviceLifecycleOperationTx(ctx, tx, d, in)
}
