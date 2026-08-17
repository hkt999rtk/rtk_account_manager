package api

import "testing"

func TestParseBillingVersionRequiresPositiveQuotedInteger(t *testing.T) {
	tests := []struct {
		value string
		want  int64
		ok    bool
	}{
		{value: `"3"`, want: 3, ok: true},
		{value: "7", want: 7, ok: true},
		{value: `W/"3"`, ok: false},
		{value: `"0"`, ok: false},
		{value: "", ok: false},
	}
	for _, test := range tests {
		got, ok := parseBillingVersion(test.value)
		if got != test.want || ok != test.ok {
			t.Errorf("parseBillingVersion(%q)=(%d,%v), want (%d,%v)", test.value, got, ok, test.want, test.ok)
		}
	}
}
