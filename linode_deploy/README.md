# Account Manager Linode Deploy

Operator-local deployment assets for placing `rtk_account_manager` on Linode.

GitHub CI remains artifact-only. Live Linode mutation, DNS updates, host
installation, and secret handling are run by a trusted operator machine.

See [`docs/RUNBOOK.md`](docs/RUNBOOK.md).

## Deployment Modes

| Mode | Entry Point | Purpose |
| --- | --- | --- |
| Manifest plan | `scripts/deploy-staging.sh` | Existing dry-run/planning evidence flow. |
| Public VM live deploy | `scripts/provision-public-vm.sh`, `scripts/deploy-public-vm.sh` | Dedicated public-only Account Manager staging VM. |

## Public VM Target Shape

```text
internet
  -> account-manager.video-cloud-staging.realtekconnect.com:443
  -> nginx on Account Manager VM
  -> rtk_account_manager on 127.0.0.1:18081
  -> local PostgreSQL on 127.0.0.1:5432
```

The Account Manager VM is independent from the `rtk_video_cloud` VPC. It does
not join the Video Cloud private network and does not use the Video Cloud edge
VM as a gateway. Other services should call it through public HTTPS.

## Public VM Files

| File | Purpose |
| --- | --- |
| `templates/account-manager-public-staging.env.example` | Local operator env template. Copy before editing. |
| `scripts/provision-public-vm.sh` | Creates the public-only Linode VM and firewall through the Linode API. |
| `scripts/set-godaddy-dns.sh` | Updates the staging DNS A record through the GoDaddy API. |
| `scripts/deploy-public-vm.sh` | Builds a Linux release, installs PostgreSQL/nginx/certbot/systemd, applies migrations, and starts the service. |
| `scripts/verify-public-vm.sh` | Runs external HTTPS health/register/login smoke checks. |
| `scripts/backup-public-postgres.sh` | Pulls a sanitized PostgreSQL custom-format dump artifact. |

## Public VM Quick Start

```sh
cp linode_deploy/templates/account-manager-public-staging.env.example \
  linode_deploy/secrets/account-manager-public-staging.env
$EDITOR linode_deploy/secrets/account-manager-public-staging.env

set -a
. ~/.env                         # LINODE_TOKEN, GODADDY_KEY, GODADDY_SECRET
. linode_deploy/secrets/account-manager-public-staging.env
set +a

linode_deploy/scripts/provision-public-vm.sh
. linode_deploy/state/rtk-account-manager-staging.env
linode_deploy/scripts/set-godaddy-dns.sh
linode_deploy/scripts/deploy-public-vm.sh
linode_deploy/scripts/verify-public-vm.sh
```

Ignored state and artifacts stay under `linode_deploy/state/` and `.artifacts/`.
Do not commit copied env files, state files, database dumps, or verification
artifacts.

## Release Object Storage

Formal release bundles are published by `.github/workflows/release.yml` to
Linode Object Storage. The workflow uses the AWS CLI only as an S3-compatible
client for Linode Object Storage.

Object prefix:

```text
releases/rtk_account_manager-<version>/
```

Required objects:

```text
<version>.tar.gz
<version>.tar.gz.sha256
manifest.json
```

Self-check:

```sh
aws s3 ls "s3://$LINODE_OBJ_BUCKET/releases/rtk_account_manager-$VERSION/" \
  --endpoint-url "$LINODE_OBJ_ENDPOINT"

mkdir -p ".artifacts/release-download/$VERSION"
aws s3 cp "s3://$LINODE_OBJ_BUCKET/releases/rtk_account_manager-$VERSION/$VERSION.tar.gz" \
  ".artifacts/release-download/$VERSION/$VERSION.tar.gz" \
  --endpoint-url "$LINODE_OBJ_ENDPOINT"
aws s3 cp "s3://$LINODE_OBJ_BUCKET/releases/rtk_account_manager-$VERSION/$VERSION.tar.gz.sha256" \
  ".artifacts/release-download/$VERSION/$VERSION.tar.gz.sha256" \
  --endpoint-url "$LINODE_OBJ_ENDPOINT"
aws s3 cp "s3://$LINODE_OBJ_BUCKET/releases/rtk_account_manager-$VERSION/manifest.json" \
  ".artifacts/release-download/$VERSION/manifest.json" \
  --endpoint-url "$LINODE_OBJ_ENDPOINT"

scripts/verify-linode-release-objects.sh "$VERSION" ".artifacts/release-download/$VERSION"
```

## Security Notes

- Remote hosts never push to GitHub.
- The VM exposes only SSH from operator CIDRs and public HTTP/HTTPS.
- PostgreSQL binds locally on the VM and is not exposed publicly.
- Staging defaults use `AUTH_TOKEN_DELIVERY=log`, `CROSS_SERVICE_BROKER=log`,
  and `OIDC_ENABLED=false`; production-like deployments must explicitly supply
  SMTP/OIDC/broker settings and record those choices in service-owned docs.
