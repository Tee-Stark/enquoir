package app_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
	"enquoir/internal/domain"
)

type membersFixture struct {
	*accountsFixture
	invites    *apptest.Invitations
	change     *app.ChangeMemberRole
	remove     *app.RemoveMember
	invite     *app.InviteMember
	accept     *app.AcceptInvitation
	revoke     *app.RevokeInvitation
	org        domain.OrgID
	owner      app.UserPrincipal
	ownerToken string
	raceHook   func()
}

func newMembersFixture(t *testing.T) *membersFixture {
	t.Helper()
	f := &membersFixture{accountsFixture: newAccountsFixture(), invites: apptest.NewInvitations()}
	f.invites.Accounts = f.accounts
	f.withEmail()
	f.invites.Tokens = f.tokens
	f.change = &app.ChangeMemberRole{Members: f.accounts, Sessions: f.sessions, Clock: f.clock}
	f.remove = &app.RemoveMember{Members: f.accounts, Sessions: f.sessions, Clock: f.clock}
	f.invite = &app.InviteMember{Accounts: f.accounts, Members: f.accounts, Invitations: f.invites, Emails: f.emails, IDs: &apptest.IDs{}, Clock: f.clock}
	f.accept = &app.AcceptInvitation{Tokens: f.tokens, Invitations: f.invites, Accounts: f.accounts, Sessions: f.sessions, Hasher: f.hasher, IDs: &apptest.IDs{}, Issuer: f.issuer, Clock: f.clock}
	f.revoke = &app.RevokeInvitation{Invitations: f.invites, Clock: f.clock}
	signed, _ := f.signUp.Run(context.Background(), validSignUp)
	f.org = signed.Organisation.ID
	f.owner, _ = f.resolve.Run(context.Background(), signed.Session.Token)
	f.ownerToken = signed.Session.Token
	return f
}

// join signs someone up elsewhere and adds them to the org with role.
func (f *membersFixture) join(t *testing.T, role domain.Role) (app.UserPrincipal, string) {
	t.Helper()
	email := fmt.Sprintf("m%d@example.com", f.clock.Now().UnixNano())
	f.clock.Advance(time.Millisecond)
	s, err := f.signUp.Run(context.Background(), app.SignUpCommand{Email: email, Password: "correct horse battery", OrganisationName: "Own"})
	if err != nil {
		t.Fatal(err)
	}
	f.accounts.AddMember(f.org, s.User.ID, role, f.clock.Now())
	sw, _ := (&app.SwitchOrganisation{Accounts: f.accounts, Sessions: f.sessions, Issuer: f.issuer}).Run(context.Background(), app.UserPrincipal{User: s.User}, s.Session.Token, f.org)
	p, _ := f.resolve.Run(context.Background(), sw.Session.Token)
	return p, sw.Session.Token
}

func TestChangeMemberRole_FollowsTheRulesAndSignsTheMemberOut(t *testing.T) {
	f := newMembersFixture(t)
	ctx := context.Background()
	admin, _ := f.join(t, domain.Admin)
	viewer, viewerToken := f.join(t, domain.Viewer)
	otherAdmin, _ := f.join(t, domain.Admin)

	if err := f.change.Run(ctx, admin, viewer.User.ID, domain.Operator); err != nil {
		t.Fatalf("admin promoting a viewer: %v", err)
	}
	if m, _ := f.accounts.Membership(ctx, f.org, viewer.User.ID); m.Role != domain.Operator {
		t.Errorf("role = %s", m.Role)
	}
	if _, err := f.resolve.Run(ctx, viewerToken); !errors.Is(err, app.ErrUnauthenticated) {
		t.Error("the member kept their session through a role change")
	}
	if err := f.change.Run(ctx, admin, otherAdmin.User.ID, domain.Viewer); !errors.Is(err, domain.ErrRoleNotAllowed) {
		t.Errorf("admin demoting another admin: %v", err)
	}
	if err := f.change.Run(ctx, admin, viewer.User.ID, domain.Owner); !errors.Is(err, domain.ErrRoleNotAllowed) {
		t.Errorf("admin granting owner: %v", err)
	}
	if err := f.change.Run(ctx, admin, "usr_stranger", domain.Viewer); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("a non-member: %v", err)
	}
}

func TestMembers_AnOrganisationAlwaysKeepsAnOwner(t *testing.T) {
	f := newMembersFixture(t)
	ctx := context.Background()
	if err := f.change.Run(ctx, f.owner, f.owner.User.ID, domain.Admin); !errors.Is(err, app.ErrLastOwner) {
		t.Errorf("the only owner stepping down: %v", err)
	}
	if err := f.remove.Run(ctx, f.owner, f.owner.User.ID); !errors.Is(err, app.ErrLastOwner) {
		t.Errorf("the only owner leaving: %v", err)
	}
	second, _ := f.join(t, domain.Viewer)
	if err := f.change.Run(ctx, f.owner, second.User.ID, domain.Owner); err != nil {
		t.Fatal(err)
	}
	if err := f.change.Run(ctx, f.owner, f.owner.User.ID, domain.Admin); err != nil {
		t.Errorf("stepping down once there is another owner: %v", err)
	}
}

func TestRemoveMember(t *testing.T) {
	f := newMembersFixture(t)
	ctx := context.Background()
	admin, _ := f.join(t, domain.Admin)
	viewer, viewerToken := f.join(t, domain.Viewer)
	operator, _ := f.join(t, domain.Operator)

	if err := f.remove.Run(ctx, admin, viewer.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.resolve.Run(ctx, viewerToken); !errors.Is(err, app.ErrUnauthenticated) {
		t.Error("a removed member's session still works")
	}
	if err := f.remove.Run(ctx, operator, admin.User.ID); !errors.Is(err, domain.ErrRoleNotAllowed) {
		t.Errorf("operator removing an admin: %v", err)
	}
	if err := f.remove.Run(ctx, operator, operator.User.ID); err != nil {
		t.Errorf("anyone may leave: %v", err)
	}
}

func TestInvitation_ANewAccount(t *testing.T) {
	f := newMembersFixture(t)
	ctx := context.Background()
	inv, err := f.invite.Run(ctx, f.owner, "  Bob@Example.com ", domain.Operator)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Email != "bob@example.com" || !inv.ExpiresAt.Equal(f.clock.Now().Add(7*24*time.Hour)) {
		t.Errorf("invitation = %+v", inv)
	}
	token := f.mailer.TokenTo("bob@example.com")
	if token == "" {
		t.Fatal("no invitation email")
	}

	var verr *app.ValidationError
	if _, err := f.accept.Run(ctx, token, "short"); !errors.As(err, &verr) {
		t.Fatalf("a weak password for a new account: %v", err)
	}
	got, err := f.accept.Run(ctx, token, "bobs long passphrase")
	if err != nil {
		t.Fatalf("the link must survive a rejected password: %v", err)
	}
	if !got.AccountCreated || got.Session == nil || got.Organisation.ID != f.org || got.Role != domain.Operator {
		t.Errorf("accepted = %+v", got)
	}
	bob, _ := f.accounts.UserByEmail(ctx, "bob@example.com")
	if !bob.EmailVerified {
		t.Error("an account created from an invitation link is not verified")
	}
	if _, err := f.accept.Run(ctx, token, "bobs long passphrase"); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("the link twice: %v", err)
	}
}

func TestInvitation_AnExistingVerifiedAccountJoinsWithoutANewPasswordOrSession(t *testing.T) {
	f := newMembersFixture(t)
	ctx := context.Background()
	f.signUp.Run(ctx, app.SignUpCommand{Email: "carol@example.com", Password: "carols passphrase", OrganisationName: "Carol Co"})
	// A verified account: it has proved it owns the address.
	if err := (&app.VerifyEmail{Tokens: f.tokens, Users: f.accounts, Clock: f.clock}).Run(ctx, f.mailer.TokenTo("carol@example.com")); err != nil {
		t.Fatal(err)
	}
	f.invite.Run(ctx, f.owner, "carol@example.com", domain.Viewer)
	got, err := f.accept.Run(ctx, f.mailer.TokenTo("carol@example.com"), "")
	if err != nil || got.AccountCreated || got.Session != nil {
		t.Fatalf("accepted = %+v, %v", got, err)
	}
	carol, _ := f.accounts.UserByEmail(ctx, "carol@example.com")
	if m, err := f.accounts.Membership(ctx, f.org, carol.ID); err != nil || m.Role != domain.Viewer {
		t.Errorf("membership = %+v, %v", m, err)
	}
}

func TestInvitation_Rules(t *testing.T) {
	f := newMembersFixture(t)
	ctx := context.Background()
	admin, _ := f.join(t, domain.Admin)
	operator, _ := f.join(t, domain.Operator)

	if _, err := f.invite.Run(ctx, admin, "x@example.com", domain.Owner); !errors.Is(err, domain.ErrRoleNotAllowed) {
		t.Errorf("admin inviting an owner: %v", err)
	}
	if _, err := f.invite.Run(ctx, operator, "x@example.com", domain.Viewer); !errors.Is(err, domain.ErrRoleNotAllowed) {
		t.Errorf("operator inviting: %v", err)
	}
	if _, err := f.invite.Run(ctx, admin, string(operator.User.Email), domain.Viewer); !errors.Is(err, app.ErrAlreadyMember) {
		t.Errorf("inviting a member: %v", err)
	}

	inv, _ := f.invite.Run(ctx, admin, "dan@example.com", domain.Viewer)
	if err := f.revoke.Run(ctx, f.owner, inv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.accept.Run(ctx, f.mailer.TokenTo("dan@example.com"), "dans long passphrase"); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("a revoked invitation: %v", err)
	}

	f.invite.Run(ctx, admin, "erin@example.com", domain.Viewer)
	f.clock.Advance(7*24*time.Hour + time.Second)
	if _, err := f.accept.Run(ctx, f.mailer.TokenTo("erin@example.com"), "erins long passphrase"); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("an expired invitation: %v", err)
	}
}

// An attacker pre-registers the address someone will be invited at,
// with their own password, and never verifies it (they can't: the link
// goes to the real owner). The invitation link proves who owns the
// address, so accepting it claims the account for them: a new password
// replaces the attacker's, and the attacker's sessions end.
func TestInvitation_AnUnverifiedAccountIsClaimedByWhoeverHoldsTheLink(t *testing.T) {
	f := newMembersFixture(t)
	ctx := context.Background()
	attacker, err := f.signUp.Run(ctx, app.SignUpCommand{Email: "bob@example.com", Password: "attacker passphrase", OrganisationName: "Trap"})
	if err != nil {
		t.Fatal(err)
	}
	f.invite.Run(ctx, f.owner, "bob@example.com", domain.Operator)
	token := f.mailer.TokenTo("bob@example.com")

	var verr *app.ValidationError
	if _, err := f.accept.Run(ctx, token, ""); !errors.As(err, &verr) || verr.Field != "password" {
		t.Fatalf("accepting for an unverified account without a password: %v, want a password to be required", err)
	}
	got, err := f.accept.Run(ctx, token, "the real bobs passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if !got.AccountClaimed || got.AccountCreated || got.Session == nil || got.Role != domain.Operator || got.User.ID != attacker.User.ID {
		t.Errorf("accepted = %+v; want the account claimed, signed in, as operator", got)
	}
	if _, err := f.resolve.Run(ctx, attacker.Session.Token); !errors.Is(err, app.ErrUnauthenticated) {
		t.Error("the pre-registering attacker's session survived the claim")
	}
	if _, err := f.logIn.Run(ctx, "bob@example.com", "attacker passphrase"); !errors.Is(err, app.ErrInvalidCredentials) {
		t.Errorf("the attacker's password still works: %v", err)
	}
	if _, err := f.logIn.Run(ctx, "bob@example.com", "the real bobs passphrase"); err != nil {
		t.Errorf("the real owner's password: %v", err)
	}
	if bob, _ := f.accounts.UserByEmail(ctx, "bob@example.com"); !bob.EmailVerified {
		t.Error("the claimed account is not verified")
	}
}

// An invitation is as senior as the role it grants: an admin may not
// revoke an owner's invitation, nor replace it by re-inviting the same
// address with a lesser role.
func TestInvitation_AdminsCannotTouchInvitationsAboveTheirRole(t *testing.T) {
	f := newMembersFixture(t)
	ctx := context.Background()
	admin, _ := f.join(t, domain.Admin)
	inv, err := f.invite.Run(ctx, f.owner, "cofounder@example.com", domain.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.revoke.Run(ctx, admin, inv.ID); !errors.Is(err, domain.ErrRoleNotAllowed) {
		t.Errorf("admin revoking an owner invitation: %v", err)
	}
	if _, err := f.invite.Run(ctx, admin, "cofounder@example.com", domain.Viewer); !errors.Is(err, domain.ErrRoleNotAllowed) {
		t.Errorf("admin replacing an owner invitation: %v", err)
	}
	pending, _ := f.invites.ListPendingInvitations(ctx, f.org, f.clock.Now())
	if len(pending) != 1 || pending[0].Role != domain.Owner {
		t.Errorf("pending = %+v, want the owner invitation untouched", pending)
	}
	if err := f.revoke.Run(ctx, f.owner, inv.ID); err != nil {
		t.Errorf("the owner revoking it: %v", err)
	}
}
