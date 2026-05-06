# Account Manager Deployment Assets

This directory contains reference deployment assets for private-cloud account
manager installs.

| Path | Purpose |
| --- | --- |
| `account-manager.env.example` | Operator-facing environment key inventory with placeholders only. |
| `systemd/` | Native Linux unit templates for API, migrations, workers, and cleanup. |

These files are examples, not a secret store. Copy the env file to an
operator-owned location such as `/etc/rtk-account-manager/account-manager.env`,
fill values from a secret manager, and set file mode `0600`.

The full operations runbook is in
[`docs/PRIVATE_CLOUD_DEPLOYMENT_RUNBOOK.md`](../docs/PRIVATE_CLOUD_DEPLOYMENT_RUNBOOK.md).
