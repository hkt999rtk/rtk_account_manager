package database

import (
	"context"
	"testing"
)

func TestMultiCloudViewerScopeConstraintsIntegration(t *testing.T) {
	db := newMultiCloudDatabase(t, false)
	ctx := context.Background()
	owner, viewer := multiCloudUser(t, db), multiCloudUser(t, db)
	cloud, foreign := multiCloudCreate(t, db, owner), multiCloudCreate(t, db, owner)
	p, other := admissionProduct(t, db, cloud), admissionProduct(t, db, foreign)
	for _, scope := range []any{nil, `null`, `{}`, `{"kind":"all_products","product_ids":[]}`, `{"kind":"selected_products","product_ids":[]}`, `{"kind":"selected_products","product_ids":["` + other + `"]}`, `{"kind":"selected_products","product_ids":["` + p + `","` + p + `"]}`} {
		_, err := db.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role,access_scope) VALUES($1,$2,'viewer',$3)`, cloud, viewer, scope)
		requirePGState(t, err, "23514")
	}
	if _, err := db.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role,access_scope) VALUES($1,$2,'viewer','{"kind":"all_products"}')`, cloud, viewer); err != nil {
		t.Fatal(err)
	}
	var allowed bool
	if err := db.QueryRow(ctx, `SELECT user_can_access_brand_cloud_product($1,$2,$3)`, viewer, cloud, p).Scan(&allowed); err != nil || !allowed {
		t.Fatalf("viewer scope inactive: %v %v", allowed, err)
	}
	for _, permission := range []string{"registry_device.manage", "billing.read", "app_certificate.issue", "video.playback"} {
		if err := db.QueryRow(ctx, `SELECT brand_cloud_permission_allowed($1,$2,$3)`, viewer, cloud, permission).Scan(&allowed); err != nil || allowed {
			t.Fatalf("viewer permission escaped ceiling: %s %v %v", permission, allowed, err)
		}
	}
	_, err := db.Exec(ctx, `UPDATE organization_members SET role='admin' WHERE organization_id=$1 AND user_id=$2`, cloud, viewer)
	requirePGState(t, err, "23514")
	if _, err := db.Exec(ctx, `UPDATE organization_members SET role='admin',access_scope=NULL WHERE organization_id=$1 AND user_id=$2`, cloud, viewer); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT user_can_access_brand_cloud_product($1,$2,$3)`, viewer, cloud, p).Scan(&allowed); err != nil || allowed {
		t.Fatalf("former viewer gained admin Product access: %v %v", allowed, err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
}
