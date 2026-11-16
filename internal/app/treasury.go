package app

import (
	"context"
	"time"

	"enquoir/internal/domain"
)

// Treasury is an organisation's logical treasury: an attribution of
// payments and payees over the one rail account Enquoir pays through.
// It holds no money of its own at the rail, and nothing presents it as
// a segregated balance. Every organisation has exactly one for now,
// created with it.
type Treasury struct {
	ID        domain.TreasuryID
	OrgID     domain.OrgID
	Name      string
	CreatedAt time.Time
}

// DefaultTreasuryName names the treasury an organisation is created with.
const DefaultTreasuryName = "Main treasury"

// TreasuryStore reads organisations' treasuries.
type TreasuryStore interface {
	// TreasuryOf is org's treasury - its only one, for now.
	TreasuryOf(ctx context.Context, org domain.OrgID) (Treasury, error)
	// ListTreasuries lists org's treasuries.
	ListTreasuries(ctx context.Context, org domain.OrgID) ([]Treasury, error)
}

// ListTreasuries lists an organisation's treasuries.
type ListTreasuries struct{ Treasuries TreasuryStore }

func (l *ListTreasuries) Run(ctx context.Context, org domain.OrgID) ([]Treasury, error) {
	return l.Treasuries.ListTreasuries(ctx, org)
}
