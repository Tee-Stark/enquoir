package workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
)

type flakyMailer struct {
	failures int
	err      error
	calls    int
	inner    apptest.Mailer
}

func (m *flakyMailer) Send(ctx context.Context, e app.OutgoingEmail) (string, error) {
	m.calls++
	if m.calls <= m.failures {
		return "", m.err
	}
	return m.inner.Send(ctx, e)
}

func emailEnv(t *testing.T, mailer app.Mailer) (*testsuite.TestWorkflowEnvironment, *apptest.Outbox, string) {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	outbox, clock := apptest.NewOutbox(), apptest.NewClock()
	id, _ := (&app.Emails{Outbox: outbox, Dispatcher: &apptest.Dispatcher{}, IDs: &apptest.IDs{}, Clock: clock}).
		Send(context.Background(), app.Email{To: "ada@example.com", Template: app.TemplatePasswordChanged})
	env.RegisterWorkflow(SendEmailWorkflow)
	env.RegisterActivity(&Activities{
		EmailDelivery: &app.DeliverEmail{Outbox: outbox, Tokens: apptest.NewTokens(), Renderer: apptest.Renderer{}, Mailer: mailer,
			Secrets: &apptest.Secrets{}, Clock: clock, AppBaseURL: "https://enquoir.etimi.me"},
		EmailGiveUp: &app.GiveUpEmail{Outbox: outbox, Clock: clock},
	})
	return env, outbox, id
}

func TestSendEmailWorkflow_RetriesAnUnclearSendUntilItGoesThrough(t *testing.T) {
	mailer := &flakyMailer{failures: 3, err: app.ErrAmbiguous}
	env, outbox, id := emailEnv(t, mailer)
	env.ExecuteWorkflow(SendEmailWorkflow, id)
	require.NoError(t, env.GetWorkflowError())
	e, _ := outbox.Get(context.Background(), id)
	require.Equal(t, app.EmailSent, e.Status)
	require.Equal(t, 4, mailer.calls)
}

func TestSendEmailWorkflow_ARefusalIsNotRetried(t *testing.T) {
	mailer := &flakyMailer{failures: 100, err: app.ErrRejected}
	env, outbox, id := emailEnv(t, mailer)
	env.ExecuteWorkflow(SendEmailWorkflow, id)
	require.NoError(t, env.GetWorkflowError())
	e, _ := outbox.Get(context.Background(), id)
	require.Equal(t, app.EmailFailed, e.Status)
	require.Equal(t, 1, mailer.calls)
}

// Retries stop inside Resend's 24-hour idempotency window, so a late
// retry can never send a second copy; the email is then marked failed.
func TestSendEmailWorkflow_GivesUpBeforeTheIdempotencyWindowCloses(t *testing.T) {
	mailer := &flakyMailer{failures: 1 << 30, err: errors.New("resend down")}
	env, outbox, id := emailEnv(t, mailer)
	env.ExecuteWorkflow(SendEmailWorkflow, id)
	require.NoError(t, env.GetWorkflowError())
	e, _ := outbox.Get(context.Background(), id)
	require.Equal(t, app.EmailFailed, e.Status)
	require.Contains(t, e.Error, "gave up")
	require.Less(t, emailRetryWindow, 24*time.Hour)
}
