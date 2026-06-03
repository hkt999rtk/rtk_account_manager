package inbox

import (
	"testing"

	"rtk_account_manager/internal/store"
)

func TestPostgresStoreSatisfiesInboxMessageStore(t *testing.T) {
	var _ messageStore = (*store.Store)(nil)
}
