# Developer PKI Test App Certificates

Account Manager owns target authorization, subject selection, certificate
metadata persistence, idempotency, and audit for the Developer Console app
certificate test path. The canonical bundle shape is defined by
[`docs/rtk_cloud_contracts_doc/certificate_bundle.md`](rtk_cloud_contracts_doc/certificate_bundle.md).

> `POST /v1/developer/brand-clouds/{brandCloudId}/pki/test-app-certificates`
> is a local/staging SDK smoke-test utility. It is not the production app
> certificate bootstrap, rotation, revocation, or hardware-backed key flow.

## Request policy

- `DEVELOPER_PKI_TEST_TOOLS_ENABLED=true` is required.
- `ACCOUNT_MANAGER_ENV` must be local, development, or staging. Production
  rejects the route regardless of the feature flag.
- Authentication must resolve to an owner/admin member of the addressed Brand
  Cloud with the effective `pki.test.issue` capability.
- `Idempotency-Key` is mandatory and reuse with different target/CSR input is a
  conflict.
- `target_type` is `user` or `end_user`; a global `user` target must be a member
  of the addressed Brand Cloud, and an `end_user` uses its scoped lookup.
  Unavailable end-user lookup fails explicitly; retired `brand_cloud_user`
  targets are rejected.
- Account Manager derives the exact subject (`app-user:<global_user_id>` or
  `app-end-user:<id>`). Callers cannot choose an arbitrary subject or SAN.
- The CSR must prove possession of its key and match the derived subject.
- Certificate lifetime is fixed at 30 days.

The response preserves the existing app-certificate PEM and metadata fields and
adds `certificate_bundle`. The bundle has `profile=certificate_only`,
`usage=app_mtls`, and `key.material.type=caller_managed`; Account Manager never
receives or returns the private key.

## Audit and secret handling

Successful issuance records `pki.test.app_certificate_issued` with the issuer
request id, idempotency key, target, CSR SHA-256, and fixed TTL. Audit and logs
must not contain CSR PEM, certificate PEM, private keys, bearer/refresh tokens,
factory JWTs, or user credentials.

The browser may locally combine the certificate-only response with its
exportable P-256 PKCS#8 key to build `test_exportable`. Such files are secrets
and are valid only for simple local/staging SDK tests. Production app keys remain
non-exportable and use the formal Account Manager bootstrap and lifecycle.
