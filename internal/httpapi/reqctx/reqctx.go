// Package reqctx defines the typed request-context keys every middleware
// and handler in internal/httpapi shares, so "how do I read the
// authenticated user" has exactly one answer across the codebase.
package reqctx

import "context"

type ctxKey int

const (
	keyUserID ctxKey = iota
	keySuperAdmin
	keyTenantID
	keyMemberRole
	keyM2MClientID
	keyM2MScopes
	keyAccessJTI
	keyAccessExpiresAt
	keyRequestID
	keyEventCapabilities
)

func WithUserID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyUserID, id)
}

func UserID(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(keyUserID).(string)
	return v, ok
}

func WithSuperAdmin(ctx context.Context, v bool) context.Context {
	return context.WithValue(ctx, keySuperAdmin, v)
}

func IsSuperAdmin(ctx context.Context) bool {
	v, _ := ctx.Value(keySuperAdmin).(bool)
	return v
}

func WithTenantID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyTenantID, id)
}

func TenantID(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(keyTenantID).(string)
	return v, ok
}

func WithMemberRole(ctx context.Context, role string) context.Context {
	return context.WithValue(ctx, keyMemberRole, role)
}

func MemberRole(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(keyMemberRole).(string)
	return v, ok
}

func WithM2MClientID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyM2MClientID, id)
}

func M2MClientID(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(keyM2MClientID).(string)
	return v, ok
}

func WithM2MScopes(ctx context.Context, scopes []string) context.Context {
	return context.WithValue(ctx, keyM2MScopes, scopes)
}

func M2MScopes(ctx context.Context) []string {
	v, _ := ctx.Value(keyM2MScopes).([]string)
	return v
}

func WithAccessTokenMeta(ctx context.Context, jti string, expiresAtUnix int64) context.Context {
	ctx = context.WithValue(ctx, keyAccessJTI, jti)
	return context.WithValue(ctx, keyAccessExpiresAt, expiresAtUnix)
}

func AccessTokenJTI(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(keyAccessJTI).(string)
	return v, ok
}

func AccessTokenExpiresAtUnix(ctx context.Context) (int64, bool) {
	v, ok := ctx.Value(keyAccessExpiresAt).(int64)
	return v, ok
}

// WithEventCapabilities/EventCapabilities carry the capability set an
// event.RequireEventAccess Path B (assignment-based) caller was granted -
// set only on that path, so a handler can tell an externally-assigned
// crew/volunteer caller apart from an internal tenant-role caller (which
// instead has MemberRole set, per RequireEventAccess's Path A) without a
// second database round trip. See internal/event/access.go.
func WithEventCapabilities(ctx context.Context, capabilities []string) context.Context {
	return context.WithValue(ctx, keyEventCapabilities, capabilities)
}

func EventCapabilities(ctx context.Context) ([]string, bool) {
	v, ok := ctx.Value(keyEventCapabilities).([]string)
	return v, ok
}

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyRequestID, id)
}

func RequestID(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(keyRequestID).(string)
	return v, ok
}
