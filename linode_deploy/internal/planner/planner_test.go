package planner

import (
	"strings"
	"testing"

	"rtk_account_manager/linode_deploy/internal/manifest"
)

func TestPlanIncludesAccountManagerDeploymentGates(t *testing.T) {
	m, err := manifest.Load("../../configs/account-manager-staging.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	plan := Text(m, Options{})
	for _, want := range []string{
		"account-manager-staging.realtekconnect.com",
		"account-manager VM account-manager-staging-account-manager 10.42.1.20",
		"edge -> account-manager 18081/tcp",
		"account-manager -> infra 5432/tcp",
		"verify release manifest and SHA256",
		"require database backup marker",
		"workers disabled by default",
	} {
		if !strings.Contains(plan, want) {
			t.Fatalf("plan missing %q:\n%s", want, plan)
		}
	}
}

func TestPlanShowsWorkersWhenEnabled(t *testing.T) {
	m, err := manifest.Load("../../configs/account-manager-staging.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	plan := Text(m, Options{EnableWorkers: true})
	if !strings.Contains(plan, "restart outbox/inbox workers") {
		t.Fatalf("plan missing worker restart:\n%s", plan)
	}
}
