package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

func validEligibility(now time.Time, request HandoffEligibilityRequest) HandoffEligibility {
	return HandoffEligibility{
		Request: request, ReceiptID: "receipt", EvidenceSHA256: strings.Repeat("a", 64),
		BalanceMinor: 0, Currency: "TWD", Complete: true,
		ObservedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute), Blockers: []string{},
	}
}

func TestConfigureOwnershipHandoffValidatesAndCopiesInventory(t *testing.T) {
	provider := handoffEligibilityFunc(func(context.Context, HandoffEligibilityRequest) (HandoffEligibility, error) {
		return HandoffEligibility{}, nil
	})
	for _, tc := range []OwnershipHandoffOptions{
		{},
		{Eligibility: provider},
		{Producers: []string{"video"}},
	} {
		if err := New(nil).ConfigureOwnershipHandoff(tc); !errors.Is(err, ErrHandoffUnavailable) {
			t.Fatalf("missing dependency accepted: %+v %v", tc, err)
		}
	}
	for _, producers := range [][]string{{"billing"}, {"Bad"}, {"video", "video"}, {"", "video"}} {
		if err := New(nil).ConfigureOwnershipHandoff(OwnershipHandoffOptions{Eligibility: provider, Producers: producers}); !errors.Is(err, ErrConflict) {
			t.Fatalf("invalid producers accepted: %q %v", producers, err)
		}
	}
	input := []string{"video", "factory"}
	s := New(nil)
	if err := s.ConfigureOwnershipHandoff(OwnershipHandoffOptions{Eligibility: provider, Producers: input}); err != nil {
		t.Fatal(err)
	}
	input[0] = "mutated"
	if !reflect.DeepEqual(s.ownershipHandoff.Producers, []string{"factory", "video"}) {
		t.Fatalf("inventory was not sorted and copied: %v", s.ownershipHandoff.Producers)
	}
}

func TestValidateHandoffEligibilityRequiresFreshNonnegativeUnblockedEvidence(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	request := HandoffEligibilityRequest{CloudID: "cloud", SourceUserID: "source", TargetUserID: "target", TransferID: "transfer", Action: "accept", OwnershipVersion: 3}
	for _, balance := range []int64{0, 1} {
		evidence := validEligibility(now, request)
		evidence.BalanceMinor = balance
		if err := validateHandoffEligibility(evidence, request, now); err != nil {
			t.Fatalf("balance %d rejected: %v", balance, err)
		}
	}
	negative := validEligibility(now, request)
	negative.BalanceMinor = -1
	if err := validateHandoffEligibility(negative, request, now); !errors.Is(err, ErrHandoffBalanceNegative) {
		t.Fatalf("negative balance: %v", err)
	}
	blocked := validEligibility(now, request)
	blocked.Blockers = []string{"payments_pending"}
	if err := validateHandoffEligibility(blocked, request, now); !errors.Is(err, ErrHandoffFinancialBlocked) {
		t.Fatalf("financial blocker: %v", err)
	}
	invalid := []func(*HandoffEligibility){
		func(e *HandoffEligibility) { e.Request.Action = "other" },
		func(e *HandoffEligibility) { e.ReceiptID = "" },
		func(e *HandoffEligibility) { e.EvidenceSHA256 = "ABC" },
		func(e *HandoffEligibility) { e.Currency = "USD" },
		func(e *HandoffEligibility) { e.Complete = false },
		func(e *HandoffEligibility) { e.ObservedAt = now.Add(time.Second) },
		func(e *HandoffEligibility) { e.ObservedAt = now.Add(-6 * time.Minute) },
		func(e *HandoffEligibility) { e.ExpiresAt = now },
		func(e *HandoffEligibility) { e.ExpiresAt = e.ObservedAt.Add(6 * time.Minute) },
	}
	for i, mutate := range invalid {
		evidence := validEligibility(now, request)
		mutate(&evidence)
		if err := validateHandoffEligibility(evidence, request, now); !errors.Is(err, ErrHandoffUnavailable) {
			t.Fatalf("invalid evidence %d: %v", i, err)
		}
	}
}

func TestCloudViewerScopeNormalizationAndComparison(t *testing.T) {
	all := &model.CloudViewerScope{Kind: "all_products"}
	if scope, err := normalizeCloudMemberScope(model.RoleViewer, all); err != nil || scope.Kind != "all_products" {
		t.Fatalf("all-products scope: %+v %v", scope, err)
	}
	selected := &model.CloudViewerScope{Kind: "selected_products", ProductIDs: []string{"BBBBBBBB-BBBB-BBBB-BBBB-BBBBBBBBBBBB", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}}
	scope, err := normalizeCloudMemberScope(model.RoleViewer, selected)
	if err != nil || !reflect.DeepEqual(scope.ProductIDs, []string{"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}) {
		t.Fatalf("selected scope: %+v %v", scope, err)
	}
	if !sameCloudScope(scope, &model.CloudViewerScope{Kind: "selected_products", ProductIDs: append([]string(nil), scope.ProductIDs...)}) || sameCloudScope(scope, all) {
		t.Fatal("scope comparison is not structural")
	}
	for _, tc := range []struct {
		role  model.Role
		scope *model.CloudViewerScope
	}{{model.RoleViewer, nil}, {model.RoleAdmin, all}, {model.RoleViewer, &model.CloudViewerScope{Kind: "selected_products", ProductIDs: []string{"bad"}}}} {
		if _, err := normalizeCloudMemberScope(tc.role, tc.scope); err == nil {
			t.Fatalf("invalid scope accepted: %+v", tc)
		}
	}
	if scope, err := normalizeCloudMemberScope(model.RoleMember, nil); err != nil || scope != nil {
		t.Fatalf("member scope: %+v %v", scope, err)
	}
}

func TestManagedCloudWriteValidation(t *testing.T) {
	for _, key := range []string{"a", strings.Repeat("x", 200), "safe/idempotency:key"} {
		if !ValidManagedCloudKey(key) {
			t.Fatalf("valid key rejected: %q", key)
		}
	}
	for _, key := range []string{"", strings.Repeat("x", 201), "has space", "line\nbreak", "非ascii"} {
		if ValidManagedCloudKey(key) {
			t.Fatalf("invalid key accepted: %q", key)
		}
	}
	name, description := "  Cloud name  ", ""
	got, err := (ManagedCloudWrite{Name: &name, Description: &description}).normalized(true)
	if err != nil || *got.Name != "Cloud name" || *got.Description != "" {
		t.Fatalf("normalized create: %+v %v", got, err)
	}
	got, err = (ManagedCloudWrite{Name: &name}).normalized(true)
	if err != nil || got.Description == nil || *got.Description != "" {
		t.Fatalf("default description: %+v %v", got, err)
	}
	for _, in := range []ManagedCloudWrite{
		{},
		{Name: stringPtr("   ")},
		{Name: stringPtr(strings.Repeat("x", 256))},
		{Name: stringPtr("bad\x00name")},
		{Name: stringPtr("valid"), Description: stringPtr(strings.Repeat("x", 2001))},
		{Name: stringPtr("valid"), Description: stringPtr("bad\x00description")},
	} {
		if _, err := in.normalized(true); !errors.Is(err, ErrInvalidManagedCloudWrite) {
			t.Fatalf("invalid write accepted: %+v %v", in, err)
		}
	}
	if _, err := (ManagedCloudWrite{Description: &description}).normalized(false); err != nil {
		t.Fatalf("description-only patch: %v", err)
	}
	if _, err := New(nil).UpdateManagedBrandCloud(context.Background(), "user", "", "key", ManagedCloudWrite{Name: &name}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty cloud id: %v", err)
	}
}
