package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const PlatformServiceLeaseDuration = 90 * time.Second
const PlatformServiceHeartbeatInterval = 30 * time.Second

var (
	serviceIDPattern              = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	instanceIDPattern             = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	serviceOptionPattern          = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	ErrServiceRegistrationInvalid = errors.New("invalid service registration")
	ErrServiceRegistrationDenied  = errors.New("service registration denied")
)

type PlatformServiceOption struct {
	Code             string   `json:"code"`
	DisplayName      string   `json:"display_name"`
	Description      string   `json:"description"`
	Requires         []string `json:"requires,omitempty"`
	LogRetentionDays []int    `json:"log_retention_days,omitempty"`
}

type PlatformServiceRegistration struct {
	RequestID       string                  `json:"request_id"`
	ServiceID       string                  `json:"service_id"`
	InstanceID      string                  `json:"instance_id"`
	ManifestVersion string                  `json:"manifest_version"`
	ProtocolVersion string                  `json:"protocol_version"`
	Options         []PlatformServiceOption `json:"options"`
	EndpointRef     string                  `json:"endpoint_ref"`
	Ready           bool                    `json:"ready"`
}

type PlatformServicePrincipal struct {
	Environment        string
	CertificateSubject string
	IssuerFingerprint  string
}

type PlatformServiceLease struct {
	ServiceID        string    `json:"service_id"`
	InstanceID       string    `json:"instance_id"`
	ManifestVersion  string    `json:"manifest_version"`
	CatalogRevision  int64     `json:"catalog_revision"`
	LeaseExpiresAt   time.Time `json:"lease_expires_at"`
	HeartbeatSeconds int       `json:"heartbeat_seconds"`
}

type PlatformServiceCatalogOption struct {
	PlatformServiceOption
	ServiceID         string `json:"service_id"`
	ManifestVersion   string `json:"manifest_version"`
	Selectable        bool   `json:"selectable"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

type PlatformServiceCatalog struct {
	CatalogRevision int64                          `json:"catalog_revision"`
	Options         []PlatformServiceCatalogOption `json:"options"`
}

type PlatformServiceWorkloadApproval struct {
	Environment        string   `json:"-"`
	ServiceID          string   `json:"service_id"`
	InstanceID         string   `json:"instance_id"`
	CertificateSubject string   `json:"certificate_subject"`
	IssuerFingerprint  string   `json:"issuer_fingerprint_sha256"`
	AllowedOptionCodes []string `json:"allowed_option_codes"`
	ApprovedBy         string   `json:"-"`
}

type PlatformServiceWorkloadRevocation struct {
	Environment        string `json:"-"`
	RevokedBy          string `json:"-"`
	ServiceID          string `json:"service_id"`
	InstanceID         string `json:"instance_id"`
	CertificateSubject string `json:"certificate_subject"`
	IssuerFingerprint  string `json:"issuer_fingerprint_sha256"`
}

func ValidatePlatformServiceRegistration(r PlatformServiceRegistration) error {
	if !serviceIDPattern.MatchString(r.ServiceID) || !instanceIDPattern.MatchString(r.InstanceID) ||
		len(r.RequestID) == 0 || len(r.RequestID) > 128 || len(r.ManifestVersion) == 0 || len(r.ManifestVersion) > 64 ||
		r.ProtocolVersion != "1" || len(r.EndpointRef) == 0 || len(r.EndpointRef) > 128 ||
		len(r.Options) == 0 || len(r.Options) > 64 {
		return ErrServiceRegistrationInvalid
	}
	seen := make(map[string]bool, len(r.Options))
	for _, option := range r.Options {
		if !serviceOptionPattern.MatchString(option.Code) || len(option.DisplayName) == 0 || len(option.DisplayName) > 120 || len(option.Description) > 1000 || seen[option.Code] {
			return ErrServiceRegistrationInvalid
		}
		seen[option.Code] = true
		deps := map[string]bool{}
		for _, dependency := range option.Requires {
			if !serviceOptionPattern.MatchString(dependency) || dependency == option.Code || deps[dependency] {
				return ErrServiceRegistrationInvalid
			}
			deps[dependency] = true
		}
		if option.Code == "mqtt" && r.ServiceID != "mqtt" {
			return ErrServiceRegistrationDenied
		}
		if option.Code == "device_logging" && r.ServiceID != "logger" {
			return ErrServiceRegistrationDenied
		}
		if option.Code == "ota" && r.ServiceID != "ota" {
			return ErrServiceRegistrationDenied
		}
		if r.ServiceID == "ota" && option.Code != "ota" {
			return ErrServiceRegistrationInvalid
		}
		if r.ServiceID == "ota" && !slices.Equal(option.Requires, []string{"mqtt"}) {
			return ErrServiceRegistrationInvalid
		}
		if r.ServiceID == "logger" && option.Code == "device_logging" {
			if !slices.Equal(option.LogRetentionDays, []int{7, 30, 90}) {
				return ErrServiceRegistrationInvalid
			}
		} else if len(option.LogRetentionDays) != 0 {
			return ErrServiceRegistrationInvalid
		}
		if option.Code == "mqtt" && len(option.Requires) != 0 {
			return ErrServiceRegistrationInvalid
		}
		if r.ServiceID == "mqtt" && option.Code != "mqtt" {
			return ErrServiceRegistrationInvalid
		}
	}
	if r.ServiceID == "mqtt" && !seen["mqtt"] {
		return ErrServiceRegistrationInvalid
	}
	return nil
}

func validateProductServiceOptions(options []string) error {
	if len(options) > 64 {
		return ErrClaimUnsupportedService
	}
	seen := make(map[string]bool, len(options))
	for _, option := range options {
		if !serviceOptionPattern.MatchString(option) || seen[option] {
			return ErrClaimUnsupportedService
		}
		seen[option] = true
	}
	return nil
}

func ValidateProductServiceOptionCodes(options []string) error {
	return validateProductServiceOptions(options)
}

func (s *Store) validateProductServiceSelectionTx(ctx context.Context, tx pgx.Tx, options []string, observedRevision int64, now time.Time) ([]PlatformServiceCatalogOption, error) {
	if !s.platformServiceProductWrites {
		return nil, nil
	}
	if !slices.Contains(options, "mqtt") || observedRevision < 1 {
		return nil, ErrClaimUnsupportedService
	}
	if _, err := lockCatalogRevision(ctx, tx, s.platformServiceEnvironment); err != nil {
		return nil, err
	}
	catalog, err := readPlatformServiceOptions(ctx, tx, s.platformServiceEnvironment, now)
	if err != nil {
		return nil, err
	}
	if catalog.CatalogRevision != observedRevision {
		return nil, ErrConflict
	}
	return validateProductServiceSelection(options, catalog)
}

func validateProductServiceSelection(options []string, catalog PlatformServiceCatalog) ([]PlatformServiceCatalogOption, error) {
	if err := validateProductServiceOptions(options); err != nil || !slices.Contains(options, "mqtt") {
		return nil, ErrClaimUnsupportedService
	}
	selected := make(map[string]bool, len(options))
	bindings := make([]PlatformServiceCatalogOption, 0, len(options))
	for _, code := range options {
		selected[code] = true
		found := false
		for _, candidate := range catalog.Options {
			if candidate.Code == code {
				if !candidate.Selectable {
					return nil, ErrConflict
				}
				bindings = append(bindings, candidate)
				found = true
				break
			}
		}
		if !found {
			return nil, ErrClaimUnsupportedService
		}
	}
	for _, binding := range bindings {
		for _, dependency := range binding.Requires {
			if !selected[dependency] {
				return nil, ErrClaimUnsupportedService
			}
		}
	}
	slices.SortFunc(bindings, func(a, b PlatformServiceCatalogOption) int { return strings.Compare(a.Code, b.Code) })
	return bindings, nil
}

func insertProductServiceGrantTx(ctx context.Context, tx pgx.Tx, productID, brandCloudID string, options []string, bindings []PlatformServiceCatalogOption, catalogRevision int64, actor *string) error {
	optionsJSON, bindingsJSON, digest, err := encodeProductServiceGrant(options, bindings)
	if err != nil {
		return err
	}
	var revision int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(revision),0)+1 FROM product_service_grants WHERE product_id=$1`, productID).Scan(&revision); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO product_service_grants(product_id,revision,brand_cloud_id,catalog_revision,options,bindings,snapshot_sha256,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, productID, revision, brandCloudID, catalogRevision, optionsJSON, bindingsJSON, digest, actor)
	return err
}

func encodeProductServiceGrant(options []string, bindings []PlatformServiceCatalogOption) ([]byte, []byte, string, error) {
	options = slices.Clone(options)
	slices.Sort(options)
	optionsJSON, err := json.Marshal(options)
	if err != nil {
		return nil, nil, "", err
	}
	bindingsJSON, err := json.Marshal(bindings)
	if err != nil {
		return nil, nil, "", err
	}
	digestData, err := json.Marshal(struct {
		Options  json.RawMessage `json:"options"`
		Bindings json.RawMessage `json:"bindings"`
	}{optionsJSON, bindingsJSON})
	if err != nil {
		return nil, nil, "", err
	}
	digest := sha256.Sum256(digestData)
	return optionsJSON, bindingsJSON, hex.EncodeToString(digest[:]), nil
}

func (s *Store) ApprovePlatformServiceWorkload(ctx context.Context, a PlatformServiceWorkloadApproval) error {
	if a.Environment == "" || !serviceIDPattern.MatchString(a.ServiceID) || !instanceIDPattern.MatchString(a.InstanceID) ||
		a.CertificateSubject == "" || a.ApprovedBy == "" || len(a.IssuerFingerprint) != 64 || len(a.AllowedOptionCodes) == 0 || len(a.AllowedOptionCodes) > 64 || validateProductServiceOptions(a.AllowedOptionCodes) != nil {
		return ErrServiceRegistrationInvalid
	}
	if (a.ServiceID == "mqtt") != slices.Contains(a.AllowedOptionCodes, "mqtt") || a.ServiceID != "mqtt" && slices.Contains(a.AllowedOptionCodes, "mqtt") {
		return ErrServiceRegistrationInvalid
	}
	for _, char := range a.IssuerFingerprint {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return ErrServiceRegistrationInvalid
		}
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `INSERT INTO platform_service_workloads(environment,certificate_subject,issuer_fingerprint_sha256,service_id,instance_id,allowed_option_codes,approved_by)
		VALUES($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT(environment,certificate_subject,issuer_fingerprint_sha256)
		DO UPDATE SET service_id=EXCLUDED.service_id,instance_id=EXCLUDED.instance_id,allowed_option_codes=EXCLUDED.allowed_option_codes,approved_by=EXCLUDED.approved_by,approved_at=now(),revoked_at=NULL
		WHERE platform_service_workloads.service_id=EXCLUDED.service_id AND platform_service_workloads.instance_id=EXCLUDED.instance_id`,
		a.Environment, a.CertificateSubject, a.IssuerFingerprint, a.ServiceID, a.InstanceID, a.AllowedOptionCodes, a.ApprovedBy)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrConflict
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: "platform_service_workload_approved", ActorUserID: &a.ApprovedBy, SubjectType: "platform_service_workload", SubjectID: a.ServiceID + "/" + a.InstanceID, Payload: map[string]any{"environment": a.Environment, "certificate_subject": a.CertificateSubject, "issuer_fingerprint_sha256": a.IssuerFingerprint, "allowed_option_codes": a.AllowedOptionCodes}}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) RevokePlatformServiceWorkload(ctx context.Context, a PlatformServiceWorkloadRevocation, now time.Time) error {
	if a.Environment == "" || a.RevokedBy == "" || !serviceIDPattern.MatchString(a.ServiceID) || !instanceIDPattern.MatchString(a.InstanceID) || a.CertificateSubject == "" || len(a.IssuerFingerprint) != 64 {
		return ErrServiceRegistrationInvalid
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := lockCatalogRevision(ctx, tx, a.Environment); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE platform_service_workloads SET revoked_at=$6 WHERE environment=$1 AND service_id=$2 AND instance_id=$3 AND certificate_subject=$4 AND issuer_fingerprint_sha256=$5 AND revoked_at IS NULL`, a.Environment, a.ServiceID, a.InstanceID, a.CertificateSubject, a.IssuerFingerprint, now)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `UPDATE platform_service_instances SET ready=false,lease_expires_at=$6,updated_at=now() WHERE environment=$1 AND service_id=$2 AND instance_id=$3 AND certificate_subject=$4 AND issuer_fingerprint_sha256=$5`, a.Environment, a.ServiceID, a.InstanceID, a.CertificateSubject, a.IssuerFingerprint, now); err != nil {
		return err
	}
	if _, err := bumpCatalogRevision(ctx, tx, a.Environment); err != nil {
		return err
	}
	if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: "platform_service_workload_revoked", ActorUserID: &a.RevokedBy, SubjectType: "platform_service_workload", SubjectID: a.ServiceID + "/" + a.InstanceID, Payload: map[string]any{"environment": a.Environment, "certificate_subject": a.CertificateSubject, "issuer_fingerprint_sha256": a.IssuerFingerprint}}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) SetPlatformServiceStatus(ctx context.Context, environment, serviceID, status, actor string) (int64, error) {
	if environment == "" || actor == "" || !serviceIDPattern.MatchString(serviceID) || (status != "active" && status != "suspended" && status != "retired") {
		return 0, ErrServiceRegistrationInvalid
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	revision, err := lockCatalogRevision(ctx, tx, environment)
	if err != nil {
		return 0, err
	}
	var previous string
	if err := tx.QueryRow(ctx, `SELECT status FROM platform_services WHERE environment=$1 AND service_id=$2 FOR UPDATE`, environment, serviceID).Scan(&previous); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	if previous == "retired" && status != "retired" {
		return 0, ErrConflict
	}
	if previous != status {
		if _, err := tx.Exec(ctx, `UPDATE platform_services SET status=$3,updated_at=now() WHERE environment=$1 AND service_id=$2`, environment, serviceID, status); err != nil {
			return 0, err
		}
		revision, err = bumpCatalogRevision(ctx, tx, environment)
		if err != nil {
			return 0, err
		}
		if err := createAuditEventTx(ctx, tx, AuditEventInput{EventType: "platform_service_status_changed", ActorUserID: &actor, SubjectType: "platform_service", SubjectID: serviceID, Payload: map[string]any{"environment": environment, "previous_status": previous, "status": status, "catalog_revision": revision}}); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return revision, nil
}

func manifestDigest(r PlatformServiceRegistration) (string, []byte, error) {
	options := slices.Clone(r.Options)
	slices.SortFunc(options, func(a, b PlatformServiceOption) int { return strings.Compare(a.Code, b.Code) })
	for i := range options {
		options[i].Requires = slices.Clone(options[i].Requires)
		slices.Sort(options[i].Requires)
	}
	content, err := json.Marshal(struct {
		ServiceID string                  `json:"service_id"`
		Version   string                  `json:"version"`
		Protocol  string                  `json:"protocol"`
		Options   []PlatformServiceOption `json:"options"`
		Endpoint  string                  `json:"endpoint_ref"`
	}{r.ServiceID, r.ManifestVersion, r.ProtocolVersion, options, r.EndpointRef})
	if err != nil {
		return "", nil, err
	}
	hash := sha256.Sum256(content)
	optionsJSON, err := json.Marshal(options)
	return hex.EncodeToString(hash[:]), optionsJSON, err
}

func (s *Store) RegisterPlatformServiceInstance(ctx context.Context, r PlatformServiceRegistration, p PlatformServicePrincipal, now time.Time) (PlatformServiceLease, error) {
	if err := ValidatePlatformServiceRegistration(r); err != nil {
		return PlatformServiceLease{}, err
	}
	digest, options, err := manifestDigest(r)
	if err != nil {
		return PlatformServiceLease{}, err
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return PlatformServiceLease{}, err
	}
	defer tx.Rollback(ctx)
	revision, err := lockCatalogRevision(ctx, tx, p.Environment)
	if err != nil {
		return PlatformServiceLease{}, err
	}
	if err := requireWorkload(ctx, tx, r.ServiceID, r.InstanceID, p, r.Options); err != nil {
		return PlatformServiceLease{}, err
	}
	requestHash := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%t", digest, r.InstanceID, r.Ready)))
	requestDigest := hex.EncodeToString(requestHash[:])
	if _, err := tx.Exec(ctx, `INSERT INTO platform_service_registration_requests(environment,certificate_subject,issuer_fingerprint_sha256,request_id,request_sha256)
		VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, p.Environment, p.CertificateSubject, p.IssuerFingerprint, r.RequestID, requestDigest); err != nil {
		return PlatformServiceLease{}, err
	}
	var previousRequestDigest string
	if err := tx.QueryRow(ctx, `SELECT request_sha256 FROM platform_service_registration_requests WHERE environment=$1 AND certificate_subject=$2 AND issuer_fingerprint_sha256=$3 AND request_id=$4`, p.Environment, p.CertificateSubject, p.IssuerFingerprint, r.RequestID).Scan(&previousRequestDigest); err != nil {
		return PlatformServiceLease{}, err
	}
	if previousRequestDigest != requestDigest {
		return PlatformServiceLease{}, ErrConflict
	}
	var existingDigest string
	err = tx.QueryRow(ctx, `SELECT digest_sha256 FROM platform_service_manifests WHERE environment=$1 AND service_id=$2 AND manifest_version=$3`, p.Environment, r.ServiceID, r.ManifestVersion).Scan(&existingDigest)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return PlatformServiceLease{}, err
	}
	if existingDigest != "" && existingDigest != digest {
		return PlatformServiceLease{}, ErrConflict
	}
	if existingDigest == "" {
		if err := validateManifestDependencies(ctx, tx, p.Environment, r); err != nil {
			return PlatformServiceLease{}, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO platform_service_manifests(environment,service_id,manifest_version,digest_sha256,protocol_version,endpoint_ref,options) VALUES($1,$2,$3,$4,$5,$6,$7)`, p.Environment, r.ServiceID, r.ManifestVersion, digest, r.ProtocolVersion, r.EndpointRef, options)
		if err != nil {
			return PlatformServiceLease{}, err
		}
	}
	initialStatus := "suspended"
	if r.ServiceID == "mqtt" {
		initialStatus = "active"
	}
	created, err := tx.Exec(ctx, `INSERT INTO platform_services(environment,service_id,published_version,status) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, p.Environment, r.ServiceID, r.ManifestVersion, initialStatus)
	if err != nil {
		return PlatformServiceLease{}, err
	}
	var prevVersion string
	var prevReady bool
	var prevLease time.Time
	err = tx.QueryRow(ctx, `SELECT manifest_version,ready,lease_expires_at FROM platform_service_instances WHERE environment=$1 AND service_id=$2 AND instance_id=$3`, p.Environment, r.ServiceID, r.InstanceID).Scan(&prevVersion, &prevReady, &prevLease)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return PlatformServiceLease{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO platform_service_instances(environment,service_id,instance_id,manifest_version,certificate_subject,issuer_fingerprint_sha256,ready,lease_expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT(environment,service_id,instance_id) DO UPDATE SET manifest_version=EXCLUDED.manifest_version,certificate_subject=EXCLUDED.certificate_subject,issuer_fingerprint_sha256=EXCLUDED.issuer_fingerprint_sha256,ready=EXCLUDED.ready,lease_expires_at=EXCLUDED.lease_expires_at,updated_at=now()`,
		p.Environment, r.ServiceID, r.InstanceID, r.ManifestVersion, p.CertificateSubject, p.IssuerFingerprint, r.Ready, now.Add(PlatformServiceLeaseDuration)); err != nil {
		return PlatformServiceLease{}, err
	}
	if created.RowsAffected() > 0 || prevVersion != r.ManifestVersion || prevReady != r.Ready || prevReady && !prevLease.After(now) {
		revision, err = bumpCatalogRevision(ctx, tx, p.Environment)
		if err != nil {
			return PlatformServiceLease{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return PlatformServiceLease{}, err
	}
	return PlatformServiceLease{r.ServiceID, r.InstanceID, r.ManifestVersion, revision, now.Add(PlatformServiceLeaseDuration), int(PlatformServiceHeartbeatInterval.Seconds())}, nil
}

func requireWorkload(ctx context.Context, tx pgx.Tx, serviceID, instanceID string, p PlatformServicePrincipal, options []PlatformServiceOption) error {
	var allowedCodes []string
	err := tx.QueryRow(ctx, `SELECT allowed_option_codes FROM platform_service_workloads WHERE environment=$1 AND certificate_subject=$2 AND issuer_fingerprint_sha256=$3 AND service_id=$4 AND instance_id=$5 AND revoked_at IS NULL`, p.Environment, p.CertificateSubject, p.IssuerFingerprint, serviceID, instanceID).Scan(&allowedCodes)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrServiceRegistrationDenied
	}
	if err != nil {
		return err
	}
	for _, option := range options {
		if !slices.Contains(allowedCodes, option.Code) {
			return ErrServiceRegistrationDenied
		}
	}
	return nil
}

func lockCatalogRevision(ctx context.Context, tx pgx.Tx, environment string) (int64, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO platform_service_catalog_revisions(environment,revision) VALUES($1,1) ON CONFLICT DO NOTHING`, environment); err != nil {
		return 0, err
	}
	var revision int64
	err := tx.QueryRow(ctx, `SELECT revision FROM platform_service_catalog_revisions WHERE environment=$1 FOR UPDATE`, environment).Scan(&revision)
	return revision, err
}

func bumpCatalogRevision(ctx context.Context, tx pgx.Tx, environment string) (int64, error) {
	var revision int64
	err := tx.QueryRow(ctx, `UPDATE platform_service_catalog_revisions SET revision=revision+1 WHERE environment=$1 RETURNING revision`, environment).Scan(&revision)
	return revision, err
}

func validateManifestDependencies(ctx context.Context, tx pgx.Tx, environment string, r PlatformServiceRegistration) error {
	available := make(map[string]string)
	allOptions := make(map[string]PlatformServiceOption)
	rows, err := tx.Query(ctx, `SELECT s.service_id,m.options FROM platform_services s JOIN platform_service_manifests m ON m.environment=s.environment AND m.service_id=s.service_id AND m.manifest_version=s.published_version WHERE s.environment=$1`, environment)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var service string
		var raw []byte
		if err := rows.Scan(&service, &raw); err != nil {
			return err
		}
		var options []PlatformServiceOption
		if err := json.Unmarshal(raw, &options); err != nil {
			return err
		}
		if service == r.ServiceID {
			continue
		}
		for _, option := range options {
			available[option.Code] = service
			allOptions[option.Code] = option
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, option := range r.Options {
		if owner := available[option.Code]; owner != "" && owner != r.ServiceID {
			return ErrConflict
		}
		allOptions[option.Code] = option
	}
	for _, option := range r.Options {
		for _, dependency := range option.Requires {
			if _, exists := allOptions[dependency]; !exists {
				return fmt.Errorf("%w: missing dependency %s", ErrServiceRegistrationInvalid, dependency)
			}
		}
		if option.Code != "mqtt" && !slices.Contains(option.Requires, "mqtt") {
			return fmt.Errorf("%w: %s requires mqtt", ErrServiceRegistrationInvalid, option.Code)
		}
	}
	states := make(map[string]uint8, len(allOptions))
	var visit func(string) error
	visit = func(code string) error {
		if states[code] == 1 {
			return fmt.Errorf("%w: cyclic option dependency", ErrServiceRegistrationInvalid)
		}
		if states[code] == 2 {
			return nil
		}
		states[code] = 1
		for _, dependency := range allOptions[code].Requires {
			if _, exists := allOptions[dependency]; !exists {
				return fmt.Errorf("%w: missing dependency %s", ErrServiceRegistrationInvalid, dependency)
			}
			if err := visit(dependency); err != nil {
				return err
			}
		}
		states[code] = 2
		return nil
	}
	for code := range allOptions {
		if err := visit(code); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) PublishPlatformServiceManifest(ctx context.Context, serviceID, expectedVersion, newVersion string, p PlatformServicePrincipal, now time.Time) (int64, error) {
	if !serviceIDPattern.MatchString(serviceID) || expectedVersion == "" || newVersion == "" || expectedVersion == newVersion {
		return 0, ErrServiceRegistrationInvalid
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := lockCatalogRevision(ctx, tx, p.Environment); err != nil {
		return 0, err
	}
	var authorized bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM platform_service_workloads WHERE environment=$1 AND certificate_subject=$2 AND issuer_fingerprint_sha256=$3 AND service_id=$4 AND revoked_at IS NULL)`, p.Environment, p.CertificateSubject, p.IssuerFingerprint, serviceID).Scan(&authorized); err != nil {
		return 0, err
	}
	if !authorized {
		return 0, ErrServiceRegistrationDenied
	}
	var currentVersion, status string
	if err := tx.QueryRow(ctx, `SELECT published_version,status FROM platform_services WHERE environment=$1 AND service_id=$2 FOR UPDATE`, p.Environment, serviceID).Scan(&currentVersion, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	if currentVersion != expectedVersion || status != "active" {
		return 0, ErrConflict
	}
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT options FROM platform_service_manifests WHERE environment=$1 AND service_id=$2 AND manifest_version=$3`, p.Environment, serviceID, newVersion).Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	var options []PlatformServiceOption
	if err := json.Unmarshal(raw, &options); err != nil {
		return 0, err
	}
	if err := validateManifestDependencies(ctx, tx, p.Environment, PlatformServiceRegistration{ServiceID: serviceID, Options: options}); err != nil {
		return 0, err
	}
	var ready bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM platform_service_instances WHERE environment=$1 AND service_id=$2 AND manifest_version=$3 AND ready AND lease_expires_at>$4)`, p.Environment, serviceID, newVersion, now).Scan(&ready); err != nil {
		return 0, err
	}
	if !ready {
		return 0, ErrConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE platform_services SET published_version=$3,updated_at=now() WHERE environment=$1 AND service_id=$2`, p.Environment, serviceID, newVersion); err != nil {
		return 0, err
	}
	revision, err := bumpCatalogRevision(ctx, tx, p.Environment)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return revision, nil
}

func (s *Store) HeartbeatPlatformServiceInstance(ctx context.Context, serviceID, instanceID, version string, ready bool, p PlatformServicePrincipal, now time.Time) (PlatformServiceLease, error) {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return PlatformServiceLease{}, err
	}
	defer tx.Rollback(ctx)
	revision, err := lockCatalogRevision(ctx, tx, p.Environment)
	if err != nil {
		return PlatformServiceLease{}, err
	}
	if err := requireWorkload(ctx, tx, serviceID, instanceID, p, nil); err != nil {
		return PlatformServiceLease{}, err
	}
	var previousReady bool
	var previousLease time.Time
	err = tx.QueryRow(ctx, `SELECT ready,lease_expires_at FROM platform_service_instances WHERE environment=$1 AND service_id=$2 AND instance_id=$3 AND manifest_version=$4 FOR UPDATE`, p.Environment, serviceID, instanceID, version).Scan(&previousReady, &previousLease)
	if errors.Is(err, pgx.ErrNoRows) {
		return PlatformServiceLease{}, ErrNotFound
	}
	if err != nil {
		return PlatformServiceLease{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE platform_service_instances SET ready=$5,lease_expires_at=$6,updated_at=now() WHERE environment=$1 AND service_id=$2 AND instance_id=$3 AND manifest_version=$4`, p.Environment, serviceID, instanceID, version, ready, now.Add(PlatformServiceLeaseDuration))
	if err != nil {
		return PlatformServiceLease{}, err
	}
	if previousReady != ready || previousReady && !previousLease.After(now) {
		revision, err = bumpCatalogRevision(ctx, tx, p.Environment)
		if err != nil {
			return PlatformServiceLease{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return PlatformServiceLease{}, err
	}
	return PlatformServiceLease{serviceID, instanceID, version, revision, now.Add(PlatformServiceLeaseDuration), int(PlatformServiceHeartbeatInterval.Seconds())}, nil
}

func (s *Store) DeregisterPlatformServiceInstance(ctx context.Context, serviceID, instanceID string, p PlatformServicePrincipal, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := lockCatalogRevision(ctx, tx, p.Environment); err != nil {
		return err
	}
	if err := requireWorkload(ctx, tx, serviceID, instanceID, p, nil); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `UPDATE platform_service_instances SET ready=false,lease_expires_at=$4,updated_at=now() WHERE environment=$1 AND service_id=$2 AND instance_id=$3`, p.Environment, serviceID, instanceID, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := bumpCatalogRevision(ctx, tx, p.Environment); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ListPlatformServiceOptions(ctx context.Context, environment string, now time.Time) (PlatformServiceCatalog, error) {
	return readPlatformServiceOptions(ctx, s.db, environment, now)
}

type platformCatalogReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func readPlatformServiceOptions(ctx context.Context, q platformCatalogReader, environment string, now time.Time) (PlatformServiceCatalog, error) {
	result := PlatformServiceCatalog{Options: []PlatformServiceCatalogOption{}}
	err := q.QueryRow(ctx, `SELECT revision FROM platform_service_catalog_revisions WHERE environment=$1`, environment).Scan(&result.CatalogRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		result.CatalogRevision = 1
	} else if err != nil {
		return result, err
	}
	rows, err := q.Query(ctx, `SELECT s.service_id,s.published_version,s.status,m.options,EXISTS(
		SELECT 1 FROM platform_service_instances i WHERE i.environment=s.environment AND i.service_id=s.service_id AND i.manifest_version=s.published_version AND i.ready AND i.lease_expires_at>$2)
		FROM platform_services s JOIN platform_service_manifests m ON m.environment=s.environment AND m.service_id=s.service_id AND m.manifest_version=s.published_version WHERE s.environment=$1 ORDER BY s.service_id`, environment, now)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var serviceID, version, status string
		var raw []byte
		var ready bool
		if err := rows.Scan(&serviceID, &version, &status, &raw, &ready); err != nil {
			return result, err
		}
		var options []PlatformServiceOption
		if err := json.Unmarshal(raw, &options); err != nil {
			return result, err
		}
		for _, option := range options {
			entry := PlatformServiceCatalogOption{PlatformServiceOption: option, ServiceID: serviceID, ManifestVersion: version, Selectable: status == "active" && ready}
			if status != "active" {
				entry.UnavailableReason = "service_" + status
			} else if !ready {
				entry.UnavailableReason = "service_unavailable"
			}
			result.Options = append(result.Options, entry)
		}
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	byCode := make(map[string]int, len(result.Options))
	for i := range result.Options {
		byCode[result.Options[i].Code] = i
	}
	visited := make(map[int]uint8, len(result.Options))
	var selectable func(int) bool
	selectable = func(i int) bool {
		if visited[i] == 1 {
			return false
		}
		if visited[i] == 2 {
			return result.Options[i].Selectable
		}
		visited[i] = 1
		for _, dependency := range result.Options[i].Requires {
			j, exists := byCode[dependency]
			if !exists || !selectable(j) {
				result.Options[i].Selectable = false
				result.Options[i].UnavailableReason = "dependency_unavailable"
				break
			}
		}
		visited[i] = 2
		return result.Options[i].Selectable
	}
	for i := range result.Options {
		selectable(i)
	}
	return result, nil
}
