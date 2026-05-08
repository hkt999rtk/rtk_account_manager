# Account Manager Deployment Assets

This directory contains reference deployment assets for private-cloud account
manager installs.

| Path | Purpose |
| --- | --- |
| `account-manager.env.example` | Operator-facing environment key inventory with placeholders only. |
| `systemd/` | Native Linux unit templates for API, migrations, workers, and cleanup. |
| `install.sh` | Root-owned native install script used by local CD. |
| `verify.sh` | Installed staging health/systemd verification script. |
| `check-release.sh` | Release bundle guard used by `make release` and CD. |

These files are examples, not a secret store. Copy the env file to an
operator-owned location such as `/etc/rtk-account-manager/account-manager.env`,
fill values from a secret manager, and set file mode `0600`.

The full operations runbook is in
[`docs/PRIVATE_CLOUD_DEPLOYMENT_RUNBOOK.md`](../docs/PRIVATE_CLOUD_DEPLOYMENT_RUNBOOK.md).

Staging CD deploys immutable release tarballs to `video-cloud-cd.local` using
the `account-manager-cd` self-hosted runner label. Runtime secrets must stay in
`/etc/rtk-account-manager/account-manager.env` or the GitHub staging
environment secret `ACCOUNT_MANAGER_RUNTIME_ENV`.
