package tenant

import (
	"errors"
	"testing"

	"github.com/racetify/racetify-api/internal/domain"
)

func TestAuthorizeMemberChange(t *testing.T) {
	member := func(role MemberRole, status MemberStatus) *TenantMember {
		return &TenantMember{ID: "m", Role: role, Status: status}
	}
	role := func(r MemberRole) *MemberRole { return &r }

	tests := []struct {
		name    string
		actor   MemberRole
		target  *TenantMember
		newRole *MemberRole
		want    error
	}{
		// removal
		{"owner removes staff", RoleOwner, member(RoleStaff, MemberStatusActive), nil, nil},
		{"owner removes admin", RoleOwner, member(RoleAdmin, MemberStatusActive), nil, nil},
		{"admin removes staff", RoleAdmin, member(RoleStaff, MemberStatusActive), nil, nil},
		{"admin cannot remove admin", RoleAdmin, member(RoleAdmin, MemberStatusActive), nil, domain.ErrForbidden},
		{"nobody removes the owner", RoleOwner, member(RoleOwner, MemberStatusActive), nil, domain.ErrForbidden},
		{"admin cannot remove the owner", RoleAdmin, member(RoleOwner, MemberStatusActive), nil, domain.ErrForbidden},
		{"staff cannot remove anyone", RoleStaff, member(RoleStaff, MemberStatusActive), nil, domain.ErrForbidden},
		{"already removed member is not found", RoleOwner, member(RoleStaff, MemberStatusRemoved), nil, domain.ErrNotFound},

		// role change
		{"owner promotes staff to admin", RoleOwner, member(RoleStaff, MemberStatusActive), role(RoleAdmin), nil},
		{"owner demotes admin to staff", RoleOwner, member(RoleAdmin, MemberStatusActive), role(RoleStaff), nil},
		{"admin cannot change roles", RoleAdmin, member(RoleStaff, MemberStatusActive), role(RoleAdmin), domain.ErrForbidden},
		{"admin cannot even set staff", RoleAdmin, member(RoleStaff, MemberStatusActive), role(RoleStaff), domain.ErrForbidden},
		{"cannot assign owner", RoleOwner, member(RoleStaff, MemberStatusActive), role(RoleOwner), domain.ErrInvalidState},
		{"cannot assign unknown role", RoleOwner, member(RoleStaff, MemberStatusActive), role("root"), domain.ErrInvalidState},
		{"cannot change the owner's role", RoleOwner, member(RoleOwner, MemberStatusActive), role(RoleAdmin), domain.ErrForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := authorizeMemberChange(tt.actor, tt.target, tt.newRole)
			if !errors.Is(err, tt.want) && !(err == nil && tt.want == nil) {
				t.Errorf("got %v, want %v", err, tt.want)
			}
		})
	}
}

func TestInvitationLink(t *testing.T) {
	cases := map[[2]string]string{
		{"https://app.racetify.com", "tok"}:   "https://app.racetify.com/invite/tok",
		{"https://lawu.racetify.com/", "tok"}: "https://lawu.racetify.com/invite/tok",
		{"http://localhost:3001", "a-b_c"}:    "http://localhost:3001/invite/a-b_c",
	}
	for in, want := range cases {
		if got := invitationLink(in[0], in[1]); got != want {
			t.Errorf("invitationLink(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}
