//go:build integration

package postgres

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"enquoir/internal/app"
)

func TestEmailOutbox_QueueGetAndFirstOutcomeStands(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	_, user := newTestOrg(t, NewAccountStore(pool))
	o := NewEmailOutbox(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	e := app.Email{ID: ids.NewID("eml"), UserID: user.ID, To: user.Email, Template: app.TemplateVerifyEmail,
		Params: map[string]string{"organisation": "Acme"}, Status: app.EmailQueued, CreatedAt: now,
		Link: &app.LinkSpec{Purpose: app.PurposeVerifyEmail, Subject: string(user.ID), TTL: 24 * time.Hour}}
	if err := o.Queue(ctx, e); err != nil {
		t.Fatal(err)
	}
	got, err := o.Get(ctx, e.ID)
	if err != nil || got.Link == nil || got.Link.TTL != 24*time.Hour || got.Params["organisation"] != "Acme" || got.UserID != user.ID {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	queued, _ := o.Queued(ctx, now.Add(time.Second), 10)
	if !contains(queued, e.ID) {
		t.Error("a queued email is not listed for the sweep")
	}
	o.MarkSent(ctx, e.ID, "re_1", now)
	o.MarkFailed(ctx, e.ID, "too late", now)
	if got, _ := o.Get(ctx, e.ID); got.Status != app.EmailSent || got.ProviderID != "re_1" {
		t.Errorf("status = %s, want sent to stand", got.Status)
	}
}

// A link works once, even when used twice at the same moment.
func TestTokenStore_ConsumeIsOnceOnly(t *testing.T) {
	ctx := context.Background()
	s := NewTokenStore(testPool(t))
	now := time.Now().UTC()
	hash := app.HashSecret(ids.NewID("tok"))
	sibling := app.HashSecret(ids.NewID("tok"))
	subject := ids.NewID("usr")
	for _, h := range []string{hash, sibling} {
		if err := s.Create(ctx, app.OneTimeToken{Hash: h, Purpose: app.PurposeVerifyEmail, Subject: subject, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Consume(ctx, hash, app.PurposeResetPassword, now); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("wrong purpose: %v", err)
	}

	var ok int32
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if t2, err := s.Consume(ctx, hash, app.PurposeVerifyEmail, now); err == nil && t2.Subject == subject {
				atomic.AddInt32(&ok, 1)
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Errorf("%d concurrent uses succeeded, want exactly 1", ok)
	}
	if _, err := s.Consume(ctx, sibling, app.PurposeVerifyEmail, now); !errors.Is(err, app.ErrTokenInvalid) {
		t.Error("an older link for the same purpose still works after one was used")
	}

	expired := app.HashSecret(ids.NewID("tok"))
	s.Create(ctx, app.OneTimeToken{Hash: expired, Purpose: app.PurposeVerifyEmail, Subject: "x", CreatedAt: now, ExpiresAt: now.Add(-time.Second)})
	if _, err := s.Consume(ctx, expired, app.PurposeVerifyEmail, now); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("expired: %v", err)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
