package outbox

import (
	"testing"

	"rtk_account_manager/internal/store"
)

func TestPostgresStoreSatisfiesOutboxMessageStore(t *testing.T) {
	var _ messageStore = (*store.Store)(nil)
}
