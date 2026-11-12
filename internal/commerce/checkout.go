package commerce

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"enquoir/internal/domain"
)

// CheckoutID identifies a checkout Enquoir evaluated.
type CheckoutID string

const (
	maxCheckoutItems    = 100
	maxCheckoutQuantity = 10000
	// maxCheckoutAmount bounds every amount in a checkout (10 billion
	// major units), so no sum of them can overflow.
	maxCheckoutAmount = 1_000_000_000_000
	maxItemNameLength = 200
)

// CheckoutItem is one line of a checkout, as the merchant's server
// priced it.
type CheckoutItem struct {
	ProductID      ExternalID
	Name           string
	Category       string
	Quantity       int
	UnitPriceMinor int64
}

// CheckoutSnapshot is a purchase as the merchant's backend submitted it:
// who, under which mandate, what, and for how much, in integer minor
// units. It is frozen once evaluated; Hash binds every field.
type CheckoutSnapshot struct {
	OrgID           domain.OrgID
	MerchantID      MerchantID
	MandateID       MandateID
	ExternalID      ExternalID
	CustomerID      CustomerID
	AgentID         AgentID
	FundingMethodID FundingMethodID
	Currency        domain.Currency
	Items           []CheckoutItem
	ShippingMinor   int64
	TaxMinor        int64
	FeesMinor       int64
	TotalMinor      int64
}

func checkoutErr(field, format string, a ...any) error {
	return &FieldError{Field: field, Err: fmt.Errorf(format, a...)}
}

func amountOK(n int64) bool { return n >= 0 && n <= maxCheckoutAmount }

// NewCheckoutSnapshot validates and normalises a checkout: names
// trimmed, categories and currency case-folded, every amount 0 to 10
// billion major units, and the total exactly the items' sum plus
// shipping, tax and fees, and positive.
func NewCheckoutSnapshot(c CheckoutSnapshot) (CheckoutSnapshot, error) {
	ext, err := NewExternalID(string(c.ExternalID))
	if err != nil {
		return CheckoutSnapshot{}, &FieldError{Field: "external_id", Err: err}
	}
	c.ExternalID = ext
	c.Currency = domain.Currency(strings.ToUpper(strings.TrimSpace(string(c.Currency))))
	if !c.Currency.Supported() {
		return CheckoutSnapshot{}, checkoutErr("currency", "currency %q is not supported: use USD, EUR or GBP", c.Currency)
	}
	if len(c.Items) == 0 || len(c.Items) > maxCheckoutItems {
		return CheckoutSnapshot{}, checkoutErr("items", "a checkout has 1 to %d items", maxCheckoutItems)
	}
	items := make([]CheckoutItem, len(c.Items))
	var sum int64
	for i, it := range c.Items {
		f := func(name string) string { return fmt.Sprintf("items[%d].%s", i, name) }
		pid, err := NewExternalID(string(it.ProductID))
		if err != nil {
			return CheckoutSnapshot{}, &FieldError{Field: f("product_id"), Err: err}
		}
		it.ProductID = pid
		it.Name = strings.TrimSpace(it.Name)
		if it.Name == "" || utf8.RuneCountInString(it.Name) > maxItemNameLength {
			return CheckoutSnapshot{}, checkoutErr(f("name"), "name must be 1 to %d characters", maxItemNameLength)
		}
		if HoldsCardNumber(it.Name) {
			return CheckoutSnapshot{}, &FieldError{Field: f("name"), Err: fmt.Errorf("name %w", ErrLooksLikeCardNumber)}
		}
		it.Category = strings.ToLower(strings.TrimSpace(it.Category))
		if !categoryPattern.MatchString(it.Category) {
			return CheckoutSnapshot{}, checkoutErr(f("category"), "category must be 1 to 50 lower-case letters, digits, '_' or '-'")
		}
		if HoldsCardNumber(it.Category) {
			return CheckoutSnapshot{}, &FieldError{Field: f("category"), Err: fmt.Errorf("category %w", ErrLooksLikeCardNumber)}
		}
		if it.Quantity < 1 || it.Quantity > maxCheckoutQuantity {
			return CheckoutSnapshot{}, checkoutErr(f("quantity"), "quantity must be 1 to %d", maxCheckoutQuantity)
		}
		if !amountOK(it.UnitPriceMinor) {
			return CheckoutSnapshot{}, checkoutErr(f("unit_price_minor"), "unit_price_minor must be 0 to %d minor units", int64(maxCheckoutAmount))
		}
		sum += int64(it.Quantity) * it.UnitPriceMinor
		items[i] = it
	}
	c.Items = items
	for field, v := range map[string]int64{"shipping_minor": c.ShippingMinor, "tax_minor": c.TaxMinor, "fees_minor": c.FeesMinor} {
		if !amountOK(v) {
			return CheckoutSnapshot{}, checkoutErr(field, "%s must be 0 to %d minor units", field, int64(maxCheckoutAmount))
		}
	}
	want := sum + c.ShippingMinor + c.TaxMinor + c.FeesMinor
	if c.TotalMinor != want {
		return CheckoutSnapshot{}, checkoutErr("total_minor", "total_minor is %d, but the items, shipping, tax and fees add up to %d", c.TotalMinor, want)
	}
	if c.TotalMinor <= 0 {
		return CheckoutSnapshot{}, checkoutErr("total_minor", "total_minor must be positive")
	}
	return c, nil
}

const checkoutVersion = "checkout/v1"

type canonicalItem struct {
	ProductID      string `json:"product_id"`
	Name           string `json:"name"`
	Category       string `json:"category"`
	Quantity       int    `json:"quantity"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
}

type canonicalCheckout struct {
	Version         string          `json:"version"`
	OrgID           string          `json:"organisation_id"`
	MerchantID      string          `json:"merchant_id"`
	MandateID       string          `json:"mandate_id"`
	ExternalID      string          `json:"external_id"`
	CustomerID      string          `json:"customer_id"`
	AgentID         string          `json:"agent_id"`
	FundingMethodID string          `json:"funding_method_id"`
	Currency        string          `json:"currency"`
	Items           []canonicalItem `json:"items"`
	ShippingMinor   int64           `json:"shipping_minor"`
	TaxMinor        int64           `json:"tax_minor"`
	FeesMinor       int64           `json:"fees_minor"`
	TotalMinor      int64           `json:"total_minor"`
}

// Canonical is the snapshot's stored form, and what Hash covers. Items
// keep the merchant's order.
func (c CheckoutSnapshot) Canonical() []byte {
	items := make([]canonicalItem, len(c.Items))
	for i, it := range c.Items {
		items[i] = canonicalItem{string(it.ProductID), it.Name, it.Category, it.Quantity, it.UnitPriceMinor}
	}
	b, err := json.Marshal(canonicalCheckout{
		Version: checkoutVersion, OrgID: string(c.OrgID), MerchantID: string(c.MerchantID), MandateID: string(c.MandateID),
		ExternalID: string(c.ExternalID), CustomerID: string(c.CustomerID), AgentID: string(c.AgentID),
		FundingMethodID: string(c.FundingMethodID), Currency: string(c.Currency), Items: items,
		ShippingMinor: c.ShippingMinor, TaxMinor: c.TaxMinor, FeesMinor: c.FeesMinor, TotalMinor: c.TotalMinor,
	})
	if err != nil {
		panic("commerce: marshalling a checkout: " + err.Error())
	}
	return b
}

// Hash is "sha256:" and the hex SHA-256 of the canonical snapshot.
func (c CheckoutSnapshot) Hash() string {
	sum := sha256.Sum256(c.Canonical())
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ParseCheckoutSnapshot reads a stored snapshot back.
func ParseCheckoutSnapshot(b []byte) (CheckoutSnapshot, error) {
	var c canonicalCheckout
	if err := json.Unmarshal(b, &c); err != nil {
		return CheckoutSnapshot{}, fmt.Errorf("reading checkout: %w", err)
	}
	if c.Version != checkoutVersion {
		return CheckoutSnapshot{}, fmt.Errorf("unknown checkout version %q", c.Version)
	}
	items := make([]CheckoutItem, len(c.Items))
	for i, it := range c.Items {
		items[i] = CheckoutItem{ExternalID(it.ProductID), it.Name, it.Category, it.Quantity, it.UnitPriceMinor}
	}
	return CheckoutSnapshot{
		OrgID: domain.OrgID(c.OrgID), MerchantID: MerchantID(c.MerchantID), MandateID: MandateID(c.MandateID),
		ExternalID: ExternalID(c.ExternalID), CustomerID: CustomerID(c.CustomerID), AgentID: AgentID(c.AgentID),
		FundingMethodID: FundingMethodID(c.FundingMethodID), Currency: domain.Currency(c.Currency), Items: items,
		ShippingMinor: c.ShippingMinor, TaxMinor: c.TaxMinor, FeesMinor: c.FeesMinor, TotalMinor: c.TotalMinor,
	}, nil
}

// DenialCode is a stable reason a checkout falls outside its mandate.
type DenialCode string

const (
	DenialMandateNotActive     DenialCode = "mandate_not_active"
	DenialCustomerMismatch     DenialCode = "customer_mismatch"
	DenialAgentMismatch        DenialCode = "agent_mismatch"
	DenialFundingMismatch      DenialCode = "funding_method_mismatch"
	DenialCurrencyMismatch     DenialCode = "currency_mismatch"
	DenialOverTransactionLimit DenialCode = "over_transaction_limit"
	DenialCategoryNotAllowed   DenialCode = "category_not_allowed"
	DenialOverTransactionCount DenialCode = "over_transaction_count"
	DenialOverBudget           DenialCode = "over_budget"
)

// Denial is one reason, with a sentence for people.
type Denial struct {
	Code   DenialCode `json:"code"`
	Detail string     `json:"detail"`
}

// MandateUsage is what counts against a mandate: the number of
// purchases and their total, SpentMinor - paid, and held (authorized or
// redeemed, not yet settled). PaidMinor is the paid part.
type MandateUsage struct {
	Purchases  int
	SpentMinor int64
	PaidMinor  int64
}

// EvaluateCheckout returns every reason c falls outside m, which has
// status at the time of evaluation and usage already counted against
// it. No reasons: c is within the mandate.
func EvaluateCheckout(m Mandate, status MandateStatus, c CheckoutSnapshot, u MandateUsage) []Denial {
	var out []Denial
	add := func(code DenialCode, format string, a ...any) {
		out = append(out, Denial{Code: code, Detail: fmt.Sprintf(format, a...)})
	}
	t, p := m.Terms, m.Terms.Parties
	if status != MandateActive {
		add(DenialMandateNotActive, "the mandate is %s", strings.ReplaceAll(string(status), "_", " "))
	}
	if c.CustomerID != p.CustomerID {
		add(DenialCustomerMismatch, "the mandate is another customer's")
	}
	if c.AgentID != p.AgentID {
		add(DenialAgentMismatch, "the mandate authorises another agent")
	}
	if c.FundingMethodID != p.FundingMethodID {
		add(DenialFundingMismatch, "the mandate is for another payment method")
	}
	if c.Currency != t.Scope.Currency {
		add(DenialCurrencyMismatch, "the mandate is in %s, the checkout in %s", t.Scope.Currency, c.Currency)
	}
	if c.TotalMinor > t.Scope.MaxPerTransactionMinor {
		add(DenialOverTransactionLimit, "the total, %d, is over the mandate's %d per purchase", c.TotalMinor, t.Scope.MaxPerTransactionMinor)
	}
	if len(t.Scope.AllowedCategories) > 0 {
		allowed := map[string]bool{}
		for _, cat := range t.Scope.AllowedCategories {
			allowed[cat] = true
		}
		seen := map[string]bool{}
		var not []string
		for _, it := range c.Items {
			if !allowed[it.Category] && !seen[it.Category] {
				seen[it.Category] = true
				not = append(not, it.Category)
			}
		}
		if len(not) > 0 {
			sort.Strings(not)
			add(DenialCategoryNotAllowed, "the mandate does not allow %s", strings.Join(not, ", "))
		}
	}
	if t.Scope.MaxTransactions > 0 && u.Purchases >= t.Scope.MaxTransactions {
		add(DenialOverTransactionCount, "the mandate's %d purchases are used up", t.Scope.MaxTransactions)
	}
	if u.SpentMinor+c.TotalMinor > t.Scope.MaxTotalMinor {
		add(DenialOverBudget, "%d of the mandate's %d is left; the total is %d", t.Scope.MaxTotalMinor-u.SpentMinor, t.Scope.MaxTotalMinor, c.TotalMinor)
	}
	return out
}

// CheckoutStatus is where a checkout stands.
type CheckoutStatus string

const (
	CheckoutAuthorized       CheckoutStatus = "authorized"
	CheckoutAwaitingApproval CheckoutStatus = "awaiting_customer_approval"
	CheckoutDenied           CheckoutStatus = "denied"
	CheckoutRejected         CheckoutStatus = "rejected"
	CheckoutCanceled         CheckoutStatus = "canceled"
	CheckoutExpired          CheckoutStatus = "expired"
	// CheckoutRedeemed: its grant was redeemed; the merchant may charge.
	CheckoutRedeemed CheckoutStatus = "redeemed"
	// CheckoutRevoked: authorized, then its mandate was revoked first.
	CheckoutRevoked CheckoutStatus = "revoked"
	// CheckoutPaid: its execution succeeded. Its amount stays spent.
	CheckoutPaid CheckoutStatus = "paid"
	// CheckoutPaymentFailed: its execution failed. Its amount is released.
	CheckoutPaymentFailed CheckoutStatus = "payment_failed"
)

// CheckoutApprovalTTL is how long a customer has to approve a checkout
// under a per-transaction mandate.
const CheckoutApprovalTTL = time.Hour

// ErrCheckoutNotCancelable means the checkout is not authorized (with
// an unredeemed, unexpired grant) or awaiting approval: nothing to
// cancel. A redeemed checkout can't be cancelled.
var ErrCheckoutNotCancelable = errors.New("only an authorized checkout whose grant is unredeemed, or one awaiting the customer, can be cancelled")

// Checkout is a submitted checkout and Enquoir's decision on it.
type Checkout struct {
	ID                CheckoutID
	Snapshot          CheckoutSnapshot
	Status            CheckoutStatus // as stored
	Denials           []Denial
	CreatedAt         time.Time
	DecidedAt         *time.Time
	ApprovalExpiresAt *time.Time
	CanceledAt        *time.Time
	// Authorization is set once the checkout is authorized.
	Authorization *Authorization
}

// StatusAt is the status at now: awaiting approval past its expiry, or
// authorized with its grant unredeemed past its expiry, is expired.
func (c Checkout) StatusAt(now time.Time) CheckoutStatus {
	if c.Status == CheckoutAwaitingApproval && c.ApprovalExpiresAt != nil && !now.Before(*c.ApprovalExpiresAt) {
		return CheckoutExpired
	}
	if c.Status == CheckoutAuthorized && c.Authorization != nil && !now.Before(c.Authorization.ExpiresAt) {
		return CheckoutExpired
	}
	return c.Status
}

// CountsAt reports whether the checkout counts against its mandate at
// now: while authorized, until its grant expires; once redeemed, while
// its payment is pending or unknown; once paid, for good. A failed
// payment is released.
func (c Checkout) CountsAt(now time.Time) bool {
	switch c.StatusAt(now) {
	case CheckoutRedeemed, CheckoutAuthorized, CheckoutPaid:
		return true
	}
	return false
}
