package database

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"rtk_account_manager/internal/store"
)

func TestAuditConsolidationPreservesHistoryAndDomains(t *testing.T) {
	db, retained := newIdentityCaseDatabase(t)
	ctx := context.Background()
	// Install the exact predecessor, with populated audit rows in both domains.
	before := t.TempDir()
	entries, err := os.ReadDir(retained)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() >= "083_" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(retained, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(before, e.Name()), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MIGRATIONS_DIR", before)
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	identityCaseExec(t, db, `INSERT INTO email_outbox(idempotency_key,message_type,payload_nonce,payload_ciphertext)
 VALUES ('retired-activation','brand_cloud_user_activation','fixture','fixture'),
 ('global-activation','login_activation','fixture','fixture')`)
	providerQuery := `SELECT * FROM identity_providers WHERE provider_id='fixture-provider'`
	tagQuery := `SELECT * FROM device_tag_catalog WHERE organization_id='00000000-0000-0000-0000-000000000001' AND tag='fixture'`
	t.Logf("before provider index: %s", indexedPlan(t, db, providerQuery))
	t.Logf("before tag index: %s", indexedPlan(t, db, tagQuery))
	actor := identityCaseUser(t, db, "audit@example.com", "fixture", true)
	general := identityCaseID(t, db, `INSERT INTO audit_events(event_type,actor_user_id,subject_type,subject_id,payload,created_at,updated_at)
 VALUES ('fixture',$1::uuid,'user',$1::text,'{"source":"general"}','2026-01-01','2026-01-02') RETURNING id::text`, actor)
	acl := identityCaseID(t, db, `INSERT INTO acl_audit_events(event_type,actor_user_id,subject_type,subject_id,payload,created_at,updated_at)
 VALUES ('fixture',$1::uuid,'user',$1::text,'{"source":"acl"}','2026-01-03','2026-01-04') RETURNING id::text`, actor)
	identityCaseExec(t, db, `CREATE TABLE audit_expected AS SELECT id,event_type,actor_user_id,organization_id,subject_type,subject_id,payload,created_at,updated_at,'general'::text AS audit_domain FROM audit_events
 UNION ALL SELECT id,event_type,actor_user_id,organization_id,subject_type,subject_id,payload,created_at,updated_at,'acl' FROM acl_audit_events`)
	complete, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIGRATIONS_DIR", complete)
	preflight, err := CheckSimplification(ctx, db)
	if err != nil || !preflight.Ready || preflight.Rows["acl_audit_events"] != 1 {
		t.Fatalf("cleanup preflight=%+v %v", preflight, err)
	}
	var rolledBack bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('acl_audit_events') IS NOT NULL AND NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='audit_events' AND column_name='audit_domain')`).Scan(&rolledBack); err != nil || !rolledBack {
		t.Fatalf("preflight changed database: %t %v", rolledBack, err)
	}
	t.Setenv("MIGRATIONS_DIR", retained)
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var mismatch int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM ((TABLE audit_expected EXCEPT TABLE audit_events) UNION ALL (TABLE audit_events EXCEPT TABLE audit_expected)) delta`).Scan(&mismatch); err != nil || mismatch != 0 {
		t.Fatalf("audit reconciliation: count=%d err=%v", mismatch, err)
	}
	s := store.New(db)
	g, err := s.ListAuditEvents(ctx, store.AuditEventListFilter{EventType: "fixture", Limit: 1})
	if err != nil || g.Page.Total != 1 || len(g.Events) != 1 || g.Events[0].ID != general {
		t.Fatalf("general scope=%+v err=%v", g, err)
	}
	a, err := s.ListACLAuditEvents(ctx, store.ACLAuditEventListFilter{EventType: "fixture", Limit: 1})
	if err != nil || a.Page.Total != 1 || len(a.Events) != 1 || a.Events[0].ID != acl {
		t.Fatalf("ACL scope=%+v err=%v", a, err)
	}
	identityCaseExec(t, db, `DELETE FROM users WHERE id=$1`, actor)
	var actors int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE actor_user_id=$1`, actor).Scan(&actors); err != nil || actors != 2 {
		t.Fatalf("lost historical actor: %d %v", actors, err)
	}
	if err := s.CreateACLAuditEvent(ctx, store.ACLAuditEventInput{EventType: "fixture", ActorUserID: &actor, SubjectType: "user", SubjectID: actor}); err == nil {
		t.Fatal("accepted deleted ACL actor")
	}
	if err := s.CreateACLAuditEvent(ctx, store.ACLAuditEventInput{EventType: " ", SubjectType: "user", SubjectID: actor}); err == nil {
		t.Fatal("accepted blank ACL event")
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIGRATIONS_DIR", complete)
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var expired, globalPending bool
	if err := db.QueryRow(ctx, `SELECT status='expired' AND payload_nonce IS NULL AND payload_ciphertext IS NULL AND lease_until IS NULL FROM email_outbox WHERE idempotency_key='retired-activation'`).Scan(&expired); err != nil || !expired {
		t.Fatalf("retired activation remains deliverable: %t %v", expired, err)
	}
	if err := db.QueryRow(ctx, `SELECT status='pending' AND payload_ciphertext IS NOT NULL FROM email_outbox WHERE idempotency_key='global-activation'`).Scan(&globalPending); err != nil || !globalPending {
		t.Fatalf("global activation was changed: %t %v", globalPending, err)
	}
	if err := VerifySimplification(ctx, db); err != nil {
		t.Fatal(err)
	}
	if plan := indexedPlan(t, db, providerQuery); !strings.Contains(plan, "identity_providers_provider_id_key") {
		t.Fatalf("retained provider index not usable: %s", plan)
	} else {
		t.Logf("after provider index: %s", plan)
	}
	if plan := indexedPlan(t, db, tagQuery); !strings.Contains(plan, "device_tag_catalog_pkey") {
		t.Fatalf("retained tag index not usable: %s", plan)
	} else {
		t.Logf("after tag index: %s", plan)
	}
	providerInsert := `INSERT INTO identity_providers(provider_id,name,type,issuer_url,client_id) VALUES('fixture-provider','fixture','oidc','https://id.example.test','client')`
	identityCaseExec(t, db, providerInsert)
	if _, err := db.Exec(ctx, providerInsert); err == nil {
		t.Fatal("provider uniqueness lost after index retirement")
	}
	owner := identityCaseUser(t, db, "index-owner@example.com", "fixture", true)
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var org string
	if err := tx.QueryRow(ctx, `INSERT INTO organizations(name) VALUES('index fixture') RETURNING id::text`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,'owner')`, org, owner); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	identityCaseExec(t, db, `INSERT INTO device_tag_catalog(organization_id,tag) VALUES($1,'fixture')`, org)
	if _, err := db.Exec(ctx, `INSERT INTO device_tag_catalog(organization_id,tag) VALUES($1,'fixture')`, org); err == nil {
		t.Fatal("tag uniqueness lost after index retirement")
	}
	identityCaseExec(t, db, `DROP TABLE audit_expected`)
	fresh := newEmptyIdentityCaseDatabase(t)
	if err := Migrate(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if got, want := businessCatalog(t, db), businessCatalog(t, fresh); got != want {
		t.Fatalf("upgraded catalog differs from fresh catalog\nupgraded: %s\nfresh: %s", got, want)
	}

}

func TestAuditCollisionRollsBackConsolidation(t *testing.T) {
	db, full := newIdentityCaseDatabase(t)
	ctx := context.Background()
	id := identityCaseID(t, db, `INSERT INTO audit_events(event_type,subject_type,subject_id) VALUES ('general','device','fixture') RETURNING id::text`)
	identityCaseExec(t, db, `INSERT INTO acl_audit_events(id,event_type,subject_type,subject_id) VALUES ($1,'acl','device','fixture')`, id)
	t.Setenv("MIGRATIONS_DIR", full)
	if err := Migrate(ctx, db); err == nil || !strings.Contains(err.Error(), "identity collision") {
		t.Fatalf("expected collision: %v", err)
	}
	var intact bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('acl_audit_events') IS NOT NULL AND NOT EXISTS(SELECT 1 FROM schema_migrations WHERE version LIKE '083_%') AND NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='audit_events' AND column_name='audit_domain')`).Scan(&intact); err != nil || !intact {
		t.Fatalf("failed migration changed schema: %t %v", intact, err)
	}
	identityCaseExec(t, db, `DELETE FROM acl_audit_events WHERE id=$1`, id)
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
}

func TestTenantRetirementRefusesUnmappedIdentity(t *testing.T) {
	db, full := newIdentityCaseDatabase(t)
	ctx := context.Background()
	owner := identityCaseUser(t, db, "owner@example.com", identityCaseHash(t, "fixture"), true)
	brand := identityCaseBrand(t, db, "retirement", owner)
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	legacy := identityCaseLegacy(t, db, brand, "late@example.com", identityCaseHash(t, "fixture"), "admin", true)
	complete, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIGRATIONS_DIR", complete)
	if err := Migrate(ctx, db); err == nil || !strings.Contains(err.Error(), "unmapped legacy identity") {
		t.Fatalf("expected unmapped blocker: %v", err)
	}
	var retained bool
	if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM brand_cloud_users WHERE id=$1) AND NOT EXISTS(SELECT 1 FROM schema_migrations WHERE version LIKE '084_%')`, legacy).Scan(&retained); err != nil || !retained {
		t.Fatalf("source lost: %t %v", retained, err)
	}
	// Explicit fixture reconciliation through the published identity correction.
	replayIdentityCorrection(t, db, full)
	// 051 corrects existing mappings only; a late identity must remain a blocker.
	if err := Migrate(ctx, db); err == nil {
		t.Fatal("late identity was silently removed")
	}
}

// Compare executed PostgreSQL catalogs, including all tables, column types,
// nullability/defaults, primary/unique keys and declared foreign keys.
func businessCatalog(t *testing.T, db *pgxpool.Pool) string {
	t.Helper()
	var catalog string
	err := db.QueryRow(context.Background(), `SELECT jsonb_agg(entry ORDER BY entry->>'table')::text FROM (
 SELECT jsonb_build_object('table',c.relname,
 'columns',(SELECT jsonb_agg(jsonb_build_array(a.attname,format_type(a.atttypid,a.atttypmod),a.attnotnull,pg_get_expr(d.adbin,d.adrelid)) ORDER BY a.attname)
 FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum WHERE a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped),
 'keys',(SELECT jsonb_agg(pg_get_constraintdef(k.oid) ORDER BY pg_get_constraintdef(k.oid)) FROM pg_constraint k WHERE k.conrelid=c.oid AND k.contype IN ('p','u','f'))
 ) entry FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind='r'
 ) catalogs`).Scan(&catalog)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func indexedPlan(t *testing.T, db *pgxpool.Pool, query string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// Force index eligibility only; this is not a latency/performance claim on a tiny fixture.
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan=off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, "EXPLAIN "+query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}
