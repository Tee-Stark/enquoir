package commerce

import (
	"time"

	"enquoir/internal/domain"
)

// Merchant is a merchant integration: one merchant's (one storefront's)
// connection to Enquoir, owned by an organisation. Its credential is an
// integration key; while it is disabled, that key is refused.
type Merchant struct {
	ID         MerchantID
	OrgID      domain.OrgID
	Name       string
	Status     Status
	CreatedAt  time.Time
	DisabledAt *time.Time
}

// Customer is one of a merchant's customers: the person whose money a
// purchase spends, and the only one who can grant an agent authority to
// spend it. Email is where Enquoir sends consent links, so that consent
// never rests on the merchant's word.
type Customer struct {
	ID         CustomerID
	MerchantID MerchantID
	OrgID      domain.OrgID
	ExternalID ExternalID
	Email      domain.Email
	Status     Status
	CreatedAt  time.Time
	DisabledAt *time.Time
}

// Agent is an AI shopping agent the merchant runs. It has no credential
// of its own: the merchant names it in the requests it makes.
type Agent struct {
	ID         AgentID
	MerchantID MerchantID
	OrgID      domain.OrgID
	ExternalID ExternalID
	Name       string
	Status     Status
	CreatedAt  time.Time
	DisabledAt *time.Time
}
