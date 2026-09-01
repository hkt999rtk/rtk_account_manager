package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"rtk_account_manager/internal/billinghandoff"
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
		if err := New(nil).ConfigureOwnershipHandoff(tc); tc.Eligibility == nil && !errors.Is(err, ErrHandoffUnavailable) {
			t.Fatalf("missing dependency accepted: %+v %v", tc, err)
		} else if tc.Eligibility != nil && !errors.Is(err, ErrConflict) {
			t.Fatalf("reduced inventory accepted: %+v %v", tc, err)
		}
	}
	for _, producers := range [][]string{{"billing"}, {"Bad"}, {"factory", "mqtt_usage"}, {"factory", "mqtt_usage", "mqtt_usage"}, {"factory", "mqtt_usage", "video_control_plane", "extra"}} {
		if err := New(nil).ConfigureOwnershipHandoff(OwnershipHandoffOptions{Eligibility: provider, Producers: producers}); !errors.Is(err, ErrConflict) {
			t.Fatalf("invalid producers accepted: %q %v", producers, err)
		}
	}
	input := []string{"video_control_plane", "factory", "mqtt_usage"}
	s := New(nil)
	if err := s.ConfigureOwnershipHandoff(OwnershipHandoffOptions{Eligibility: provider, Producers: input}); err != nil {
		t.Fatal(err)
	}
	input[0] = "mutated"
	if !reflect.DeepEqual(s.ownershipHandoff.Producers, RequiredHandoffProducers()) {
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

func TestHandoffStatusProjectionAndBillingBlockers(t *testing.T) {
	snapshot := &model.CloudBalanceSnapshot{BalanceMinor: 0, Currency: "TWD"}
	for _, tc := range []struct {
		name, status, operation, phase, state string
		snapshot                              *model.CloudBalanceSnapshot
		confirmed                             bool
	}{
		{"pending", "pending", "", "awaiting_acceptance", "running", nil, false},
		{"preparing", "accepted", "preparing", "preparing", "running", nil, false},
		{"committing", "accepted", "committing", "committing", "running", snapshot, false},
		{"finalizing", "accepted", "finalizing", "finalizing", "running", snapshot, true},
		{"succeeded", "accepted", "succeeded", "succeeded", "succeeded", snapshot, true},
		{"canceling", "accepted", "canceling", "blocked", "blocked", nil, false},
		{"canceled", "accepted", "canceled", "canceled", "canceled", nil, false},
		{"missing operation", "accepted", "", "blocked", "blocked", nil, false},
		{"stale confirmation", "accepted", "preparing", "blocked", "blocked", snapshot, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := projectHandoffStatus(model.BrandCloudOwnerTransfer{
				Status: tc.status, OperationPhase: tc.operation, RequestedByUserID: "source",
				BalanceSnapshot: tc.snapshot, Operation: &model.CloudOperation{},
			})
			if view.SourceUserID != "source" || view.Phase != tc.phase || view.Operation.State != tc.state {
				t.Fatalf("projection: %+v", view)
			}
			if tc.operation == "canceling" && view.Operation.Phase != "canceling" {
				t.Fatalf("cancel phase hidden: %+v", view.Operation)
			}
			if tc.confirmed && (view.SourceConfirmed == nil || !*view.SourceConfirmed || !*view.TargetConfirmed) {
				t.Fatalf("committed consent missing: %+v", view)
			}
		})
	}
	blockers := billingBlockers([]string{"balance_negative", "usage_unsettled", "outstanding_debt", "unpaid_invoices", "payments_pending", "payment_setups_pending", "refunds_pending", "disputes_open", "settlement_evidence_stale", "unknown"})
	want := []string{"balance_negative", "usage_unsettled", "debt_outstanding", "payment_pending", "refund_pending", "dispute_pending", "confirmation_stale", "evidence_unavailable"}
	if len(blockers) != len(want) {
		t.Fatalf("blockers=%+v", blockers)
	}
	for i, code := range want {
		if blockers[i].Code != code || blockers[i].Retryable == (code == "balance_negative") {
			t.Fatalf("blocker %d: %+v", i, blockers[i])
		}
	}
	if got := billingBlockers(nil); len(got) != 1 || got[0].Code != "evidence_unavailable" {
		t.Fatalf("empty blockers: %+v", got)
	}
}

func TestHandoffAndDeletionAdapterConfiguration(t *testing.T) {
	s := New(nil)
	if err := s.ConfigureHandoffBilling(nil); !errors.Is(err, ErrHandoffUnavailable) {
		t.Fatalf("nil billing: %v", err)
	}
	billing := &balanceTestBilling{}
	if err := s.ConfigureHandoffBilling(billing); err != nil || s.handoffBilling != billing {
		t.Fatalf("billing configuration: %v", err)
	}
	participant := workerFailureParticipant{}
	for _, adapters := range []map[string]HandoffParticipant{
		{"billing": participant}, {"Bad": participant}, {"video": nil},
	} {
		if err := s.ConfigureHandoffParticipants(adapters); !errors.Is(err, ErrConflict) {
			t.Fatalf("invalid participants accepted: %+v %v", adapters, err)
		}
	}
	adapters := map[string]HandoffParticipant{
		HandoffParticipantFactory:           participant,
		HandoffParticipantMQTTUsage:         participant,
		HandoffParticipantVideoControlPlane: participant,
	}
	if err := s.ConfigureHandoffParticipants(adapters); err != nil {
		t.Fatal(err)
	}
	delete(adapters, HandoffParticipantFactory)
	if _, ok := s.handoffParticipants[HandoffParticipantFactory]; !ok {
		t.Fatal("participant inventory was not copied")
	}
	for _, tc := range []struct {
		err     error
		outcome string
	}{
		{ErrHandoffBalanceNegative, "balance_negative"},
		{ErrHandoffFinancialBlocked, "financial_blocked"},
		{ErrDeveloperCloudLimitExceeded, "quota_exceeded"},
		{ErrConflict, "lifecycle_conflict"},
		{ErrNotFound, "lifecycle_conflict"},
		{errors.New("offline"), "evidence_unavailable"},
	} {
		if got := handoffStepError(tc.err); got.Outcome != tc.outcome || !got.Retry {
			t.Fatalf("step error %v: %+v", tc.err, got)
		}
	}
	if handoffDigest(map[string]int{"version": 1}) != handoffDigest(map[string]int{"version": 1}) {
		t.Fatal("handoff digest is not deterministic")
	}

	deletion := New(nil)
	billingFn := deletionBillingFunc(func(context.Context, billinghandoff.CloudDeletionScope) (billinghandoff.CloudDeletionPreflight, error) {
		return billinghandoff.CloudDeletionPreflight{}, nil
	})
	resourceFn := deletionResourceFunc(func(context.Context, CloudDeletionResourceScope) (CloudDeletionResourceEvidence, error) {
		return CloudDeletionResourceEvidence{}, nil
	})
	if err := deletion.ConfigureCloudDeletionPreflight(CloudDeletionPreflightOptions{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing deletion billing: %v", err)
	}
	for name, observer := range map[string]CloudDeletionResourceObserver{"billing": resourceFn, "Bad": resourceFn, "video": nil} {
		if err := deletion.ConfigureCloudDeletionPreflight(CloudDeletionPreflightOptions{Billing: billingFn, Resources: map[string]CloudDeletionResourceObserver{name: observer}}); !errors.Is(err, ErrConflict) {
			t.Fatalf("invalid deletion observer %q: %v", name, err)
		}
	}
	resources := map[string]CloudDeletionResourceObserver{"video": resourceFn}
	if err := deletion.ConfigureCloudDeletionPreflight(CloudDeletionPreflightOptions{Billing: billingFn, Resources: resources}); err != nil {
		t.Fatal(err)
	}
	delete(resources, "video")
	if _, ok := deletion.deletionPreflight.Resources["video"]; !ok {
		t.Fatal("deletion observer inventory was not copied")
	}
	if err := deletion.ConfigureCloudDeletionPreflight(CloudDeletionPreflightOptions{Billing: billingFn}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate deletion configuration: %v", err)
	}
}
