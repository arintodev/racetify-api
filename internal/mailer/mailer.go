// Package mailer defines the outbound-email boundary used by the
// verification-OTP/magic-link and staff-invitation flows. Phase 0 ships a
// LogMailer that writes the would-be email to the structured logger
// instead of an SMTP/SES/SendGrid integration, so the flows are fully
// exercisable (and testable) in this sandbox without provisioning a real
// mail provider or reaching a host outside its network allowlist. Swapping
// in a real provider means implementing Mailer and wiring it in
// internal/app/app.go - nothing in the auth/tenant bounded contexts that
// consume Mailer needs to change.
package mailer

import (
	"context"
	"log/slog"
)

type Mailer interface {
	SendVerificationEmail(ctx context.Context, toEmail, toName, verifyLink string) error
	SendPasswordResetEmail(ctx context.Context, toEmail, toName, resetLink string) error
	SendInvitationEmail(ctx context.Context, toEmail, tenantName, inviterName, acceptLink string) error
	// SendEventInvitationEmail is the event-scoped crew/volunteer
	// counterpart to SendInvitationEmail (docs/event-crew-access-plan.md
	// §3) - a separate method rather than reusing SendInvitationEmail with
	// eventName in place of tenantName, since the invitee here is not
	// being invited into the tenant workspace at all.
	SendEventInvitationEmail(ctx context.Context, toEmail, eventName, inviterName, acceptLink string) error
}

type LogMailer struct {
	logger *slog.Logger
}

func NewLogMailer(logger *slog.Logger) *LogMailer {
	return &LogMailer{logger: logger}
}

func (m *LogMailer) SendVerificationEmail(ctx context.Context, toEmail, toName, verifyLink string) error {
	m.logger.InfoContext(ctx, "mailer: verification email",
		"to", toEmail, "name", toName, "link", verifyLink)
	return nil
}

func (m *LogMailer) SendPasswordResetEmail(ctx context.Context, toEmail, toName, resetLink string) error {
	m.logger.InfoContext(ctx, "mailer: password reset email",
		"to", toEmail, "name", toName, "link", resetLink)
	return nil
}

func (m *LogMailer) SendInvitationEmail(ctx context.Context, toEmail, tenantName, inviterName, acceptLink string) error {
	m.logger.InfoContext(ctx, "mailer: tenant staff invitation email",
		"to", toEmail, "tenant", tenantName, "invited_by", inviterName, "link", acceptLink)
	return nil
}

func (m *LogMailer) SendEventInvitationEmail(ctx context.Context, toEmail, eventName, inviterName, acceptLink string) error {
	m.logger.InfoContext(ctx, "mailer: event crew/volunteer invitation email",
		"to", toEmail, "event", eventName, "invited_by", inviterName, "link", acceptLink)
	return nil
}
