# Billing Integration Boundary

Account Manager owns organization identity, membership, roles, and billing
permission grants. It does not own pricing, wallet balances, payment methods,
payment intents, invoices, or billing access state.

Cloud Admin authenticates a customer through Account Manager and resolves the
active organization and capabilities. It then calls the independent
`rtk_billing` service with a dedicated service credential, stable actor ID, and
the one permission required by that operation.

The billing permission migrations in this repository intentionally remain here
because RBAC is an Account Manager responsibility. Monetary migrations and
runtime packages belong only to `rtk_billing`.

Canonical behavior is defined by `billing_service.md`,
`payments_and_balance.md`, `pricing_and_invoicing.md`, and
`billing_activity.md` in the contracts repository.
