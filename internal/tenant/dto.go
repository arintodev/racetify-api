package tenant

import "time"

type TenantDTO struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Slug         string    `json:"slug"`
	Status       string    `json:"status"`
	StatusReason *string   `json:"status_reason,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

func tenantResponse(t *Tenant) TenantDTO {
	return TenantDTO{
		ID: t.ID, Name: t.Name, Slug: t.Slug, Status: string(t.Status),
		StatusReason: t.StatusReason, CreatedAt: t.CreatedAt,
	}
}

type TenantWithRoleDTO struct {
	TenantDTO
	Role string `json:"role"`
}

func tenantWithRoleResponse(t TenantWithRole) TenantWithRoleDTO {
	return TenantWithRoleDTO{TenantDTO: tenantResponse(&t.Tenant), Role: string(t.Role)}
}

type MemberDTO struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	UserID    string    `json:"user_id"`
	Role      string    `json:"role"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	// Email and the names are present on the member list (which joins the
	// person in); responses that only echo a membership omit them.
	Email     string `json:"email,omitempty"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
}

func memberResponse(m *TenantMember) MemberDTO {
	return MemberDTO{ID: m.ID, TenantID: m.TenantID, UserID: m.UserID, Role: string(m.Role), Status: string(m.Status), CreatedAt: m.CreatedAt}
}

func memberWithUserResponse(m *MemberWithUser) MemberDTO {
	dto := memberResponse(&m.TenantMember)
	dto.Email, dto.FirstName, dto.LastName = m.Email, m.FirstName, m.LastName
	return dto
}

type InvitationDTO struct {
	ID         string     `json:"id"`
	Email      string     `json:"email"`
	Role       string     `json:"role"`
	Status     string     `json:"status"`
	ExpiresAt  time.Time  `json:"expires_at"`
	AcceptedAt *time.Time `json:"accepted_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

func invitationResponse(i *Invitation) InvitationDTO {
	return InvitationDTO{
		ID: i.ID, Email: i.Email, Role: string(i.Role), Status: string(i.Status),
		ExpiresAt: i.ExpiresAt, AcceptedAt: i.AcceptedAt, CreatedAt: i.CreatedAt,
	}
}

// InvitationCreatedDTO is only ever returned once, from the creation
// endpoint - like internal/oauthclient's OAuthClientCreatedDTO, it carries
// the one piece of single-use secret material (here, the raw invitation
// token) alongside the normal fields. The inviting Tenant Owner/Admin is a
// legitimate holder of this value (they are the one who just generated it)
// and, in an environment with no real mail provider configured, it is the
// only way they can hand the invite link to their staff member directly;
// it is never returned again from GET /invitations, which only exposes the
// token's hash-backed status.
type InvitationCreatedDTO struct {
	InvitationDTO
	Token string `json:"token"`
}

// tenantSummaryDTO is Handler.Summary's response shape - see
// Service.TenantSummary's doc comment.
type tenantSummaryDTO struct {
	TenantID         string `json:"tenant_id"`
	TenantName       string `json:"tenant_name"`
	OAuthClientCount int    `json:"oauth_client_count"`
}
