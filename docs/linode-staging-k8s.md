# Linode Staging K8s Runtime

Linode staging runtime is K8s-only. The previous Account Manager Linode VM
toolkit has been retired and must not be used for staging provision, deploy,
verify, backup, or VM log collection.

Operate staging from the workspace root:

```sh
./stg.sh provision --confirm video-cloud-staging
scripts/run-staging-e2e.sh --confirm video-cloud-staging
```

Account Manager staging endpoints and bootstrap credentials are provided through
the workspace K8s service discovery and runtime secret flow.

## Account Manager Target Profile

The LKE target preserves Account Manager API behavior while replacing the
retired public VM, nginx, local env file, and systemd service with Kubernetes
primitives:

- Run the API as a Deployment in an `account-manager` namespace with a
  ClusterIP Service and readiness/liveness probes.
- Expose public HTTPS through Linode NodeBalancer plus Ingress or Gateway API.
  cert-manager owns TLS automation.
- Keep `/v1/health` as the external smoke endpoint and keep
  `/metrics/prometheus` private to the observability namespace.
- Source runtime secrets from OpenBao or the approved workspace secret manager.
  Kubernetes Secrets may hold injected runtime material only; do not commit env
  files, DSNs, tokens, or signing material.
- Keep the database outside Account Manager manifests until the staging database
  target is explicitly selected. Compare temporary VM/external database
  retention, a PostgreSQL operator, a StatefulSet, or a managed/external
  PostgreSQL option before cutover.
- Run database migration and rollback through release-controlled jobs with
  restore-tested backups before production traffic moves to LKE.

Before writing production manifests, confirm namespace naming, database target,
migration job shape, cert-manager issuer, OpenBao auth role, NetworkPolicy, and
backup target in the workspace LKE migration inventory.
