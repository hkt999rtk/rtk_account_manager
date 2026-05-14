package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"rtk_account_manager/linode_deploy/internal/artifact"
	"rtk_account_manager/linode_deploy/internal/manifest"
	"rtk_account_manager/linode_deploy/internal/planner"
	"rtk_account_manager/linode_deploy/internal/render"
	"rtk_account_manager/linode_deploy/internal/secrets"
)

const help = `account-manager linode-deploy

Usage:
  linode-deploy plan --config linode_deploy/configs/account-manager-staging.yaml
  linode-deploy deploy --config linode_deploy/configs/account-manager-staging.yaml --stack account-manager-staging --release vX.Y.Z --secrets-file linode_deploy/secrets/account-manager-staging.env [--dry-run] [--skip-infra-apply] [--enable-workers] [--skip-oidc] [--report .artifacts/account-manager-linode-deploy.md]
`

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	_ = ctx
	if stdout == nil {
		stdout = io.Discard
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		_, _ = io.WriteString(stdout, help)
		return nil
	}
	switch args[0] {
	case "plan":
		return runPlan(args[1:], stdout)
	case "deploy":
		return runDeploy(args[1:], stdout)
	default:
		return fmt.Errorf("unknown command %s", args[0])
	}
}

func runPlan(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfg := fs.String("config", "", "")
	enableWorkers := fs.Bool("enable-workers", false, "")
	skipOIDC := fs.Bool("skip-oidc", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfg == "" {
		return fmt.Errorf("--config is required")
	}
	m, err := manifest.Load(*cfg)
	if err != nil {
		return err
	}
	_, _ = io.WriteString(out, planner.Text(m, planner.Options{EnableWorkers: *enableWorkers, SkipOIDC: *skipOIDC}))
	return nil
}

func runDeploy(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfg := fs.String("config", "", "")
	stack := fs.String("stack", "", "")
	release := fs.String("release", "", "")
	secretsFile := fs.String("secrets-file", "", "")
	dryRun := fs.Bool("dry-run", false, "")
	skipInfraApply := fs.Bool("skip-infra-apply", false, "")
	enableWorkers := fs.Bool("enable-workers", false, "")
	skipOIDC := fs.Bool("skip-oidc", false, "")
	report := fs.String("report", ".artifacts/account-manager-linode-deploy.md", "")
	releaseBundle := fs.String("release-bundle", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfg == "" || *stack == "" || *release == "" {
		return fmt.Errorf("--config, --stack, and --release are required")
	}
	if strings.EqualFold(*release, "latest") {
		return fmt.Errorf("explicit release is required; latest is not allowed")
	}
	m, err := manifest.Load(*cfg)
	if err != nil {
		return err
	}
	if m.Stack != *stack {
		return fmt.Errorf("manifest stack %q does not match --stack %q", m.Stack, *stack)
	}
	vals, err := secrets.Load(*secretsFile)
	if err != nil {
		return err
	}
	required := []string{"ACCOUNT_MANAGER_DB_PASSWORD", "JWT_ACCESS_SECRET", "JWT_REFRESH_SECRET"}
	if !*skipOIDC {
		required = append(required, "OIDC_CLIENT_SECRET", "OIDC_ISSUER_URL", "OIDC_CLIENT_ID")
	}
	if err := vals.Require(required...); err != nil {
		return err
	}
	env, redactedEnv := render.RuntimeEnv(m, vals, render.Options{SkipOIDC: *skipOIDC, EnableWorkers: *enableWorkers})
	_ = env
	_, _ = fmt.Fprintf(out, "dry-run deploy %s release %s\n", *stack, *release)
	if !*dryRun {
		_, _ = io.WriteString(out, "live execution is intentionally script-driven; run with --dry-run first and review generated commands\n")
	}
	if *skipInfraApply {
		_, _ = io.WriteString(out, "skip infrastructure apply\n")
	} else {
		_, _ = io.WriteString(out, "apply/create account-manager VM and edge route if missing\n")
	}
	if *releaseBundle != "" {
		if _, err := artifact.VerifyLocal(*releaseBundle, *release); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "verify local release bundle %s\n", *releaseBundle)
	} else {
		_, _ = fmt.Fprintf(out, "verify Object Storage release releases/%s/rtk_account_manager-%s.tar.gz\n", *release, *release)
	}
	_, _ = io.WriteString(out, "write runtime env to /etc/rtk-account-manager/account-manager.env mode 0600\n")
	_, _ = io.WriteString(out, "require database backup marker\n")
	_, _ = io.WriteString(out, "run rtk-account-manager-migrate.service\n")
	_, _ = io.WriteString(out, "restart rtk-account-manager.service rtk-account-manager-cleanup-tokens.timer\n")
	if *enableWorkers {
		_, _ = io.WriteString(out, "restart rtk-account-manager-outbox-worker.service rtk-account-manager-inbox-worker.service\n")
	}
	_, _ = io.WriteString(out, "run private and edge HTTPS health smoke checks\n")
	_, _ = fmt.Fprintf(out, "write deployment evidence report %s\n", *report)
	_, _ = io.WriteString(out, "redacted env inventory\n")
	_, _ = io.WriteString(out, redactedEnv)
	return nil
}
