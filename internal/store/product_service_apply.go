package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"rtk_account_manager/internal/model"
)

type ProductServiceApplyBlocker struct {
	DeviceID string `json:"device_id"`
	Code     string `json:"code"`
}

var productApplyJobIDPattern = regexp.MustCompile(`^job-[0-9a-f]{24}$`)

type ProductServiceApplyPreview struct {
	PreviewToken   string                       `json:"preview_token"`
	TargetRevision int64                        `json:"target_revision"`
	TargetDigest   string                       `json:"target_digest"`
	TotalDevices   int                          `json:"total_devices"`
	AddedCount     int                          `json:"added_count"`
	RemovedCount   int                          `json:"removed_count"`
	AddedOptions   []string                     `json:"added_options"`
	RemovedOptions []string                     `json:"removed_options"`
	Blockers       []ProductServiceApplyBlocker `json:"blockers"`
	devices        []productServiceApplyBaseline
	options        []string
	retention      *int
}

type productServiceApplyBaseline struct {
	DeviceID        string   `json:"device_id"`
	VideoCloudDevid string   `json:"video_cloud_devid"`
	Revision        int64    `json:"revision"`
	ProductRevision int64    `json:"product_revision"`
	Digest          string   `json:"digest"`
	Options         []string `json:"options"`
	State           string   `json:"state"`
}

type ProductServiceApplyJob struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	ProductID      string    `json:"product_id"`
	TargetRevision int64     `json:"target_revision"`
	TargetDigest   string    `json:"target_digest"`
	TotalDevices   int       `json:"total_devices"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
}

type ProductServiceApplyItem struct {
	DeviceID         string `json:"device_id"`
	OperationID      string `json:"operation_id"`
	Status           string `json:"status"`
	BaselineRevision int64  `json:"baseline_revision"`
	BaselineDigest   string `json:"baseline_digest"`
	BaselineState    string `json:"baseline_state"`
	AppliedRevision  *int64 `json:"applied_revision,omitempty"`
	Retryable        bool   `json:"retryable"`
	ErrorCode        string `json:"error_code,omitempty"`
}

type ProductServiceApplyItemPage struct {
	Items []ProductServiceApplyItem `json:"items"`
	Total int                       `json:"total"`
}

func (s *Store) PreviewProductServiceApply(ctx context.Context, actor, cloud, product string) (ProductServiceApplyPreview, error) {
	if !s.platformServiceProductWrites {
		return ProductServiceApplyPreview{}, ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return ProductServiceApplyPreview{}, err
	}
	defer tx.Rollback(ctx)
	if err := authorizeProductUserMutationTx(ctx, tx, actor, cloud, product, false); err != nil {
		return ProductServiceApplyPreview{}, err
	}
	preview, err := previewProductServiceApplyTx(ctx, tx, cloud, product)
	if err != nil {
		return ProductServiceApplyPreview{}, err
	}
	return preview, tx.Commit(ctx)
}

func previewProductServiceApplyTx(ctx context.Context, tx pgx.Tx, cloud, product string) (ProductServiceApplyPreview, error) {
	var profileStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM device_item_profiles WHERE brand_cloud_id=$1 AND id=$2 AND disabled_at IS NULL`, cloud, product).Scan(&profileStatus); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ProductServiceApplyPreview{}, ErrNotFound
		}
		return ProductServiceApplyPreview{}, err
	}
	if profileStatus != "active" {
		return ProductServiceApplyPreview{}, ErrConflict
	}
	var optionsJSON, bindingsJSON []byte
	var revision int64
	var digest string
	var retention *int
	err := tx.QueryRow(ctx, `SELECT revision,options,bindings,snapshot_sha256,log_retention_days
		FROM product_service_grants WHERE brand_cloud_id=$1 AND product_id=$2 ORDER BY revision DESC LIMIT 1`, cloud, product).
		Scan(&revision, &optionsJSON, &bindingsJSON, &digest, &retention)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProductServiceApplyPreview{}, ErrConflict
	}
	if err != nil {
		return ProductServiceApplyPreview{}, err
	}
	var options []string
	var bindings []PlatformServiceCatalogOption
	if json.Unmarshal(optionsJSON, &options) != nil || json.Unmarshal(bindingsJSON, &bindings) != nil {
		return ProductServiceApplyPreview{}, ErrConflict
	}
	_, _, expected, err := encodeProductServiceGrantWithRetention(options, bindings, retention)
	if err != nil || expected != digest || !slices.Contains(options, "mqtt") {
		return ProductServiceApplyPreview{}, ErrConflict
	}
	preview := ProductServiceApplyPreview{TargetRevision: revision, TargetDigest: digest,
		AddedOptions: []string{}, RemovedOptions: []string{}, Blockers: []ProductServiceApplyBlocker{},
		devices: []productServiceApplyBaseline{}, options: options, retention: retention}
	rows, err := tx.Query(ctx, `SELECT id::text FROM devices WHERE organization_id=$1 AND device_item_profile_id=$2 AND disabled_at IS NULL ORDER BY id`, cloud, product)
	if err != nil {
		return ProductServiceApplyPreview{}, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return ProductServiceApplyPreview{}, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return ProductServiceApplyPreview{}, err
	}
	rows.Close()
	preview.TotalDevices = len(ids)
	added, removed := map[string]bool{}, map[string]bool{}
	for _, id := range ids {
		device, err := getDeviceForUpdateTx(ctx, tx, cloud, id)
		if err != nil {
			return ProductServiceApplyPreview{}, err
		}
		var conflicting bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM device_operations WHERE organization_id=$1 AND device_id=$2
			AND status NOT IN ('succeeded','failed'))`, cloud, id).Scan(&conflicting); err != nil {
			return ProductServiceApplyPreview{}, err
		}
		if conflicting {
			preview.Blockers = append(preview.Blockers, ProductServiceApplyBlocker{id, "conflicting_operation"})
			continue
		}
		baseline, err := latestDeviceEntitlementBaselineTx(ctx, tx, device)
		if err != nil {
			preview.Blockers = append(preview.Blockers, ProductServiceApplyBlocker{id, "untrusted_entitlement"})
			continue
		}
		if baseline.OperationID != "" {
			var status string
			if err := tx.QueryRow(ctx, `SELECT status FROM device_operations WHERE operation_id=$1`, baseline.OperationID).Scan(&status); err != nil || status != "succeeded" {
				preview.Blockers = append(preview.Blockers, ProductServiceApplyBlocker{id, "unapplied_entitlement"})
				continue
			}
		}
		identity, ok := lifecycleMetadataString(device.Metadata, model.DeviceMetadataVideoCloudDevid)
		if !ok || identity != baseline.VideoCloudDevid {
			preview.Blockers = append(preview.Blockers, ProductServiceApplyBlocker{id, "identity_conflict"})
			continue
		}
		grantOptions, grantBindings, grantDigest, grantRetention, _, err := productGrantForEntitlementTx(ctx, tx, product, cloud, baseline.ProductServiceRevision)
		if err != nil {
			preview.Blockers = append(preview.Blockers, ProductServiceApplyBlocker{id, "missing_grant"})
			continue
		}
		_, _, verified, err := encodeProductServiceGrantWithRetention(grantOptions, grantBindings, grantRetention)
		if err != nil || verified != grantDigest || grantDigest != baseline.ServiceGrantSHA256 ||
			validateHistoricalDeviceServices(baseline.ServiceOptions, grantOptions, grantBindings) != nil {
			preview.Blockers = append(preview.Blockers, ProductServiceApplyBlocker{id, "untrusted_entitlement"})
			continue
		}
		if baseline.State != "active" && baseline.State != "suspended" && baseline.State != "revoked" {
			preview.Blockers = append(preview.Blockers, ProductServiceApplyBlocker{id, "untrusted_state"})
			continue
		}
		old, target := map[string]bool{}, map[string]bool{}
		for _, code := range baseline.ServiceOptions {
			old[code] = true
		}
		for _, code := range options {
			target[code] = true
			if !old[code] {
				added[code] = true
				preview.AddedCount++
			}
		}
		for _, code := range baseline.ServiceOptions {
			if !target[code] {
				removed[code] = true
				preview.RemovedCount++
			}
		}
		preview.devices = append(preview.devices, productServiceApplyBaseline{id, baseline.VideoCloudDevid, baseline.Revision,
			baseline.ProductServiceRevision, baseline.ServiceGrantSHA256, slices.Clone(baseline.ServiceOptions), baseline.State})
	}
	for code := range added {
		preview.AddedOptions = append(preview.AddedOptions, code)
	}
	for code := range removed {
		preview.RemovedOptions = append(preview.RemovedOptions, code)
	}
	slices.Sort(preview.AddedOptions)
	slices.Sort(preview.RemovedOptions)
	raw, err := json.Marshal(struct {
		Cloud     string
		Product   string
		Revision  int64
		Digest    string
		Retention *int
		Total     int
		Devices   []productServiceApplyBaseline
		Blockers  []ProductServiceApplyBlocker
	}{cloud, product, revision, digest, retention, len(ids), preview.devices, preview.Blockers})
	if err != nil {
		return ProductServiceApplyPreview{}, err
	}
	sum := sha256.Sum256(raw)
	preview.PreviewToken = hex.EncodeToString(sum[:])
	return preview, nil
}

func (s *Store) AdmitProductServiceApply(ctx context.Context, actor, cloud, product, jobID, previewToken string) (ProductServiceApplyJob, error) {
	if !s.platformServiceProductWrites || !productApplyJobIDPattern.MatchString(jobID) || len(previewToken) != 64 {
		return ProductServiceApplyJob{}, ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ProductServiceApplyJob{}, err
	}
	defer tx.Rollback(ctx)
	if err := authorizeProductUserMutationTx(ctx, tx, actor, cloud, product, false); err != nil {
		return ProductServiceApplyJob{}, err
	}
	// Existing job retries are idempotent even if the Product has since changed.
	job, err := getProductServiceApplyJobTx(ctx, tx, cloud, product, jobID)
	if err == nil {
		var storedToken string
		if err := tx.QueryRow(ctx, `SELECT preview_token FROM product_service_apply_jobs WHERE id=$1`, jobID).Scan(&storedToken); err != nil {
			return ProductServiceApplyJob{}, err
		}
		if storedToken != previewToken {
			return ProductServiceApplyJob{}, ErrConflict
		}
		return job, tx.Commit(ctx)
	}
	if !errors.Is(err, ErrNotFound) {
		return ProductServiceApplyJob{}, err
	}
	// Freeze membership during admission; the transaction saves the exact member set.
	if _, err := tx.Exec(ctx, `LOCK TABLE devices IN SHARE MODE`); err != nil {
		return ProductServiceApplyJob{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM device_item_profiles WHERE brand_cloud_id=$1 AND id=$2 FOR UPDATE`, cloud, product); err != nil {
		return ProductServiceApplyJob{}, err
	}
	preview, err := previewProductServiceApplyTx(ctx, tx, cloud, product)
	if err != nil {
		return ProductServiceApplyJob{}, err
	}
	if preview.PreviewToken != previewToken || len(preview.Blockers) != 0 {
		return ProductServiceApplyJob{}, ErrConflict
	}
	optionsJSON, err := json.Marshal(preview.options)
	if err != nil {
		return ProductServiceApplyJob{}, err
	}
	job, err = scanProductServiceApplyJob(tx.QueryRow(ctx, `INSERT INTO product_service_apply_jobs
		(id,organization_id,product_id,actor_user_id,target_revision,target_digest,target_options,target_log_retention_days,preview_token,total_devices)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING id::text,organization_id::text,product_id::text,target_revision,target_digest,total_devices,status,created_at`,
		jobID, cloud, product, actor, preview.TargetRevision, preview.TargetDigest, optionsJSON, preview.retention, previewToken, preview.TotalDevices))
	if err != nil {
		return ProductServiceApplyJob{}, err
	}
	for _, device := range preview.devices {
		options, err := json.Marshal(device.Options)
		if err != nil {
			return ProductServiceApplyJob{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO product_service_apply_items
			(job_id,device_id,operation_id,baseline_revision,baseline_product_revision,baseline_digest,baseline_options,baseline_state,video_cloud_devid)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, jobID, device.DeviceID, jobID+"/"+device.DeviceID,
			device.Revision, device.ProductRevision, device.Digest, options, device.State, device.VideoCloudDevid); err != nil {
			return ProductServiceApplyJob{}, err
		}
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{ActorUserID: &actor, OrganizationID: &cloud,
		EventType: "product.services.apply.started", SubjectType: "device_item_profile", SubjectID: product,
		Payload: map[string]any{"job_id": jobID, "target_revision": preview.TargetRevision, "target_digest": preview.TargetDigest,
			"total_devices": preview.TotalDevices}}); err != nil {
		return ProductServiceApplyJob{}, err
	}
	return job, tx.Commit(ctx)
}

func scanProductServiceApplyJob(row rowScanner) (ProductServiceApplyJob, error) {
	var job ProductServiceApplyJob
	err := row.Scan(&job.ID, &job.OrganizationID, &job.ProductID, &job.TargetRevision, &job.TargetDigest,
		&job.TotalDevices, &job.Status, &job.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProductServiceApplyJob{}, ErrNotFound
	}
	return job, err
}

func getProductServiceApplyJobTx(ctx context.Context, tx pgx.Tx, cloud, product, jobID string) (ProductServiceApplyJob, error) {
	return scanProductServiceApplyJob(tx.QueryRow(ctx, `SELECT id::text,organization_id::text,product_id::text,target_revision,target_digest,total_devices,status,created_at
		FROM product_service_apply_jobs WHERE id=$1 AND organization_id=$2 AND product_id=$3`, jobID, cloud, product))
}

func (s *Store) GetProductServiceApplyJob(ctx context.Context, actor, cloud, product, jobID string) (ProductServiceApplyJob, error) {
	if err := s.authorizeProductApplyRead(ctx, actor, cloud, product); err != nil {
		return ProductServiceApplyJob{}, err
	}
	return scanProductServiceApplyJob(s.db.QueryRow(ctx, `SELECT id::text,organization_id::text,product_id::text,target_revision,target_digest,total_devices,status,created_at
		FROM product_service_apply_jobs WHERE id=$1 AND organization_id=$2 AND product_id=$3`, jobID, cloud, product))
}

func (s *Store) authorizeProductApplyRead(ctx context.Context, actor, cloud, product string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := authorizeProductUserMutationTx(ctx, tx, actor, cloud, product, false); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func validateProductApplyItemTx(ctx context.Context, tx pgx.Tx, jobID, cloud, product, deviceID, operationID string, targetRevision *int64, options []string) error {
	var storedRevision int64
	var raw []byte
	var status, storedOperation string
	err := tx.QueryRow(ctx, `SELECT j.target_revision,j.target_options,j.status,i.operation_id
		FROM product_service_apply_jobs j JOIN product_service_apply_items i ON i.job_id=j.id
		WHERE j.id=$1 AND j.organization_id=$2 AND j.product_id=$3 AND i.device_id=$4 FOR UPDATE OF j,i`,
		jobID, cloud, product, deviceID).Scan(&storedRevision, &raw, &status, &storedOperation)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var targetOptions []string
	if json.Unmarshal(raw, &targetOptions) != nil || status != "active" || storedOperation != operationID ||
		targetRevision == nil || *targetRevision != storedRevision || !serviceOptionSetsEqual(options, targetOptions) {
		return ErrConflict
	}
	return nil
}

func validateProductApplyBaselineTx(ctx context.Context, tx pgx.Tx, jobID, deviceID string, baseline DeviceEntitlementSnapshot) error {
	var expectedRevision, expectedProductRevision int64
	var expectedDigest, expectedState, expectedVideo string
	var optionsJSON []byte
	err := tx.QueryRow(ctx, `SELECT baseline_revision,baseline_product_revision,baseline_digest,baseline_options,baseline_state,video_cloud_devid
		FROM product_service_apply_items WHERE job_id=$1 AND device_id=$2`, jobID, deviceID).
		Scan(&expectedRevision, &expectedProductRevision, &expectedDigest, &optionsJSON, &expectedState, &expectedVideo)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var expectedOptions []string
	if json.Unmarshal(optionsJSON, &expectedOptions) != nil || baseline.Revision != expectedRevision ||
		baseline.ProductServiceRevision != expectedProductRevision || baseline.ServiceGrantSHA256 != expectedDigest ||
		baseline.State != expectedState || baseline.VideoCloudDevid != expectedVideo ||
		!serviceOptionSetsEqual(baseline.ServiceOptions, expectedOptions) {
		return ErrConflict
	}
	var conflicting bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM device_operations WHERE device_id=$1
		AND operation_id<>$2 AND status NOT IN ('succeeded','failed'))`, deviceID, jobID+"/"+deviceID).Scan(&conflicting); err != nil {
		return err
	}
	if conflicting {
		return ErrConflict
	}
	return nil
}

func validateProductApplyReplayTx(ctx context.Context, tx pgx.Tx, device model.Device, snapshot DeviceEntitlementSnapshot) error {
	var latest int64
	if err := tx.QueryRow(ctx, `SELECT max(revision) FROM device_entitlement_snapshots WHERE organization_id=$1 AND account_device_id=$2`, device.OrganizationID, device.ID).Scan(&latest); err != nil {
		return err
	}
	if latest != snapshot.Revision {
		return ErrConflict
	}
	identity, ok := lifecycleMetadataString(device.Metadata, model.DeviceMetadataVideoCloudDevid)
	if !ok || identity != snapshot.VideoCloudDevid {
		return ErrConflict
	}
	if status, _ := lifecycleMetadataString(device.Metadata, model.DeviceMetadataVideoCloudActivationStatus); status == string(model.VideoCloudActivationStatusDeactivated) && snapshot.State == "active" {
		return ErrConflict
	}
	var conflicting bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM device_operations WHERE organization_id=$1 AND device_id=$2
		AND operation_id<>$3 AND status NOT IN ('succeeded','failed'))`, device.OrganizationID, device.ID, snapshot.OperationID).Scan(&conflicting); err != nil {
		return err
	}
	if conflicting {
		return ErrConflict
	}
	return nil
}

func (s *Store) DispatchProductServiceApplyItem(ctx context.Context, actor, cloud, product, jobID, deviceID, messageID string) (ProductServiceApplyItem, error) {
	item, err := s.GetProductServiceApplyItem(ctx, actor, cloud, product, jobID, deviceID)
	if err != nil {
		return ProductServiceApplyItem{}, err
	}
	if item.Status == "applied" {
		return item, nil
	}
	if item.Status == "failed" && !item.Retryable {
		return item, ErrConflict
	}
	var targetRevision int64
	var targetOptionsJSON []byte
	err = s.db.QueryRow(ctx, `SELECT target_revision,target_options FROM product_service_apply_jobs WHERE id=$1 AND organization_id=$2 AND product_id=$3 AND status='active'`, jobID, cloud, product).
		Scan(&targetRevision, &targetOptionsJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProductServiceApplyItem{}, ErrConflict
	}
	if err != nil {
		return ProductServiceApplyItem{}, err
	}
	var targetOptions []string
	if err := json.Unmarshal(targetOptionsJSON, &targetOptions); err != nil {
		return ProductServiceApplyItem{}, ErrConflict
	}
	_, err = s.StartDeviceEntitlementSnapshot(ctx, DeviceEntitlementSnapshotInput{
		BatchJobID: jobID, OperationID: item.OperationID, CorrelationID: item.OperationID, MessageID: messageID,
		OrganizationID: cloud, DeviceID: deviceID, RequestedBy: actor, TargetProductServiceRevision: &targetRevision,
		ServiceOptions: targetOptions, State: item.BaselineState, Now: time.Now().UTC(),
	})
	if err != nil {
		return ProductServiceApplyItem{}, err
	}
	if item.Status == "failed" && item.Retryable {
		// Recheck authorization before making the original command dispatchable.
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return ProductServiceApplyItem{}, err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `UPDATE device_operations SET status='retrying',error_code=NULL,error_message=NULL,retryable=NULL,completed_at=NULL
			WHERE operation_id=$1 AND status IN ('failed','dead_lettered') AND retryable=true`, item.OperationID); err != nil {
			return ProductServiceApplyItem{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE device_message_outbox SET status='retrying',attempt_count=0,available_at=now(),last_error=NULL
			WHERE operation_id=$1 AND status IN ('published','retrying','dead_lettered')`, item.OperationID); err != nil {
			return ProductServiceApplyItem{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return ProductServiceApplyItem{}, err
		}
	}
	if _, err := s.db.Exec(ctx, `UPDATE product_service_apply_items SET dispatched_at=COALESCE(dispatched_at,now()) WHERE job_id=$1 AND device_id=$2`, jobID, deviceID); err != nil {
		return ProductServiceApplyItem{}, err
	}
	return s.GetProductServiceApplyItem(ctx, actor, cloud, product, jobID, deviceID)
}

func (s *Store) GetProductServiceApplyItem(ctx context.Context, actor, cloud, product, jobID, deviceID string) (ProductServiceApplyItem, error) {
	if err := s.authorizeProductApplyRead(ctx, actor, cloud, product); err != nil {
		return ProductServiceApplyItem{}, err
	}
	return scanProductServiceApplyItem(s.db.QueryRow(ctx, productApplyItemSelect+` WHERE j.id=$1 AND j.organization_id=$2 AND j.product_id=$3 AND i.device_id=$4`, jobID, cloud, product, deviceID))
}

const productApplyItemSelect = `SELECT i.device_id::text,i.operation_id,i.baseline_revision,i.baseline_digest,i.baseline_state,
	COALESCE(o.status,''),COALESCE(o.retryable,false),COALESCE(o.error_code,''),
	CASE WHEN s.revision IS NOT NULL AND o.status='succeeded' AND o.result_payload->>'platform_entitlement_revision'=s.revision::text
		AND s.product_service_revision=j.target_revision AND s.service_grant_sha256=j.target_digest THEN j.target_revision ELSE NULL END
	FROM product_service_apply_items i JOIN product_service_apply_jobs j ON j.id=i.job_id
	LEFT JOIN device_operations o ON o.operation_id=i.operation_id
	LEFT JOIN device_entitlement_snapshots s ON s.operation_id=i.operation_id`

func scanProductServiceApplyItem(row rowScanner) (ProductServiceApplyItem, error) {
	var item ProductServiceApplyItem
	var operationStatus string
	err := row.Scan(&item.DeviceID, &item.OperationID, &item.BaselineRevision, &item.BaselineDigest,
		&item.BaselineState, &operationStatus, &item.Retryable, &item.ErrorCode, &item.AppliedRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProductServiceApplyItem{}, ErrNotFound
	}
	if err != nil {
		return ProductServiceApplyItem{}, err
	}
	switch {
	case item.AppliedRevision != nil:
		item.Status = "applied"
	case operationStatus == "":
		item.Status = "pending"
	case operationStatus == "failed" || operationStatus == "dead_lettered" || operationStatus == "succeeded":
		item.Status = "failed"
	default:
		item.Status = "accepted"
	}
	return item, nil
}

func (s *Store) ListProductServiceApplyItems(ctx context.Context, actor, cloud, product, jobID string, limit, offset int) (ProductServiceApplyItemPage, error) {
	if err := s.authorizeProductApplyRead(ctx, actor, cloud, product); err != nil {
		return ProductServiceApplyItemPage{}, err
	}
	if limit < 1 {
		limit = 100
	}
	if limit > 250 {
		limit = 250
	}
	if offset < 0 {
		offset = 0
	}
	var total int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM product_service_apply_items i JOIN product_service_apply_jobs j ON j.id=i.job_id
		WHERE j.id=$1 AND j.organization_id=$2 AND j.product_id=$3`, jobID, cloud, product).Scan(&total); err != nil {
		return ProductServiceApplyItemPage{}, err
	}
	rows, err := s.db.Query(ctx, productApplyItemSelect+` WHERE j.id=$1 AND j.organization_id=$2 AND j.product_id=$3 ORDER BY i.device_id LIMIT $4 OFFSET $5`, jobID, cloud, product, limit, offset)
	if err != nil {
		return ProductServiceApplyItemPage{}, err
	}
	defer rows.Close()
	page := ProductServiceApplyItemPage{Items: []ProductServiceApplyItem{}, Total: total}
	for rows.Next() {
		item, err := scanProductServiceApplyItem(rows)
		if err != nil {
			return ProductServiceApplyItemPage{}, err
		}
		page.Items = append(page.Items, item)
	}
	return page, rows.Err()
}

func (s *Store) FinishProductServiceApply(ctx context.Context, actor, cloud, product, jobID, action string) (ProductServiceApplyJob, error) {
	if action != "cancel" && action != "complete" {
		return ProductServiceApplyJob{}, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return ProductServiceApplyJob{}, err
	}
	defer tx.Rollback(ctx)
	if err := authorizeProductUserMutationTx(ctx, tx, actor, cloud, product, false); err != nil {
		return ProductServiceApplyJob{}, err
	}
	job, err := getProductServiceApplyJobTx(ctx, tx, cloud, product, jobID)
	if err != nil {
		return ProductServiceApplyJob{}, err
	}
	if job.Status == "canceled" && action == "cancel" || job.Status == "completed" && action == "complete" {
		return job, tx.Commit(ctx)
	}
	if job.Status != "active" {
		return ProductServiceApplyJob{}, ErrConflict
	}
	if action == "complete" {
		var remaining int
		err := tx.QueryRow(ctx, `SELECT count(*) FROM product_service_apply_items i
			LEFT JOIN device_operations o ON o.operation_id=i.operation_id
			LEFT JOIN device_entitlement_snapshots s ON s.operation_id=i.operation_id
			WHERE i.job_id=$1 AND NOT (o.status='succeeded' AND s.revision IS NOT NULL
				AND o.result_payload->>'platform_entitlement_revision'=s.revision::text
				AND s.product_service_revision=$2 AND s.service_grant_sha256=$3)`, jobID, job.TargetRevision, job.TargetDigest).Scan(&remaining)
		if err != nil {
			return ProductServiceApplyJob{}, err
		}
		if remaining != 0 {
			return ProductServiceApplyJob{}, ErrConflict
		}
	}
	newStatus := "completed"
	if action == "cancel" {
		newStatus = "canceled"
	}
	job, err = scanProductServiceApplyJob(tx.QueryRow(ctx, `UPDATE product_service_apply_jobs SET status=$4,finished_at=now(),updated_at=now()
		WHERE id=$1 AND organization_id=$2 AND product_id=$3 AND status='active'
		RETURNING id::text,organization_id::text,product_id::text,target_revision,target_digest,total_devices,status,created_at`, jobID, cloud, product, newStatus))
	if err != nil {
		return ProductServiceApplyJob{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{ActorUserID: &actor, OrganizationID: &cloud,
		EventType: "product.services.apply." + job.Status, SubjectType: "device_item_profile", SubjectID: product,
		Payload: map[string]any{"job_id": jobID, "target_revision": job.TargetRevision}}); err != nil {
		return ProductServiceApplyJob{}, err
	}
	return job, tx.Commit(ctx)
}
