package security

import (
	"testing"
	"time"
)

func TestUserAccessTokenRoundTrip(t *testing.T) {
	tm := NewTokenManager("test-secret", "racetify-test")

	token, jti, expiresAt, err := tm.IssueUserAccessToken("user-123", true, "", 15*time.Minute)
	if err != nil {
		t.Fatalf("IssueUserAccessToken: %v", err)
	}
	if jti == "" {
		t.Fatal("expected non-empty jti")
	}
	if !expiresAt.After(time.Now()) {
		t.Fatal("expected expiresAt in the future")
	}

	claims, err := tm.Parse(token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if claims.TokenType != TokenTypeUserAccess {
		t.Fatalf("token type = %q, want %q", claims.TokenType, TokenTypeUserAccess)
	}
	if claims.UserID != "user-123" {
		t.Fatalf("user id = %q, want %q", claims.UserID, "user-123")
	}
	if !claims.IsSuperAdmin {
		t.Fatal("expected IsSuperAdmin to round-trip as true")
	}
	if claims.TenantID != "" {
		t.Fatalf("tenant id = %q, want empty (tenant-less token)", claims.TenantID)
	}
}

func TestUserAccessTokenCanCarryTenant(t *testing.T) {
	tm := NewTokenManager("test-secret", "racetify-test")

	token, _, _, err := tm.IssueUserAccessToken("user-123", false, "tenant-xyz", 15*time.Minute)
	if err != nil {
		t.Fatalf("IssueUserAccessToken: %v", err)
	}

	claims, err := tm.Parse(token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if claims.TokenType != TokenTypeUserAccess {
		t.Fatalf("token type = %q, want %q", claims.TokenType, TokenTypeUserAccess)
	}
	if claims.TenantID != "tenant-xyz" {
		t.Fatalf("tenant id = %q, want %q", claims.TenantID, "tenant-xyz")
	}
}

func TestM2MAccessTokenCarriesTenantAndScopes(t *testing.T) {
	tm := NewTokenManager("test-secret", "racetify-test")

	token, _, _, err := tm.IssueM2MAccessToken("rtf_client_abc", "tenant-xyz", []string{"tenant:read", "timing:write"}, time.Hour)
	if err != nil {
		t.Fatalf("IssueM2MAccessToken: %v", err)
	}

	claims, err := tm.Parse(token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if claims.TokenType != TokenTypeM2MAccess {
		t.Fatalf("token type = %q, want %q", claims.TokenType, TokenTypeM2MAccess)
	}
	if claims.TenantID != "tenant-xyz" {
		t.Fatalf("tenant id = %q, want %q", claims.TenantID, "tenant-xyz")
	}
	if !claims.HasScope("tenant:read") || !claims.HasScope("timing:write") {
		t.Fatalf("expected both granted scopes present, got %v", claims.Scopes)
	}
	if claims.HasScope("events:write") {
		t.Fatal("did not expect an ungranted scope to be present")
	}
}

func TestParseRejectsTamperedSignature(t *testing.T) {
	tm := NewTokenManager("test-secret", "racetify-test")
	other := NewTokenManager("different-secret", "racetify-test")

	token, _, _, err := other.IssueUserAccessToken("user-123", false, "", time.Hour)
	if err != nil {
		t.Fatalf("IssueUserAccessToken: %v", err)
	}

	if _, err := tm.Parse(token); err == nil {
		t.Fatal("expected a token signed with a different secret to fail verification")
	}
}

func TestParseRejectsExpiredToken(t *testing.T) {
	tm := NewTokenManager("test-secret", "racetify-test")

	token, _, _, err := tm.IssueUserAccessToken("user-123", false, "", -time.Minute)
	if err != nil {
		t.Fatalf("IssueUserAccessToken: %v", err)
	}

	if _, err := tm.Parse(token); err == nil {
		t.Fatal("expected an already-expired token to fail verification")
	}
}

func TestParseIgnoringExpiryAcceptsExpiredButValidToken(t *testing.T) {
	tm := NewTokenManager("test-secret", "racetify-test")

	token, _, _, err := tm.IssueUserAccessToken("user-123", false, "tenant-xyz", -time.Minute)
	if err != nil {
		t.Fatalf("IssueUserAccessToken: %v", err)
	}

	// The ordinary path must still reject it...
	if _, err := tm.Parse(token); err == nil {
		t.Fatal("Parse: expected an already-expired token to fail verification")
	}

	// ...but the refresh-flow path recovers its claims anyway.
	claims, err := tm.ParseIgnoringExpiry(token)
	if err != nil {
		t.Fatalf("ParseIgnoringExpiry: %v", err)
	}
	if claims.TenantID != "tenant-xyz" {
		t.Fatalf("tenant id = %q, want %q", claims.TenantID, "tenant-xyz")
	}
}

func TestParseIgnoringExpiryStillRejectsTamperedSignature(t *testing.T) {
	tm := NewTokenManager("test-secret", "racetify-test")
	other := NewTokenManager("different-secret", "racetify-test")

	token, _, _, err := other.IssueUserAccessToken("user-123", false, "tenant-xyz", -time.Minute)
	if err != nil {
		t.Fatalf("IssueUserAccessToken: %v", err)
	}

	if _, err := tm.ParseIgnoringExpiry(token); err == nil {
		t.Fatal("expected a token signed with a different secret to fail verification even with expiry ignored")
	}
}
