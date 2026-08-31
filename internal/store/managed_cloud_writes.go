package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

var ErrInvalidManagedCloudWrite = errors.New("invalid cloud name, description or idempotency key")

// Pointers preserve PATCH omission; an empty description explicitly clears it.
// There is deliberately no caller-selected slug, owner, status or metadata.
type ManagedCloudWrite struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
}

func ValidManagedCloudKey(key string) bool {
	if len(key) < 1 || len(key) > 200 {
		return false
	}
	for _, c := range key {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

func (in ManagedCloudWrite) normalized(create bool) (ManagedCloudWrite, error) {
	if (create && in.Name == nil) || (in.Name == nil && in.Description == nil) {
		return in, ErrInvalidManagedCloudWrite
	}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if !utf8.ValidString(name) || strings.ContainsRune(name, 0) || utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 255 {
			return in, ErrInvalidManagedCloudWrite
		}
		in.Name = &name
	}
	if in.Description != nil {
		if !utf8.ValidString(*in.Description) || strings.ContainsRune(*in.Description, 0) || utf8.RuneCountInString(*in.Description) > 2000 {
			return in, ErrInvalidManagedCloudWrite
		}
	} else if create {
		empty := ""
		in.Description = &empty
	}
	return in, nil
}

func (s *Store) CreateManagedBrandCloud(ctx context.Context, userID, key string, in ManagedCloudWrite) (ManagedBrandCloud, error) {
	return s.writeManagedBrandCloud(ctx, userID, "", key, in)
}

func (s *Store) UpdateManagedBrandCloud(ctx context.Context, userID, cloudID, key string, in ManagedCloudWrite) (ManagedBrandCloud, error) {
	if cloudID == "" {
		return ManagedBrandCloud{}, ErrNotFound
	}
	return s.writeManagedBrandCloud(ctx, userID, cloudID, key, in)
}

func (s *Store) writeManagedBrandCloud(ctx context.Context, userID, cloudID, key string, in ManagedCloudWrite) (ManagedBrandCloud, error) {
	create := cloudID == ""
	in, err := in.normalized(create)
	if err != nil {
		return ManagedBrandCloud{}, err
	}
	if !ValidManagedCloudKey(key) {
		return ManagedBrandCloud{}, ErrInvalidManagedCloudWrite
	}
	request, err := json.Marshal(in)
	if err != nil {
		return ManagedBrandCloud{}, err
	}
	operation, scope := "create", cloudID
	if !create {
		operation = "update"
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return ManagedBrandCloud{}, err
	}
	defer tx.Rollback(ctx)
	// Match quota/ownership lock order. Actor serialization also collapses
	// concurrent retries before generating a random slug or consuming quota.
	user, err := getDeveloperUserTx(ctx, tx, userID)
	if err != nil {
		return ManagedBrandCloud{}, err
	}
	if !user.EmailVerified || user.SignupPendingVerification {
		return ManagedBrandCloud{}, ErrAccountNotActivated
	}
	if !create {
		if err := lockBrandCloudCollaborationTx(ctx, tx, cloudID, userID); err != nil {
			return ManagedBrandCloud{}, err
		}
		if err := requireBrandCloudOwnerTx(ctx, tx, cloudID, userID); err != nil {
			return ManagedBrandCloud{}, err
		}
	}
	var response []byte
	var same bool
	err = tx.QueryRow(ctx, `SELECT response, request=$5::jsonb FROM managed_cloud_write_receipts
        WHERE actor_user_id=$1 AND operation=$2 AND scope=$3 AND idempotency_key=$4`, userID, operation, scope, key, request).Scan(&response, &same)
	if err == nil {
		if !same {
			return ManagedBrandCloud{}, ErrConflict
		}
		var saved ManagedBrandCloud
		if err := json.Unmarshal(response, &saved); err != nil {
			return ManagedBrandCloud{}, err
		}
		// A receipt is not a reusable authorization grant. Never return old owner
		// capabilities after transfer/removal/deletion or during a lifecycle fence.
		if create {
			if err := lockBrandCloudCollaborationTx(ctx, tx, saved.ID, userID); err != nil {
				return ManagedBrandCloud{}, err
			}
			if err := requireBrandCloudOwnerTx(ctx, tx, saved.ID, userID); err != nil {
				return ManagedBrandCloud{}, err
			}
		}
		current, err := getManagedBrandCloud(ctx, tx, userID, saved.ID)
		if err != nil {
			return ManagedBrandCloud{}, err
		}
		if current.OwnershipVersion != saved.OwnershipVersion {
			return ManagedBrandCloud{}, ErrConflict
		}
		saved.Operational = current.Operational
		return saved, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ManagedBrandCloud{}, err
	}
	event := "developer_brand_cloud_updated"
	if create {
		org, err := createDeveloperBrandCloudTx(ctx, tx, userID, BrandCloudInput{Name: *in.Name}, true)
		if err != nil {
			return ManagedBrandCloud{}, err
		}
		cloudID = org.ID
		event = "developer_brand_cloud_created"
	}
	if _, err := tx.Exec(ctx, `UPDATE organizations SET name=COALESCE($2,name),description=COALESCE($3,description),updated_at=now() WHERE id::text=$1`, cloudID, in.Name, in.Description); err != nil {
		return ManagedBrandCloud{}, err
	}
	cloud, err := getManagedBrandCloud(ctx, tx, userID, cloudID)
	if err != nil {
		return ManagedBrandCloud{}, err
	}
	response, err = json.Marshal(cloud)
	if err != nil {
		return ManagedBrandCloud{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO managed_cloud_write_receipts(actor_user_id,operation,scope,idempotency_key,request,response,cloud_id)
        VALUES($1,$2,$3,$4,$5,$6,$7)`, userID, operation, scope, key, request, response, cloudID); err != nil {
		return ManagedBrandCloud{}, err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: event, ActorUserID: &userID, OrganizationID: &cloudID, SubjectType: "brand_cloud", SubjectID: cloudID,
		Payload: map[string]any{"name": cloud.Name, "description": cloud.Description, "ownership_version": cloud.OwnershipVersion}}); err != nil {
		return ManagedBrandCloud{}, err
	}
	return cloud, tx.Commit(ctx)
}
