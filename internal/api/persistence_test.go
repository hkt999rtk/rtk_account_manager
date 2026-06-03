package api

import (
	"testing"

	"rtk_account_manager/internal/store"
)

func TestPostgresStoreSatisfiesAPIPersistenceBoundaries(t *testing.T) {
	var _ Store = (*store.Store)(nil)
}
