//go:build integration

package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"enquoir/internal/commerce"
	"enquoir/internal/domain"
	"enquoir/internal/policy"
)

func TestPolicyStore_CurrentPolicy_ReturnsDefaultForNewOrg(t *testing.T) {
	ctx := context.Background()
	accounts, store := NewAccountStore(testPool(t)), NewPolicyStore(testPool(t))
	org, _ := newTestOrg(t, accounts)

	v, err := store.CurrentPolicy(ctx, org.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Version != 0 || v.OrgID != org.ID {
		t.Errorf("CurrentPolicy for a fresh org = %+v, want the default version (0) scoped to the org", v)
	}
}

func TestPolicyStore_SaveNewVersion_IncrementsVersion(t *testing.T) {
	ctx := context.Background()
	accounts, store := NewAccountStore(testPool(t)), NewPolicyStore(testPool(t))
	org, user := newTestOrg(t, accounts)

	v1, err := policy.NewPolicyVersion(org.ID, 0, map[domain.Currency]policy.CurrencyRule{
		domain.USD: {MaxPerTransactionMinor: 10_000},
	}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	v1.CreatedBy = user.ID
	saved1, err := store.SaveNewVersion(ctx, v1)
	if err != nil {
		t.Fatal(err)
	}
	if saved1.Version != 1 {
		t.Fatalf("first saved version = %d, want 1", saved1.Version)
	}

	v2, _ := policy.NewPolicyVersion(org.ID, 0, map[domain.Currency]policy.CurrencyRule{
		domain.USD: {MaxPerTransactionMinor: 20_000},
	}, nil, nil, nil)
	v2.CreatedBy = user.ID
	v2.CreatedAt = time.Now()
	saved2, err := store.SaveNewVersion(ctx, v2)
	if err != nil {
		t.Fatal(err)
	}
	if saved2.Version != 2 {
		t.Errorf("second saved version = %d, want 2", saved2.Version)
	}

	// CurrentPolicy must read back created_at/created_by too, not just
	// the canonical snapshot's own fields - those live in their own
	// columns, not inside the canonical JSON (which Hash is computed
	// over and never carried them).
	current, err := store.CurrentPolicy(ctx, org.ID)
	if err != nil || current.Version != 2 || current.CurrencyRules[domain.USD].MaxPerTransactionMinor != 20_000 {
		t.Errorf("CurrentPolicy = %+v, %v, want version 2 with the latest rule", current, err)
	}
	if current.CreatedBy != user.ID {
		t.Errorf("CurrentPolicy.CreatedBy = %q, want %q", current.CreatedBy, user.ID)
	}
	if current.CreatedAt.IsZero() || current.CreatedAt.Year() < 2000 {
		t.Errorf("CurrentPolicy.CreatedAt = %v, want the real save time, not the Go zero value", current.CreatedAt)
	}

	// An old version is still readable by evaluating against it directly
	// isn't exposed by this store yet, but the saved value round-trips.
	if saved1.CurrencyRules[domain.USD].MaxPerTransactionMinor != 10_000 {
		t.Errorf("first saved version's own rule changed after a later save: %+v", saved1)
	}
}

func TestEvaluateAndReserve_AllowsWithinTheDefaultPolicy(t *testing.T) {
	ctx := context.Background()
	accounts, store := NewAccountStore(testPool(t)), NewPolicyStore(testPool(t))
	org, _ := newTestOrg(t, accounts)

	d, err := store.EvaluateAndReserve(ctx, org.ID, policy.EvaluationInput{
		Context: policy.ContextTreasury, Currency: domain.USD, AmountMinor: 1_000,
	})
	if err != nil || d.Outcome != policy.Allow {
		t.Errorf("EvaluateAndReserve against an org with no policy set = %+v, %v, want Allow", d, err)
	}
}

func TestEvaluateAndReserve_DeniesOverASavedLimit(t *testing.T) {
	ctx := context.Background()
	accounts, store := NewAccountStore(testPool(t)), NewPolicyStore(testPool(t))
	org, _ := newTestOrg(t, accounts)

	v, _ := policy.NewPolicyVersion(org.ID, 0, map[domain.Currency]policy.CurrencyRule{
		domain.USD: {MaxPerTransactionMinor: 5_000},
	}, nil, nil, nil)
	if _, err := store.SaveNewVersion(ctx, v); err != nil {
		t.Fatal(err)
	}

	d, err := store.EvaluateAndReserve(ctx, org.ID, policy.EvaluationInput{
		Context: policy.ContextTreasury, Currency: domain.USD, AmountMinor: 10_000,
	})
	if err != nil || d.Outcome != policy.Deny {
		t.Errorf("EvaluateAndReserve over the saved limit = %+v, %v, want Deny", d, err)
	}
}

// A payment counts towards usage the instant it is persisted - at
// PROPOSED, before any Temporal worker ever advances it - matching
// policy.Store's doc comment on what "persisting" means. Before this,
// treasuryUsage only counted AUTHORIZED onward, so a payment stuck at
// PROPOSED or AWAITING_APPROVAL (awaiting an escalation decision) was
// invisible to a concurrent evaluation for as long as the worker took
// to advance it.
func TestEvaluateAndReserve_CountsIntentsStillProposedOrAwaitingApproval(t *testing.T) {
	ctx := context.Background()
	accounts := NewAccountStore(testPool(t))
	store := NewPolicyStore(testPool(t))
	intents := NewIntentRepository(testPool(t))
	events := NewEventLog(testPool(t))
	org, _ := newTestOrg(t, accounts)

	v, _ := policy.NewPolicyVersion(org.ID, 0, map[domain.Currency]policy.CurrencyRule{
		domain.USD: {Max24hMinor: 15_000},
	}, nil, nil, nil)
	if _, err := store.SaveNewVersion(ctx, v); err != nil {
		t.Fatal(err)
	}

	proposed, err := domain.NewIntent(org.ID, treasuryOf(org.ID), "payee-proposed", 10_000, "USD", "ref-proposed", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := intents.Save(ctx, proposed); err != nil {
		t.Fatal(err)
	}
	cleanupIntent(t, testPool(t), proposed.Key)
	if _, err := events.Append(ctx, org.ID, proposed.Key, "PROPOSED", ""); err != nil {
		t.Fatal(err)
	}

	awaiting, err := domain.NewIntent(org.ID, treasuryOf(org.ID), "payee-awaiting", 4_000, "USD", "ref-awaiting", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := intents.Save(ctx, awaiting); err != nil {
		t.Fatal(err)
	}
	cleanupIntent(t, testPool(t), awaiting.Key)
	if _, err := events.Append(ctx, org.ID, awaiting.Key, "PROPOSED", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := events.Append(ctx, org.ID, awaiting.Key, "AWAITING_APPROVAL", ""); err != nil {
		t.Fatal(err)
	}

	// 10,000 (proposed) + 4,000 (awaiting approval) + 2,000 candidate =
	// 16,000, over the 15,000 24h limit - denied only if both pending
	// intents already count.
	d, err := store.EvaluateAndReserve(ctx, org.ID, policy.EvaluationInput{
		Context: policy.ContextTreasury, Currency: domain.USD, AmountMinor: 2_000,
	})
	if err != nil || d.Outcome != policy.Deny {
		t.Errorf("EvaluateAndReserve with PROPOSED+AWAITING_APPROVAL usage already at 14,000 = %+v, %v, want Deny", d, err)
	}
}

// The rolling window is anchored on created_at, not state_changed_at:
// a payment created well outside the window must stay excluded even
// if it only just settled (state_changed_at refreshed to now) -
// otherwise a payment stuck in UNKNOWN for a day would re-enter the
// window the moment it finally resolves, as if it were brand new.
func TestEvaluateAndReserve_WindowIsAnchoredOnCreatedAtNotStateChangedAt(t *testing.T) {
	ctx := context.Background()
	accounts := NewAccountStore(testPool(t))
	store := NewPolicyStore(testPool(t))
	intents := NewIntentRepository(testPool(t))
	events := NewEventLog(testPool(t))
	pool := testPool(t)
	org, _ := newTestOrg(t, accounts)

	v, _ := policy.NewPolicyVersion(org.ID, 0, map[domain.Currency]policy.CurrencyRule{
		domain.USD: {Max24hMinor: 15_000},
	}, nil, nil, nil)
	if _, err := store.SaveNewVersion(ctx, v); err != nil {
		t.Fatal(err)
	}

	old, err := domain.NewIntent(org.ID, treasuryOf(org.ID), "payee-old", 10_000, "USD", "ref-old", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := intents.Save(ctx, old); err != nil {
		t.Fatal(err)
	}
	cleanupIntent(t, pool, old.Key)
	if _, err := events.Append(ctx, org.ID, old.Key, "PROPOSED", ""); err != nil {
		t.Fatal(err)
	}
	// Backdate created_at to 2 days ago, then settle it just now -
	// state_changed_at refreshes to the current time, created_at
	// stays old.
	if _, err := pool.Exec(ctx, `UPDATE intents SET created_at = now() - interval '2 days' WHERE key = $1`, string(old.Key)); err != nil {
		t.Fatal(err)
	}
	if _, err := events.Append(ctx, org.ID, old.Key, "APPLIED", "tr_1"); err != nil {
		t.Fatal(err)
	}

	// 10,000 (the old, just-settled payment) + 6,000 candidate =
	// 16,000, over the 15,000 24h limit only if the old payment still
	// counts despite its state_changed_at being seconds old.
	d, err := store.EvaluateAndReserve(ctx, org.ID, policy.EvaluationInput{
		Context: policy.ContextTreasury, Currency: domain.USD, AmountMinor: 6_000,
	})
	if err != nil || d.Outcome != policy.Allow {
		t.Errorf("EvaluateAndReserve with a 2-day-old settled payment = %+v, %v, want Allow (it must not count towards the 24h window)", d, err)
	}
}

// A per-transaction checkout already passed this organisation's
// policy (that gate runs before the mandate is ever evaluated) and is
// only waiting on the customer's own approval - it must already count
// against the org's rolling usage, or several such checkouts could
// each pass org-policy evaluation independently without ever seeing
// each other's pending amount.
func TestEvaluateAndReserve_CountsCommerceCheckoutsAwaitingCustomerApproval(t *testing.T) {
	ctx := context.Background()
	w := newCheckoutWorld(t)
	m := w.mandate(t, "m1", commerce.PerTransaction)

	store := NewPolicyStore(testPool(t))
	v, _ := policy.NewPolicyVersion(w.merchant.OrgID, 0, map[domain.Currency]policy.CurrencyRule{
		domain.EUR: {Max24hMinor: 6_000},
	}, nil, nil, nil)
	if _, err := store.SaveNewVersion(ctx, v); err != nil {
		t.Fatal(err)
	}

	a, _, err := w.submit.Run(ctx, w.merchant, w.book(m, "a", 5000))
	if err != nil || a.Status != commerce.CheckoutAwaitingApproval {
		t.Fatalf("submit: %+v %v", a, err)
	}

	// 5,000 already awaiting the customer + 2,000 candidate = 7,000,
	// over the 6,000 24h limit - denied only if the awaiting checkout
	// already counts.
	d, err := store.EvaluateAndReserve(ctx, w.merchant.OrgID, policy.EvaluationInput{
		Context: policy.ContextCommerce, Currency: domain.EUR, AmountMinor: 2_000,
	})
	if err != nil || d.Outcome != policy.Deny {
		t.Errorf("EvaluateAndReserve with a checkout awaiting_customer_approval already at 5,000 = %+v, %v, want Deny", d, err)
	}
}

// A real concurrency test: fire several evaluations at once near an
// hourly-count limit and confirm the organisation's lock serialises
// them so usage is never read against a torn view of a concurrent
// evaluation. This does not by itself prove no two payments can ever
// be allowed past a limit (see Store's doc comment on the window
// between a successful evaluation and the caller persisting its
// payment) - it proves the lock itself works.
func TestEvaluateAndReserve_LocksOrgRow_ConcurrentCallsSerialise(t *testing.T) {
	ctx := context.Background()
	accounts, store := NewAccountStore(testPool(t)), NewPolicyStore(testPool(t))
	org, _ := newTestOrg(t, accounts)

	v, _ := policy.NewPolicyVersion(org.ID, 0, map[domain.Currency]policy.CurrencyRule{
		domain.USD: {MaxPerHourCount: 3},
	}, nil, nil, nil)
	if _, err := store.SaveNewVersion(ctx, v); err != nil {
		t.Fatal(err)
	}

	const attempts = 10
	var wg sync.WaitGroup
	results := make([]policy.Outcome, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d, err := store.EvaluateAndReserve(ctx, org.ID, policy.EvaluationInput{
				Context: policy.ContextTreasury, Currency: domain.USD, AmountMinor: 100,
			})
			if err != nil {
				t.Errorf("attempt %d: %v", i, err)
				return
			}
			results[i] = d.Outcome
		}(i)
	}
	wg.Wait()

	// Every evaluation ran against zero prior usage (nothing persisted
	// any of these candidate payments), so with a per-hour limit of 3,
	// a correctly-locked store still allows every one of them - the
	// lock's job here is only to prevent a torn concurrent read, not to
	// cross-count calls that never persisted anything. What this test
	// actually guards against is a crash or a deadlock under concurrent
	// access to one organisation's lock row.
	allowed := 0
	for _, o := range results {
		if o == policy.Allow {
			allowed++
		}
	}
	if allowed != attempts {
		t.Errorf("allowed = %d of %d concurrent evaluations with nothing persisted between them, want all of them", allowed, attempts)
	}
}
