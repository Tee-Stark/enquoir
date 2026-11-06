package postgres

// PreTenancyOrgID is the organisation that intents recorded before
// organisations existed belong to (migration 0005). Nobody signs in to
// it; it keeps that history readable and verifiable instead of lost.
const PreTenancyOrgID = "org_pre_tenancy"
