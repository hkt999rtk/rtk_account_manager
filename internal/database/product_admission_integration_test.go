package database

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func admissionProduct(t *testing.T, db *pgxpool.Pool, cloud string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(context.Background(), `INSERT INTO device_item_profiles
	    (brand_cloud_id,profile_key,display_name,category,ca_profile,issuer_profile)
	    VALUES ($1,gen_random_uuid()::text,'migration product','ip_camera','ca','issuer') RETURNING id::text`, cloud).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestMultiCloudProductAdmissionMigrationIntegration(t *testing.T) {
	db := newMultiCloudDatabaseBefore(t, "053_brand_cloud_product_admission.sql")
	ctx := context.Background()
	owner, explicit, broad, revoked := multiCloudUser(t, db), multiCloudUser(t, db), multiCloudUser(t, db), multiCloudUser(t, db)
	cloud, other := multiCloudCreate(t, db, owner), multiCloudCreate(t, db, owner)
	product, foreign := admissionProduct(t, db, cloud), admissionProduct(t, db, other)
	for _, user := range []string{explicit, broad, revoked} {
		if _, err := db.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES ($1,$2,'member')`, cloud, user); err != nil {
			t.Fatal(err)
		}
	}
	for _, user := range []string{explicit, revoked} {
		if _, err := db.Exec(ctx, `INSERT INTO role_assignments(role_id,actor_type,actor_id,scope_type,scope_id,organization_id,disabled_at)
		    SELECT id,'user',$1,'product',$2,$3,CASE WHEN $4 THEN now() END FROM roles WHERE name='product_viewer'`, user, product, cloud, user == revoked); err != nil {
			t.Fatal(err)
		}
	}
	// Organization membership already has its broad organization ACL. It must
	// not create Product admission, nor may a mismatched cloud/Product ACL do so.
	if _, err := db.Exec(ctx, `INSERT INTO role_assignments(role_id,actor_type,actor_id,scope_type,scope_id,organization_id)
	    SELECT id,'user',$1,'product',$2,$3 FROM roles WHERE name='product_viewer'`, broad, foreign, cloud); err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.Abs("../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIGRATIONS_DIR", dir)
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM brand_cloud_product_admissions`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("backfill count=%d want=1: %v", count, err)
	}
	var provenance string
	if err := db.QueryRow(ctx, `SELECT provenance FROM brand_cloud_product_admissions WHERE organization_id=$1 AND user_id=$2 AND product_id=$3 AND approved_by IS NULL`, cloud, explicit, product).Scan(&provenance); err != nil || provenance != "legacy_explicit_product_assignment" {
		t.Fatalf("migration invented approver or wrong source: %q %v", provenance, err)
	}
	_, err = db.Exec(ctx, `INSERT INTO brand_cloud_product_admissions(organization_id,user_id,product_id,provenance,approved_by) VALUES ($1,$2,$3,'owner_invitation',$4)`, cloud, broad, foreign, owner)
	requirePGState(t, err, "23503")
	for _, c := range []struct {
		user, product string
		want          bool
	}{
		{explicit, product, true}, {explicit, foreign, false}, {broad, product, false}, {revoked, product, false}, {owner, product, true}, {owner, foreign, false},
	} {
		var allowed bool
		if err := db.QueryRow(ctx, `SELECT user_can_access_brand_cloud_product($1,$2,$3)`, c.user, cloud, c.product).Scan(&allowed); err != nil || allowed != c.want {
			t.Fatalf("admission=%v want=%v: %v", allowed, c.want, err)
		}
	}
	if _, err := db.Exec(ctx, `DELETE FROM organization_members WHERE organization_id=$1 AND user_id=$2`, cloud, explicit); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES ($1,$2,'member')`, cloud, explicit); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM brand_cloud_product_admissions`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("migration replay revived revoked admission: %d %v", count, err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM role_assignments WHERE actor_id=$1 AND organization_id=$2 AND scope_type='product' AND disabled_at IS NULL`, explicit, cloud).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejoin revived old Product ACL: %d %v", count, err)
	}
}
