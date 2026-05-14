package planner

import (
	"fmt"
	"strings"

	"rtk_account_manager/linode_deploy/internal/manifest"
)

type Options struct {
	EnableWorkers bool
	SkipOIDC      bool
}

func Text(m manifest.Manifest, opts Options) string {
	acct := m.AccountManager()
	edge := m.Edge()
	infra := m.Infra()
	lines := []string{
		fmt.Sprintf("stack %s region %s", m.Stack, m.Region),
		fmt.Sprintf("public route %s -> edge %s -> account-manager %s:%d", m.Deploy.Domain, edge.PrivateIP, acct.PrivateIP, m.Deploy.APIPort),
		fmt.Sprintf("account-manager VM %s %s", acct.Label, acct.PrivateIP),
		"edge -> account-manager 18081/tcp",
		"account-manager -> infra 5432/tcp",
		fmt.Sprintf("database %s role %s on infra %s", m.Deploy.DatabaseName, m.Deploy.DatabaseRole, infra.PrivateIP),
		"verify release manifest and SHA256",
		"write /etc/rtk-account-manager/account-manager.env mode 0600",
		"require database backup marker",
		"run rtk-account-manager-migrate.service before runtime restart",
		"restart rtk-account-manager.service and rtk-account-manager-cleanup-tokens.timer",
	}
	if opts.EnableWorkers {
		lines = append(lines, "restart outbox/inbox workers")
	} else {
		lines = append(lines, "workers disabled by default")
	}
	if opts.SkipOIDC {
		lines = append(lines, "OIDC smoke skipped by operator flag")
	} else {
		lines = append(lines, "OIDC redirect "+m.Deploy.PublicBaseURL+"/v1/auth/oidc/keycloak/callback")
	}
	lines = append(lines, "write redacted deployment evidence report")
	return strings.Join(lines, "\n") + "\n"
}
