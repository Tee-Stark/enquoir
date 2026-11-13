package domain

import (
	"errors"
	"fmt"
	"strings"
)

// Role is what a member may do in an organisation.
type Role string

const (
	Owner    Role = "owner"    // everything, including admins and ownership
	Admin    Role = "admin"    // members, settings and merchant integrations, up to admin
	Operator Role = "operator" // read and write: agents, keys, payees
	Viewer   Role = "viewer"   // read only
)

// Permission is one thing a role may do.
type Permission string

const (
	PermRead          Permission = "read"
	PermWrite         Permission = "write"
	PermManageMembers Permission = "manage_members"
	PermViewAuditLog  Permission = "view_audit_log"
	PermManageAdmins  Permission = "manage_admins"
	// PermManageIntegrations is connecting merchants for commerce:
	// creating, disabling and enabling merchant integrations and
	// issuing or revoking their keys.
	PermManageIntegrations Permission = "manage_integrations"
	// PermManagePolicy is editing the organisation's spending policy
	// and running its dry-run evaluator.
	PermManagePolicy Permission = "manage_policy"
)

// permissions is the whole policy: adding a role is one line here.
var permissions = map[Role][]Permission{
	Owner:    {PermRead, PermWrite, PermManageMembers, PermViewAuditLog, PermManageAdmins, PermManageIntegrations, PermManagePolicy},
	Admin:    {PermRead, PermWrite, PermManageMembers, PermViewAuditLog, PermManageIntegrations, PermManagePolicy},
	Operator: {PermRead, PermWrite},
	Viewer:   {PermRead},
}

var rank = map[Role]int{Viewer: 1, Operator: 2, Admin: 3, Owner: 4}

// Roles lists every role, lowest first.
func Roles() []Role { return []Role{Viewer, Operator, Admin, Owner} }

// ParseRole reads a role name, case-insensitively.
func ParseRole(s string) (Role, error) {
	r := Role(strings.ToLower(strings.TrimSpace(s)))
	if _, ok := rank[r]; !ok {
		return "", fmt.Errorf("unknown role %q: must be owner, admin, operator or viewer", s)
	}
	return r, nil
}

// Can reports whether the role has a permission.
func (r Role) Can(p Permission) bool {
	for _, have := range permissions[r] {
		if have == p {
			return true
		}
	}
	return false
}

// ErrRoleNotAllowed means a role change or invitation is beyond the
// actor's own role.
var ErrRoleNotAllowed = errors.New("role not allowed")

// CanAssign reports whether r may move a member from one role to
// another. Nobody grants a role above their own, and only an owner
// changes the role of someone at their own level or above - so an
// admin may promote to admin but never demote another admin.
func (r Role) CanAssign(from, to Role) error {
	return r.canSet(rank[from], to)
}

// CanInvite reports whether r may invite someone as role.
func (r Role) CanInvite(role Role) error { return r.canSet(0, role) }

func (r Role) canSet(fromRank int, to Role) error {
	if !r.Can(PermManageMembers) {
		return fmt.Errorf("%s cannot manage members: %w", r, ErrRoleNotAllowed)
	}
	if _, ok := rank[to]; !ok {
		return fmt.Errorf("unknown role %q: %w", to, ErrRoleNotAllowed)
	}
	if rank[to] > rank[r] {
		return fmt.Errorf("%s cannot grant %s: %w", r, to, ErrRoleNotAllowed)
	}
	if r != Owner && fromRank >= rank[r] {
		return fmt.Errorf("only an owner can change a member at %s level or above: %w", r, ErrRoleNotAllowed)
	}
	return nil
}
