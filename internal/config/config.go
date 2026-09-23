// Package config loads runtime configuration for the Racetify API from
// environment variables (optionally seeded from a local .env file in
// development). No third-party dependency is used here on purpose: see
// README.md "Dependency footprint" for why.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every tunable the service needs at boot. Fields are grouped
// by subsystem to keep call sites self-documenting (cfg.HTTP.Port, etc).
type Config struct {
	Env     string // "development", "staging", "production"
	HTTP    HTTPConfig
	DB      DBConfig
	Redis   RedisConfig
	Auth    AuthConfig
	Google  GoogleConfig
	CORS    CORSConfig
	Storage StorageConfig
}

type HTTPConfig struct {
	Port            string
	ShutdownTimeout time.Duration
}

type DBConfig struct {
	// DSN is what the API server uses for the overwhelming majority of
	// queries: authenticated as the generic app role (AppRoleName below),
	// fully subject to Row-Level Security
	// (migrations/0003_row_level_security.up.sql).
	DSN string

	// AdminDSN authenticates as the generic admin role (AdminRoleName
	// below, BYPASSRLS) and is used ONLY for the small,
	// explicitly-documented set of pre-tenant-context lookups that
	// authenticate a caller by an unguessable secret they present (an
	// OAuth client_secret, an invitation token) or that are provably
	// scoped to the caller's own verified identity (listing which tenants
	// the current user belongs to) - see the doc comments on
	// InvitationRepository.GetByTokenHash, OAuthClientRepository.
	// GetByClientID, and TenantRepository.ListForUser. It must never be
	// used for general tenant data access.
	AdminDSN string

	// MigratorDSN authenticates as a role with DDL privileges (able to
	// CREATE TABLE, CREATE ROLE, ALTER TABLE ... FORCE ROW LEVEL SECURITY,
	// etc - migrations/0003_row_level_security.up.sql creates the two
	// roles named by AppRoleName/AdminRoleName below). It is used ONLY by
	// cmd/migrate, never by the running API server: the server's own
	// runtime credentials (DSN, AdminDSN) intentionally have no
	// schema-modification rights, so a compromised or buggy request
	// handler can never alter the schema or its RLS policies. In local
	// dev this defaults to the Postgres bootstrap superuser; in
	// staging/production it should be a dedicated migration role used
	// only by the deploy pipeline's one-shot migrate step.
	MigratorDSN string

	// AppRoleName/AppRolePassword and AdminRoleName/AdminRolePassword are
	// the two Postgres roles 0003_row_level_security.up.sql creates (via
	// cmd/migrate's {{APP_ROLE}}/{{ADMIN_ROLE}} templating - see that
	// migration's doc comment and internal/platform/database.MigrationSet).
	// Named generically here (not after this product) since the
	// name/password of each is an operational deployment detail the schema
	// should never hardcode. These must stay consistent with DSN/AdminDSN
	// above - both name the same two roles, just for two different
	// purposes (connecting vs. provisioning).
	AppRoleName       string
	AppRolePassword   string
	AdminRoleName     string
	AdminRolePassword string

	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

type RedisConfig struct {
	Addr     string
	Password string
	DB       int
}

type AuthConfig struct {
	// JWTSecret signs both user-session and M2M tokens in this phase.
	// In production this should be an asymmetric key (RS256/ES256) rotated
	// via a KMS; HS256 + a long random secret is the pragmatic Phase 0
	// baseline called out in the implementation guide.
	JWTSecret             string
	Issuer                string
	AccessTokenTTL        time.Duration // user session access token (15-30 min)
	RefreshTokenTTL       time.Duration // user session refresh token (7-14 days)
	M2MTokenTTL           time.Duration // client_credentials access token (60 min)
	EmailVerifyTokenTTL   time.Duration
	InvitationTokenTTL    time.Duration
	PasswordResetTokenTTL time.Duration
}

type GoogleConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

type CORSConfig struct {
	AllowedOrigins []string
}

// StorageConfig configures the Object Storage module
// (internal/platform/objectstorage): the implementation guide's §3
// deliverable ("Object Storage dapat menerima unggahan via API dan
// menyajikan presigned URL untuk file terenkripsi"), satisfied by one of
// two swappable drivers - see objectstorage.go's package doc for the
// Laravel-Storage/AdonisJS-Drive-style pattern this follows. Driver picks
// which; every other field mirrors objectstorage.Config field-for-field
// so internal/app.Build can pass this struct straight through.
type StorageConfig struct {
	// Driver selects the backend: "local" (default, self-hosted disk, no
	// external account needed) or "r2" (Cloudflare R2 / any S3-compatible
	// bucket). See objectstorage.NewDriver.
	Driver string

	// PresignSecret signs upload/download URLs (HMAC-SHA256), used by
	// every driver. Anyone who can compute a valid signature can
	// read/write within its (bucket, tenant, key, expiry) scope, so this
	// must be kept as secret as JWTSecret.
	PresignSecret string
	// EncryptionKeyHex is a 32-byte AES-256 key, hex-encoded (64 hex
	// chars), used by every driver. Every object is encrypted with this
	// key before it reaches the backend - see objectstorage.go for the
	// "why not real KMS-managed per-object keys yet" trade-off.
	EncryptionKeyHex string
	// PublicBaseURL prefixes the stable, unsigned URLs handed out for the
	// public bucket (tenant logos, photo thumbnails - meant to be publicly
	// cacheable per the guide's bucket split), used by every driver.
	PublicBaseURL string
	UploadTTL     time.Duration
	DownloadTTL   time.Duration

	// RootDir is "local"-only: where encrypted object bytes are written,
	// namespaced as <RootDir>/<bucket>/<tenant_id>/<key>.
	RootDir string

	// R2 is "r2"-only - see objectstorage.R2Config's field docs.
	R2 StorageR2Config
}

type StorageR2Config struct {
	AccountID       string
	Endpoint        string
	AccessKeyID     string
	AccessKeySecret string
	Bucket          string
	// PublicBaseURL, when set, prefixes a stable, unsigned public-bucket
	// URL (a custom domain or r2.dev subdomain mapped to Bucket). Left
	// blank, the r2 driver falls back to a long-TTL presigned URL instead
	// - see objectstorage.R2Config.PublicBaseURL's doc comment.
	PublicBaseURL string
}

// Load reads a .env file (if present, without overriding already-exported
// environment variables) and then materializes a Config from the process
// environment, applying sane development defaults where a value is optional.
func Load() (*Config, error) {
	loadDotEnv(".env")

	cfg := &Config{
		Env: getEnv("APP_ENV", "development"),
		HTTP: HTTPConfig{
			Port:            getEnv("HTTP_PORT", "8080"),
			ShutdownTimeout: getEnvDuration("HTTP_SHUTDOWN_TIMEOUT", 10*time.Second),
		},
		DB: DBConfig{
			DSN:               getEnv("DATABASE_URL", "postgres://app_user:app_user_dev_password@localhost:5432/racetify?sslmode=disable"),
			AdminDSN:          getEnv("DATABASE_ADMIN_URL", "postgres://platform_admin:platform_admin_dev_password@localhost:5432/racetify?sslmode=disable"),
			MigratorDSN:       getEnv("DATABASE_MIGRATOR_URL", "postgres://postgres:postgres@localhost:5432/racetify?sslmode=disable"),
			AppRoleName:       getEnv("DB_APP_ROLE", "app_user"),
			AppRolePassword:   getEnv("DB_APP_ROLE_PASSWORD", "app_user_dev_password"),
			AdminRoleName:     getEnv("DB_ADMIN_ROLE", "platform_admin"),
			AdminRolePassword: getEnv("DB_ADMIN_ROLE_PASSWORD", "platform_admin_dev_password"),
			MaxOpenConns:      getEnvInt("DB_MAX_OPEN_CONNS", 20),
			MaxIdleConns:      getEnvInt("DB_MAX_IDLE_CONNS", 10),
			ConnMaxLifetime:   getEnvDuration("DB_CONN_MAX_LIFETIME", 30*time.Minute),
		},
		Redis: RedisConfig{
			Addr:     getEnv("REDIS_ADDR", "localhost:6379"),
			Password: getEnv("REDIS_PASSWORD", ""),
			DB:       getEnvInt("REDIS_DB", 0),
		},
		Auth: AuthConfig{
			JWTSecret:             getEnv("JWT_SECRET", ""),
			Issuer:                getEnv("JWT_ISSUER", "racetify"),
			AccessTokenTTL:        getEnvDuration("ACCESS_TOKEN_TTL", 20*time.Minute),
			RefreshTokenTTL:       getEnvDuration("REFRESH_TOKEN_TTL", 10*24*time.Hour),
			M2MTokenTTL:           getEnvDuration("M2M_TOKEN_TTL", 60*time.Minute),
			EmailVerifyTokenTTL:   getEnvDuration("EMAIL_VERIFY_TOKEN_TTL", 24*time.Hour),
			InvitationTokenTTL:    getEnvDuration("INVITATION_TOKEN_TTL", 7*24*time.Hour),
			PasswordResetTokenTTL: getEnvDuration("PASSWORD_RESET_TOKEN_TTL", time.Hour),
		},
		Google: GoogleConfig{
			ClientID:     getEnv("GOOGLE_CLIENT_ID", ""),
			ClientSecret: getEnv("GOOGLE_CLIENT_SECRET", ""),
			RedirectURL:  getEnv("GOOGLE_REDIRECT_URL", ""),
		},
		CORS: CORSConfig{
			AllowedOrigins: splitCSV(getEnv("CORS_ALLOWED_ORIGINS", "http://localhost:3000")),
		},
		Storage: StorageConfig{
			Driver:           getEnv("STORAGE_DRIVER", "local"),
			RootDir:          getEnv("STORAGE_ROOT_DIR", "./data/storage"),
			PresignSecret:    getEnv("STORAGE_PRESIGN_SECRET", ""),
			EncryptionKeyHex: getEnv("STORAGE_ENCRYPTION_KEY", ""),
			PublicBaseURL:    getEnv("STORAGE_PUBLIC_BASE_URL", "http://localhost:8080"),
			UploadTTL:        getEnvDuration("STORAGE_UPLOAD_TTL", 15*time.Minute),
			DownloadTTL:      getEnvDuration("STORAGE_DOWNLOAD_TTL", 15*time.Minute),
			R2: StorageR2Config{
				AccountID:       getEnv("STORAGE_R2_ACCOUNT_ID", ""),
				Endpoint:        getEnv("STORAGE_R2_ENDPOINT", ""),
				AccessKeyID:     getEnv("STORAGE_R2_ACCESS_KEY_ID", ""),
				AccessKeySecret: getEnv("STORAGE_R2_ACCESS_KEY_SECRET", ""),
				Bucket:          getEnv("STORAGE_R2_BUCKET", ""),
				PublicBaseURL:   getEnv("STORAGE_R2_PUBLIC_BASE_URL", ""),
			},
		},
	}

	if cfg.Auth.JWTSecret == "" {
		if cfg.Env == "production" {
			return nil, fmt.Errorf("config: JWT_SECRET is required in production")
		}
		cfg.Auth.JWTSecret = "dev-only-insecure-secret-change-me"
	}
	if cfg.Storage.PresignSecret == "" {
		if cfg.Env == "production" {
			return nil, fmt.Errorf("config: STORAGE_PRESIGN_SECRET is required in production")
		}
		cfg.Storage.PresignSecret = "dev-only-insecure-presign-secret-change-me"
	}
	if cfg.Storage.EncryptionKeyHex == "" {
		if cfg.Env == "production" {
			return nil, fmt.Errorf("config: STORAGE_ENCRYPTION_KEY is required in production")
		}
		// 32 fixed, publicly-known dev bytes, hex-encoded (64 hex chars) -
		// insecure by construction, same pattern as JWTSecret's dev
		// fallback above. Never used when APP_ENV=production.
		cfg.Storage.EncryptionKeyHex = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	}

	return cfg, nil
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

// loadDotEnv is a deliberately tiny KEY=VALUE parser so the service does not
// need the joho/godotenv module. It never overrides a variable already set
// in the real environment (12-factor precedence) and silently no-ops if the
// file does not exist.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, value)
		}
	}
}
