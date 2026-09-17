package database

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
)

type migrationAliasRow bool

func (r migrationAliasRow) Scan(dest ...any) error {
	*(dest[0].(*bool)) = bool(r)
	return nil
}

type migrationAliasTx struct {
	pgx.Tx
	aliases []string
}

func (tx *migrationAliasTx) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	tx.aliases = append([]string(nil), args[0].([]string)...)
	return migrationAliasRow(false)
}

func TestRenamedTestLabMigrationUsesOnlyExactHistoricalAliases(t *testing.T) {
	tests := []struct {
		name    string
		aliases []string
	}{
		{name: "071_test_lab_sessions.sql", aliases: []string{"068_test_lab_sessions.sql", "070_test_lab_sessions.sql"}},
		{name: "072_test_lab_bindings.sql", aliases: []string{"069_test_lab_bindings.sql", "071_test_lab_bindings.sql"}},
		{name: "073_test_lab_console_identity.sql", aliases: []string{"070_test_lab_console_identity.sql", "072_test_lab_console_identity.sql"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tx := &migrationAliasTx{}
			adopted, err := adoptRenamedTestLabMigration(t.Context(), tx, test.name, nil)
			if err != nil || adopted {
				t.Fatalf("adopted=%t err=%v", adopted, err)
			}
			if !reflect.DeepEqual(tx.aliases, test.aliases) {
				t.Fatalf("aliases=%v want=%v", tx.aliases, test.aliases)
			}
		})
	}
	if adopted, err := adoptRenamedTestLabMigration(t.Context(), nil, "074_unrelated.sql", nil); err != nil || adopted {
		t.Fatalf("unrelated migration adopted=%t err=%v", adopted, err)
	}
}
