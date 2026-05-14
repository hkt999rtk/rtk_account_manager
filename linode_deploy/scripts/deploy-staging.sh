#!/usr/bin/env bash

set -euo pipefail

config="linode_deploy/configs/account-manager-staging.yaml"
stack="account-manager-staging"
secrets_file="linode_deploy/secrets/account-manager-staging.env"
report=".artifacts/account-manager-linode-deploy.md"
release=""
release_bundle=""
execute=false
skip_infra_apply=false
enable_workers=false
skip_oidc=false

usage() {
  cat <<'USAGE'
Usage:
  linode_deploy/scripts/deploy-staging.sh --release vX.Y.Z [options]

Options:
  --config PATH          Manifest path. Default: linode_deploy/configs/account-manager-staging.yaml
  --stack NAME           Stack name. Default: account-manager-staging
  --secrets-file PATH    Operator secrets file. Default: linode_deploy/secrets/account-manager-staging.env
  --release-bundle PATH  Local release bundle override for debug only.
  --report PATH          Redacted evidence report path.
  --skip-infra-apply     Skip infrastructure apply checks for deploy-only reruns.
  --enable-workers       Include outbox/inbox workers in runtime restart plan.
  --skip-oidc            Render OIDC disabled and mark OIDC smoke skipped.
  --execute              Leave dry-run mode. Review dry-run output before using.
USAGE
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --config) config="$2"; shift 2 ;;
    --stack) stack="$2"; shift 2 ;;
    --secrets-file) secrets_file="$2"; shift 2 ;;
    --release) release="$2"; shift 2 ;;
    --release-bundle) release_bundle="$2"; shift 2 ;;
    --report) report="$2"; shift 2 ;;
    --skip-infra-apply) skip_infra_apply=true; shift ;;
    --enable-workers) enable_workers=true; shift ;;
    --skip-oidc) skip_oidc=true; shift ;;
    --execute) execute=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [ -z "$release" ]; then
  echo "--release is required; latest is not allowed" >&2
  exit 2
fi
if [ "$release" = "latest" ]; then
  echo "--release latest is not allowed" >&2
  exit 2
fi
if [ ! -f "$config" ]; then
  echo "missing config: $config" >&2
  exit 1
fi
if [ ! -f "$secrets_file" ]; then
  echo "missing secrets file: $secrets_file" >&2
  exit 1
fi

args=(deploy --config "$config" --stack "$stack" --release "$release" --secrets-file "$secrets_file" --report "$report")
if [ -n "$release_bundle" ]; then
  args+=(--release-bundle "$release_bundle")
fi
if [ "$skip_infra_apply" = true ]; then
  args+=(--skip-infra-apply)
fi
if [ "$enable_workers" = true ]; then
  args+=(--enable-workers)
fi
if [ "$skip_oidc" = true ]; then
  args+=(--skip-oidc)
fi
if [ "$execute" = false ]; then
  args+=(--dry-run)
fi

mkdir -p "$(dirname "$report")"

{
  echo "# Account Manager Linode Deployment Evidence"
  echo
  echo "- stack: $stack"
  echo "- release: $release"
  echo "- config: $config"
  echo "- mode: $(if [ "$execute" = true ]; then echo execute; else echo dry-run; fi)"
  echo
  echo "## Plan"
  echo
  go run ./linode_deploy/cmd/linode-deploy plan --config "$config" \
    $(if [ "$enable_workers" = true ]; then echo --enable-workers; fi) \
    $(if [ "$skip_oidc" = true ]; then echo --skip-oidc; fi)
  echo
  echo "## Deployment Commands"
  echo
  go run ./linode_deploy/cmd/linode-deploy "${args[@]}"
} | tee "$report"

if [ "$execute" = false ]; then
  cat <<'MSG'

Dry-run only. Review the evidence report, confirm the Linode stack and backup
marker, then rerun with --execute when ready.
MSG
  exit 0
fi

cat <<'MSG'

Execution mode requested. This first account-manager Linode deploy artifact
validates and records the ordered deployment plan. Infrastructure mutation and
SSH installation should be executed by the operator following the generated
report until the Linode API/SSH executor is wired for account-manager live ops.
MSG
