package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"rtk_account_manager/internal/channel"
	"rtk_account_manager/internal/model"
)

type DeviceEntitlementSnapshotInput struct {
	OperationID                  string
	CorrelationID                string
	MessageID                    string
	OrganizationID               string
	DeviceID                     string
	RequestedBy                  string
	TargetProductServiceRevision *int64
	ServiceOptions               []string // nil preserves the current effective set.
	State                        string
	Now                          time.Time
}

type DeviceEntitlementSnapshot struct {
	OrganizationID         string    `json:"organization_id"`
	AccountDeviceID        string    `json:"account_device_id"`
	VideoCloudDevid        string    `json:"video_cloud_devid"`
	Revision               int64     `json:"revision"`
	ProductID              string    `json:"product_id"`
	ProductServiceRevision int64     `json:"product_service_revision"`
	ServiceGrantSHA256     string    `json:"service_grant_sha256"`
	ServiceOptions         []string  `json:"service_options"`
	State                  string    `json:"state"`
	OperationID            string    `json:"operation_id"`
	CreatedBy              string    `json:"created_by"`
	CreatedAt              time.Time `json:"created_at"`
}

type DeviceEntitlementSnapshotResult struct {
	Snapshot  DeviceEntitlementSnapshot
	Operation model.DeviceOperation
	Message   model.DeviceMessageOutbox
	Created   bool
}

// A new revision, audit event, and outbox command commit together. The caller
// may narrow the Product grant or explicitly select its current revision, but
// cannot supply a Product digest or widen beyond the selected Product grant.
func (s *Store) StartDeviceEntitlementSnapshot(ctx context.Context, in DeviceEntitlementSnapshotInput) (DeviceEntitlementSnapshotResult, error) {
	if !s.platformServiceProductWrites || strings.TrimSpace(in.RequestedBy) == "" ||
		strings.TrimSpace(in.OperationID) == "" || strings.TrimSpace(in.CorrelationID) == "" || strings.TrimSpace(in.MessageID) == "" {
		return DeviceEntitlementSnapshotResult{}, ErrConflict
	}
	if in.State != "active" && in.State != "suspended" && in.State != "revoked" {
		return DeviceEntitlementSnapshotResult{}, ErrConflict
	}
	requested := slices.Clone(in.ServiceOptions)
	if requested != nil {
		if len(requested) == 0 || validateProductServiceOptions(requested) != nil {
			return DeviceEntitlementSnapshotResult{}, ErrClaimUnsupportedService
		}
		slices.Sort(requested)
	}
	if in.TargetProductServiceRevision != nil && (*in.TargetProductServiceRevision < 1 || requested == nil) {
		return DeviceEntitlementSnapshotResult{}, ErrConflict
	}
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	}
	in.Now = in.Now.UTC().Truncate(time.Microsecond)
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return DeviceEntitlementSnapshotResult{}, err
	}
	defer tx.Rollback(ctx)
	if err := authorizeDeviceUserMutationTx(ctx, tx, in.RequestedBy, in.OrganizationID, in.DeviceID, "lifecycle_operation.provision"); err != nil {
		return DeviceEntitlementSnapshotResult{}, err
	}
	device, err := getDeviceForUpdateTx(ctx, tx, in.OrganizationID, in.DeviceID)
	if err != nil {
		return DeviceEntitlementSnapshotResult{}, err
	}
	if device.DisabledAt != nil || device.DeviceItemProfileID == nil {
		return DeviceEntitlementSnapshotResult{}, ErrConflict
	}
	if err := authorizeProductUserMutationTx(ctx, tx, in.RequestedBy, in.OrganizationID, *device.DeviceItemProfileID, false); err != nil {
		return DeviceEntitlementSnapshotResult{}, err
	}
	requestPayload := map[string]any{"requested_by": in.RequestedBy, "state": in.State, "service_options": requested,
		"target_product_service_revision": in.TargetProductServiceRevision}
	opInput := DeviceLifecycleOperationInput{OperationID: in.OperationID, CorrelationID: in.CorrelationID, MessageID: in.MessageID,
		OrganizationID: in.OrganizationID, DeviceID: in.DeviceID, OperationType: model.DeviceOperationTypeEntitlementUpdate,
		RequestedBy: &in.RequestedBy, RequestPayload: requestPayload, Now: in.Now}
	operation, created, err := createOrGetDeviceOperationTx(ctx, tx, opInput)
	if err != nil {
		return DeviceEntitlementSnapshotResult{}, err
	}
	if !created {
		snapshot, err := getDeviceEntitlementSnapshotByOperationTx(ctx, tx, in.OperationID)
		if err != nil {
			return DeviceEntitlementSnapshotResult{}, err
		}
		message, err := getLatestOutboxMessageByOperationTx(ctx, tx, in.OperationID)
		if err != nil {
			return DeviceEntitlementSnapshotResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return DeviceEntitlementSnapshotResult{}, err
		}
		return DeviceEntitlementSnapshotResult{Snapshot: snapshot, Operation: operation, Message: message}, nil
	}
	baseline, err := latestDeviceEntitlementBaselineTx(ctx, tx, device)
	if err != nil {
		return DeviceEntitlementSnapshotResult{}, err
	}
	grantRevision := baseline.ProductServiceRevision
	if in.TargetProductServiceRevision != nil {
		grantRevision = *in.TargetProductServiceRevision
	}
	productOptions, bindings, digest, latestRevision, err := productGrantForEntitlementTx(ctx, tx, *device.DeviceItemProfileID, in.OrganizationID, grantRevision)
	if err != nil {
		return DeviceEntitlementSnapshotResult{}, err
	}
	if in.TargetProductServiceRevision != nil && grantRevision != latestRevision ||
		in.TargetProductServiceRevision == nil && digest != baseline.ServiceGrantSHA256 {
		return DeviceEntitlementSnapshotResult{}, ErrConflict
	}
	options := requested
	if options == nil {
		options = slices.Clone(baseline.ServiceOptions)
	}
	if err := validateEffectiveDeviceServices(options, productOptions, bindings); err != nil {
		return DeviceEntitlementSnapshotResult{}, err
	}
	snapshot := DeviceEntitlementSnapshot{OrganizationID: in.OrganizationID, AccountDeviceID: in.DeviceID,
		VideoCloudDevid: baseline.VideoCloudDevid, Revision: baseline.Revision + 1, ProductID: *device.DeviceItemProfileID,
		ProductServiceRevision: grantRevision, ServiceGrantSHA256: digest, ServiceOptions: options, State: in.State,
		OperationID: in.OperationID, CreatedBy: in.RequestedBy, CreatedAt: in.Now}
	optionsJSON, err := json.Marshal(options)
	if err != nil {
		return DeviceEntitlementSnapshotResult{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO device_entitlement_snapshots
		(organization_id,account_device_id,video_cloud_devid,revision,product_id,product_service_revision,
		service_grant_sha256,service_options,entitlement_state,operation_id,created_by,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		snapshot.OrganizationID, snapshot.AccountDeviceID, snapshot.VideoCloudDevid, snapshot.Revision,
		snapshot.ProductID, snapshot.ProductServiceRevision, snapshot.ServiceGrantSHA256, optionsJSON,
		snapshot.State, snapshot.OperationID, snapshot.CreatedBy, snapshot.CreatedAt); err != nil {
		return DeviceEntitlementSnapshotResult{}, err
	}
	opInput.OutboxMessageType = string(channel.MessageTypeDeviceEntitlementSnapshotRequested)
	opInput.OutboxPayload = map[string]any{"org_id": snapshot.OrganizationID, "account_device_id": snapshot.AccountDeviceID,
		"video_cloud_devid": snapshot.VideoCloudDevid, "product_id": snapshot.ProductID,
		"product_service_revision": snapshot.ProductServiceRevision, "service_grant_sha256": snapshot.ServiceGrantSHA256,
		"platform_entitlement_revision": snapshot.Revision, "service_options": snapshot.ServiceOptions,
		"state": snapshot.State, "requested_by": in.RequestedBy}
	message, err := createOutboxMessageTx(ctx, tx, operation, opInput)
	if err != nil {
		return DeviceEntitlementSnapshotResult{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{ActorUserID: &in.RequestedBy, OrganizationID: &in.OrganizationID,
		EventType: "device.entitlement.snapshot.requested", SubjectType: "device", SubjectID: in.DeviceID,
		Payload: map[string]any{"operation_id": in.OperationID, "platform_entitlement_revision": snapshot.Revision,
			"product_service_revision": snapshot.ProductServiceRevision, "state": snapshot.State,
			"service_options": snapshot.ServiceOptions}}); err != nil {
		return DeviceEntitlementSnapshotResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DeviceEntitlementSnapshotResult{}, err
	}
	return DeviceEntitlementSnapshotResult{Snapshot: snapshot, Operation: operation, Message: message, Created: true}, nil
}

func latestDeviceEntitlementBaselineTx(ctx context.Context, tx pgx.Tx, device model.Device) (DeviceEntitlementSnapshot, error) {
	snapshot, err := scanDeviceEntitlementSnapshot(tx.QueryRow(ctx, `SELECT organization_id::text,account_device_id::text,video_cloud_devid,
		revision,product_id::text,product_service_revision,service_grant_sha256,service_options,entitlement_state,
		operation_id,created_by::text,created_at FROM device_entitlement_snapshots
		WHERE organization_id=$1 AND account_device_id=$2 ORDER BY revision DESC LIMIT 1`, device.OrganizationID, device.ID))
	if err == nil {
		if device.DeviceItemProfileID == nil || snapshot.ProductID != *device.DeviceItemProfileID {
			return DeviceEntitlementSnapshot{}, ErrConflict
		}
		return snapshot, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return DeviceEntitlementSnapshot{}, err
	}
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT m.payload FROM device_operations o JOIN device_message_outbox m ON m.operation_id=o.operation_id
		WHERE o.organization_id=$1 AND o.device_id=$2 AND o.operation_type='provision' AND o.status='succeeded'
		AND m.message_type='DeviceProvisionRequested'
		ORDER BY o.completed_at DESC NULLS LAST,o.created_at DESC LIMIT 1`, device.OrganizationID, device.ID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeviceEntitlementSnapshot{}, ErrNotProvisioned
	}
	if err != nil {
		return DeviceEntitlementSnapshot{}, err
	}
	var provision channel.DeviceProvisionRequestedPayload
	if err := json.Unmarshal(raw, &provision); err != nil || provision.ProductServiceRevision == nil ||
		provision.ProductID != *device.DeviceItemProfileID || provision.OrgID != device.OrganizationID ||
		provision.AccountDeviceID != device.ID || provision.ServiceGrantSHA256 == "" ||
		len(provision.ServiceOptions) == 0 || strings.TrimSpace(provision.VideoCloudDevid) == "" {
		return DeviceEntitlementSnapshot{}, ErrConflict
	}
	return DeviceEntitlementSnapshot{OrganizationID: device.OrganizationID, AccountDeviceID: device.ID,
		VideoCloudDevid: provision.VideoCloudDevid, ProductID: provision.ProductID,
		ProductServiceRevision: *provision.ProductServiceRevision, ServiceGrantSHA256: provision.ServiceGrantSHA256,
		ServiceOptions: provision.ServiceOptions, State: "active"}, nil
}

func productGrantForEntitlementTx(ctx context.Context, tx pgx.Tx, productID, cloudID string, revision int64) ([]string, []PlatformServiceCatalogOption, string, int64, error) {
	var optionsJSON, bindingsJSON []byte
	var digest string
	var latest int64
	err := tx.QueryRow(ctx, `SELECT options,bindings,snapshot_sha256,
		(SELECT MAX(revision) FROM product_service_grants WHERE product_id=$1 AND brand_cloud_id=$2)
		FROM product_service_grants WHERE product_id=$1 AND brand_cloud_id=$2 AND revision=$3`, productID, cloudID, revision).
		Scan(&optionsJSON, &bindingsJSON, &digest, &latest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, "", 0, ErrConflict
	}
	if err != nil {
		return nil, nil, "", 0, err
	}
	var options []string
	var bindings []PlatformServiceCatalogOption
	if err := json.Unmarshal(optionsJSON, &options); err != nil || validateProductServiceOptions(options) != nil ||
		json.Unmarshal(bindingsJSON, &bindings) != nil || len(options) == 0 {
		return nil, nil, "", 0, ErrConflict
	}
	return options, bindings, digest, latest, nil
}

func validateEffectiveDeviceServices(effective, product []string, bindings []PlatformServiceCatalogOption) error {
	if len(effective) == 0 || validateProductServiceOptions(effective) != nil {
		return ErrClaimUnsupportedService
	}
	productSet := make(map[string]bool, len(product))
	for _, option := range product {
		productSet[option] = true
	}
	selected := make(map[string]bool, len(effective))
	for _, option := range effective {
		if !productSet[option] {
			return ErrClaimUnsupportedService
		}
		selected[option] = true
	}
	if !selected["mqtt"] {
		return ErrClaimUnsupportedService
	}
	for _, binding := range bindings {
		if selected[binding.Code] {
			for _, dependency := range binding.Requires {
				if !selected[dependency] {
					return ErrClaimUnsupportedService
				}
			}
		}
	}
	return nil
}

func getDeviceEntitlementSnapshotByOperationTx(ctx context.Context, tx pgx.Tx, operationID string) (DeviceEntitlementSnapshot, error) {
	snapshot, err := scanDeviceEntitlementSnapshot(tx.QueryRow(ctx, `SELECT organization_id::text,account_device_id::text,video_cloud_devid,
		revision,product_id::text,product_service_revision,service_grant_sha256,service_options,entitlement_state,
		operation_id,created_by::text,created_at FROM device_entitlement_snapshots WHERE operation_id=$1`, operationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return DeviceEntitlementSnapshot{}, ErrNotFound
	}
	return snapshot, err
}

func scanDeviceEntitlementSnapshot(row rowScanner) (DeviceEntitlementSnapshot, error) {
	var snapshot DeviceEntitlementSnapshot
	var optionsJSON []byte
	err := row.Scan(&snapshot.OrganizationID, &snapshot.AccountDeviceID, &snapshot.VideoCloudDevid,
		&snapshot.Revision, &snapshot.ProductID, &snapshot.ProductServiceRevision, &snapshot.ServiceGrantSHA256,
		&optionsJSON, &snapshot.State, &snapshot.OperationID, &snapshot.CreatedBy, &snapshot.CreatedAt)
	if err != nil {
		return DeviceEntitlementSnapshot{}, err
	}
	if err := json.Unmarshal(optionsJSON, &snapshot.ServiceOptions); err != nil {
		return DeviceEntitlementSnapshot{}, err
	}
	return snapshot, nil
}
