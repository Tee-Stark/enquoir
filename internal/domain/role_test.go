package domain

import "testing"

func TestParseRole(t *testing.T) {
	for in, want := range map[string]Role{"owner": Owner, " Admin ": Admin, "OPERATOR": Operator, "viewer": Viewer} {
		if got, err := ParseRole(in); err != nil || got != want {
			t.Errorf("ParseRole(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "root", "read_only"} {
		if _, err := ParseRole(bad); err == nil {
			t.Errorf("ParseRole(%q): want an error", bad)
		}
	}
}

// The permission table, as agreed: everyone reads; operators and up
// write (agents, keys, payees); admins and up manage members and
// merchant integrations; only owners manage admins and ownership.
func TestRole_Can(t *testing.T) {
	table := []struct {
		p                              Permission
		owner, admin, operator, viewer bool
	}{
		{PermRead, true, true, true, true},
		{PermWrite, true, true, true, false},
		{PermManageMembers, true, true, false, false},
		{PermViewAuditLog, true, true, false, false},
		{PermManageAdmins, true, false, false, false},
		{PermManageIntegrations, true, true, false, false},
	}
	for _, row := range table {
		for role, want := range map[Role]bool{Owner: row.owner, Admin: row.admin, Operator: row.operator, Viewer: row.viewer} {
			if got := role.Can(row.p); got != want {
				t.Errorf("%s.Can(%s) = %v, want %v", role, row.p, got, want)
			}
		}
	}
	if Role("ghost").Can(PermRead) {
		t.Error("an unknown role can read")
	}
}

// Nobody grants a role above their own; admins manage operators and
// viewers only; only owners touch admins and owners.
func TestRole_CanAssign(t *testing.T) {
	cases := []struct {
		actor, from, to Role
		ok              bool
	}{
		{Owner, Viewer, Owner, true},
		{Owner, Admin, Viewer, true},
		{Admin, Viewer, Operator, true},
		{Admin, Operator, Admin, true},
		{Admin, Viewer, Owner, false},
		{Admin, Admin, Viewer, false},
		{Admin, Owner, Admin, false},
		{Operator, Viewer, Operator, false},
		{Viewer, Viewer, Viewer, false},
	}
	for _, c := range cases {
		if err := c.actor.CanAssign(c.from, c.to); (err == nil) != c.ok {
			t.Errorf("%s moving %s to %s: err = %v, want ok=%v", c.actor, c.from, c.to, err, c.ok)
		}
	}
}

// Inviting is assigning a role to someone with none.
func TestRole_CanInvite(t *testing.T) {
	if Admin.CanInvite(Admin) != nil || Admin.CanInvite(Owner) == nil || Operator.CanInvite(Viewer) == nil || Owner.CanInvite(Owner) != nil {
		t.Error("invitation rules wrong")
	}
}
