package api

import (
	"context"
	"testing"
)

func readyDevicePKIFixture(t *testing.T, env integrationEnv, product string) {
	t.Helper()
	if _, err := env.db.Exec(context.Background(), `UPDATE device_item_profiles SET pki_status='ready',pki_issuer_id=gen_random_uuid() WHERE id=$1`, product); err != nil {
		t.Fatal(err)
	}
}
