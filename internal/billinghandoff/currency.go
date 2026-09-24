package billinghandoff

// CurrentCurrency is the sole currency Billing accepts for a live handoff.
// A future base-currency change requires a coordinated contract and settlement
// migration; accepting another code here alone would be unsafe.
const CurrentCurrency = "TWD"
