package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kevinhuang/rtk_account_manager/internal/model"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db *pgxpool.Pool
}

func New(db *pgxpool.Pool) *Store {
	return &Store{db: db}
}

type RegisterInput struct {
	Email            string
	PasswordHash     string
	DisplayName      *string
	OrganizationName string
}

type RegisterResult struct {
	User         model.User         `json:"user"`
	Organization model.Organization `json:"organization"`
}

func (s *Store) Register(ctx context.Context, in RegisterInput) (RegisterResult, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return RegisterResult{}, err
	}
	defer tx.Rollback(ctx)

	var user model.User
	err = tx.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, display_name)
		VALUES ($1, $2, $3)
		RETURNING id::text, email, display_name, created_at, updated_at, disabled_at
	`, in.Email, in.PasswordHash, in.DisplayName).Scan(&user.ID, &user.Email, &user.DisplayName, &user.CreatedAt, &user.UpdatedAt, &user.DisabledAt)
	if err != nil {
		return RegisterResult{}, err
	}

	var org model.Organization
	err = tx.QueryRow(ctx, `
		INSERT INTO organizations (name)
		VALUES ($1)
		RETURNING id::text, name, created_at, updated_at
	`, in.OrganizationName).Scan(&org.ID, &org.Name, &org.CreatedAt, &org.UpdatedAt)
	if err != nil {
		return RegisterResult{}, err
	}
	org.Role = model.RoleOwner

	_, err = tx.Exec(ctx, `
		INSERT INTO organization_members (organization_id, user_id, role)
		VALUES ($1, $2, 'owner')
	`, org.ID, user.ID)
	if err != nil {
		return RegisterResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return RegisterResult{}, err
	}
	return RegisterResult{User: user, Organization: org}, nil
}

func (s *Store) GetUserPassword(ctx context.Context, email string) (model.User, string, error) {
	var user model.User
	var hash string
	err := s.db.QueryRow(ctx, `
		SELECT id::text, email, password_hash, display_name, created_at, updated_at, disabled_at
		FROM users
		WHERE email = $1 AND disabled_at IS NULL
	`, email).Scan(&user.ID, &user.Email, &hash, &user.DisplayName, &user.CreatedAt, &user.UpdatedAt, &user.DisabledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.User{}, "", ErrNotFound
	}
	return user, hash, err
}

func (s *Store) GetUser(ctx context.Context, userID string) (model.User, error) {
	var user model.User
	err := s.db.QueryRow(ctx, `
		SELECT id::text, email, display_name, created_at, updated_at, disabled_at
		FROM users
		WHERE id = $1 AND disabled_at IS NULL
	`, userID).Scan(&user.ID, &user.Email, &user.DisplayName, &user.CreatedAt, &user.UpdatedAt, &user.DisabledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.User{}, ErrNotFound
	}
	return user, err
}

func (s *Store) SaveRefreshToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO refresh_tokens (user_id, token_hash, expires_at)
		VALUES ($1, $2, $3)
	`, userID, tokenHash, expiresAt)
	return err
}

func (s *Store) RefreshTokenActive(ctx context.Context, tokenHash string) (string, error) {
	var userID string
	err := s.db.QueryRow(ctx, `
		SELECT user_id::text
		FROM refresh_tokens
		WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()
	`, tokenHash).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return userID, err
}

func (s *Store) RevokeRefreshToken(ctx context.Context, tokenHash string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE refresh_tokens
		SET revoked_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL
	`, tokenHash)
	return err
}

func (s *Store) ListOrganizations(ctx context.Context, userID string) ([]model.Organization, error) {
	rows, err := s.db.Query(ctx, `
		SELECT o.id::text, o.name, m.role, o.created_at, o.updated_at
		FROM organizations o
		JOIN organization_members m ON m.organization_id = o.id
		WHERE m.user_id = $1
		ORDER BY o.created_at ASC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	orgs := []model.Organization{}
	for rows.Next() {
		var org model.Organization
		if err := rows.Scan(&org.ID, &org.Name, &org.Role, &org.CreatedAt, &org.UpdatedAt); err != nil {
			return nil, err
		}
		orgs = append(orgs, org)
	}
	return orgs, rows.Err()
}

func (s *Store) CreateOrganization(ctx context.Context, userID, name string) (model.Organization, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return model.Organization{}, err
	}
	defer tx.Rollback(ctx)

	var org model.Organization
	err = tx.QueryRow(ctx, `
		INSERT INTO organizations (name)
		VALUES ($1)
		RETURNING id::text, name, created_at, updated_at
	`, name).Scan(&org.ID, &org.Name, &org.CreatedAt, &org.UpdatedAt)
	if err != nil {
		return model.Organization{}, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO organization_members (organization_id, user_id, role)
		VALUES ($1, $2, 'owner')
	`, org.ID, userID)
	if err != nil {
		return model.Organization{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Organization{}, err
	}
	org.Role = model.RoleOwner
	return org, nil
}

func (s *Store) GetOrganization(ctx context.Context, orgID, userID string) (model.Organization, error) {
	var org model.Organization
	err := s.db.QueryRow(ctx, `
		SELECT o.id::text, o.name, m.role, o.created_at, o.updated_at
		FROM organizations o
		JOIN organization_members m ON m.organization_id = o.id
		WHERE o.id = $1 AND m.user_id = $2
	`, orgID, userID).Scan(&org.ID, &org.Name, &org.Role, &org.CreatedAt, &org.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Organization{}, ErrNotFound
	}
	return org, err
}

func (s *Store) GetRole(ctx context.Context, orgID, userID string) (model.Role, error) {
	var role model.Role
	err := s.db.QueryRow(ctx, `
		SELECT role FROM organization_members WHERE organization_id = $1 AND user_id = $2
	`, orgID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return role, err
}

func (s *Store) ListMembers(ctx context.Context, orgID string) ([]model.Member, error) {
	rows, err := s.db.Query(ctx, `
		SELECT m.organization_id::text, m.user_id::text, u.email, u.display_name, m.role, m.created_at, m.updated_at
		FROM organization_members m
		JOIN users u ON u.id = m.user_id
		WHERE m.organization_id = $1
		ORDER BY m.created_at ASC
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	members := []model.Member{}
	for rows.Next() {
		var member model.Member
		if err := rows.Scan(&member.OrganizationID, &member.UserID, &member.Email, &member.DisplayName, &member.Role, &member.CreatedAt, &member.UpdatedAt); err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	return members, rows.Err()
}

func (s *Store) AddMember(ctx context.Context, orgID, email string, role model.Role) (model.Member, error) {
	var member model.Member
	err := s.db.QueryRow(ctx, `
		INSERT INTO organization_members (organization_id, user_id, role)
		SELECT $1, id, $3 FROM users WHERE email = $2 AND disabled_at IS NULL
		RETURNING organization_id::text, user_id::text, role, created_at, updated_at
	`, orgID, email, role).Scan(&member.OrganizationID, &member.UserID, &member.Role, &member.CreatedAt, &member.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Member{}, ErrNotFound
	}
	if err != nil {
		return model.Member{}, err
	}
	user, err := s.GetUser(ctx, member.UserID)
	if err != nil {
		return model.Member{}, err
	}
	member.Email = user.Email
	member.DisplayName = user.DisplayName
	return member, nil
}

func (s *Store) UpdateMemberRole(ctx context.Context, orgID, userID string, role model.Role) (model.Member, error) {
	if role != model.RoleOwner {
		if err := s.ensureNotLastOwner(ctx, orgID, userID); err != nil {
			return model.Member{}, err
		}
	}
	var member model.Member
	err := s.db.QueryRow(ctx, `
		UPDATE organization_members
		SET role = $3, updated_at = now()
		WHERE organization_id = $1 AND user_id = $2
		RETURNING organization_id::text, user_id::text, role, created_at, updated_at
	`, orgID, userID, role).Scan(&member.OrganizationID, &member.UserID, &member.Role, &member.CreatedAt, &member.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Member{}, ErrNotFound
	}
	if err != nil {
		return model.Member{}, err
	}
	user, err := s.GetUser(ctx, userID)
	if err != nil {
		return model.Member{}, err
	}
	member.Email = user.Email
	member.DisplayName = user.DisplayName
	return member, nil
}

func (s *Store) RemoveMember(ctx context.Context, orgID, userID string) error {
	if err := s.ensureNotLastOwner(ctx, orgID, userID); err != nil {
		return err
	}
	tag, err := s.db.Exec(ctx, `
		DELETE FROM organization_members WHERE organization_id = $1 AND user_id = $2
	`, orgID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ensureNotLastOwner(ctx context.Context, orgID, userID string) error {
	var role model.Role
	var ownerCount int
	err := s.db.QueryRow(ctx, `
		SELECT role, (SELECT count(*) FROM organization_members WHERE organization_id = $1 AND role = 'owner')
		FROM organization_members
		WHERE organization_id = $1 AND user_id = $2
	`, orgID, userID).Scan(&role, &ownerCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if role == model.RoleOwner && ownerCount <= 1 {
		return errors.New("last owner cannot be removed or downgraded")
	}
	return nil
}

type DeviceInput struct {
	Name         string
	Category     model.DeviceCategory
	SerialNumber *string
	MACAddress   *string
	Manufacturer *string
	Model        *string
	Metadata     map[string]any
}

func (s *Store) CreateDevice(ctx context.Context, orgID string, in DeviceInput) (model.Device, error) {
	metadata, err := json.Marshal(defaultMetadata(in.Metadata))
	if err != nil {
		return model.Device{}, err
	}
	return s.scanDevice(s.db.QueryRow(ctx, `
		INSERT INTO devices (organization_id, name, category, serial_number, mac_address, manufacturer, model, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id::text, organization_id::text, name, category, serial_number, mac_address, manufacturer, model, status, last_seen_at, metadata, created_at, updated_at, disabled_at
	`, orgID, in.Name, in.Category, in.SerialNumber, in.MACAddress, in.Manufacturer, in.Model, metadata))
}

func (s *Store) ListDevices(ctx context.Context, orgID string, limit, offset int) ([]model.Device, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id::text, organization_id::text, name, category, serial_number, mac_address, manufacturer, model, status, last_seen_at, metadata, created_at, updated_at, disabled_at
		FROM devices
		WHERE organization_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, orgID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	devices := []model.Device{}
	for rows.Next() {
		device, err := scanDeviceRows(rows)
		if err != nil {
			return nil, err
		}
		devices = append(devices, device)
	}
	return devices, rows.Err()
}

func (s *Store) GetDevice(ctx context.Context, orgID, deviceID string) (model.Device, error) {
	device, err := s.scanDevice(s.db.QueryRow(ctx, `
		SELECT id::text, organization_id::text, name, category, serial_number, mac_address, manufacturer, model, status, last_seen_at, metadata, created_at, updated_at, disabled_at
		FROM devices
		WHERE organization_id = $1 AND id = $2
	`, orgID, deviceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Device{}, ErrNotFound
	}
	return device, err
}

func (s *Store) UpdateDevice(ctx context.Context, orgID, deviceID string, in DeviceInput) (model.Device, error) {
	metadata, err := json.Marshal(defaultMetadata(in.Metadata))
	if err != nil {
		return model.Device{}, err
	}
	device, err := s.scanDevice(s.db.QueryRow(ctx, `
		UPDATE devices
		SET name = $3, category = $4, serial_number = $5, mac_address = $6, manufacturer = $7, model = $8, metadata = $9, updated_at = now()
		WHERE organization_id = $1 AND id = $2
		RETURNING id::text, organization_id::text, name, category, serial_number, mac_address, manufacturer, model, status, last_seen_at, metadata, created_at, updated_at, disabled_at
	`, orgID, deviceID, in.Name, in.Category, in.SerialNumber, in.MACAddress, in.Manufacturer, in.Model, metadata))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Device{}, ErrNotFound
	}
	return device, err
}

func (s *Store) DeleteDevice(ctx context.Context, orgID, deviceID string) error {
	tag, err := s.db.Exec(ctx, `
		DELETE FROM devices WHERE organization_id = $1 AND id = $2
	`, orgID, deviceID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) UpdateDeviceStatus(ctx context.Context, orgID, deviceID string, status model.DeviceStatus, lastSeenAt *time.Time) (model.Device, error) {
	device, err := s.scanDevice(s.db.QueryRow(ctx, `
		UPDATE devices
		SET status = $3, last_seen_at = COALESCE($4, last_seen_at), disabled_at = CASE WHEN $3 = 'disabled' THEN now() ELSE disabled_at END, updated_at = now()
		WHERE organization_id = $1 AND id = $2
		RETURNING id::text, organization_id::text, name, category, serial_number, mac_address, manufacturer, model, status, last_seen_at, metadata, created_at, updated_at, disabled_at
	`, orgID, deviceID, status, lastSeenAt))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Device{}, ErrNotFound
	}
	return device, err
}

func defaultMetadata(metadata map[string]any) map[string]any {
	if metadata == nil {
		return map[string]any{}
	}
	return metadata
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) scanDevice(row rowScanner) (model.Device, error) {
	return scanDevice(row)
}

func scanDeviceRows(rows pgx.Rows) (model.Device, error) {
	return scanDevice(rows)
}

func scanDevice(row rowScanner) (model.Device, error) {
	var device model.Device
	var metadata []byte
	err := row.Scan(
		&device.ID,
		&device.OrganizationID,
		&device.Name,
		&device.Category,
		&device.SerialNumber,
		&device.MACAddress,
		&device.Manufacturer,
		&device.Model,
		&device.Status,
		&device.LastSeenAt,
		&metadata,
		&device.CreatedAt,
		&device.UpdatedAt,
		&device.DisabledAt,
	)
	if err != nil {
		return model.Device{}, err
	}
	if len(metadata) == 0 {
		device.Metadata = map[string]any{}
		return device, nil
	}
	if err := json.Unmarshal(metadata, &device.Metadata); err != nil {
		return model.Device{}, err
	}
	return device, nil
}
