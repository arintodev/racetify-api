//go:build integration

// Integration test of the gallery against a real Postgres. It creates a
// throwaway database next to the one in DATABASE_MIGRATOR_URL, applies every
// migration to it, and drops it afterwards, so it never touches dev data:
//
//	go test -tags=integration ./internal/gallery/... -v
package gallery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	_ "github.com/lib/pq"

	"github.com/racetify/racetify-api/internal/audit"
	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/objectstorage"
	"github.com/racetify/racetify-api/internal/platform/pagination"
	"github.com/racetify/racetify-api/internal/platform/rbac"
	"github.com/racetify/racetify-api/internal/security"
	"github.com/racetify/racetify-api/internal/storage"
	"github.com/racetify/racetify-api/migrations"
)

func withDatabase(t *testing.T, dsn, name string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

type fixture struct {
	svc      *Service
	storage  *storage.Service
	store    objectstorage.ProxyDriver
	super    *sql.DB
	tenantID string
	eventID  string
	userID   string
	otherID  string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Skipf("config: %v", err)
	}
	ctx := context.Background()

	name := "gallery_it_" + strings.ReplaceAll(security.MustNewUUIDv4()[:8], "-", "")
	root, err := sql.Open("postgres", withDatabase(t, cfg.DB.MigratorDSN, "postgres"))
	if err != nil {
		t.Fatal(err)
	}
	if err := root.PingContext(ctx); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	if _, err := root.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
		root.Close()
	})

	migratorCfg := cfg.DB
	migratorCfg.DSN = withDatabase(t, cfg.DB.MigratorDSN, name)
	migrator, err := database.Open(ctx, migratorCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { migrator.Close() })
	vars := map[string]string{
		"APP_ROLE":                 cfg.DB.AppRoleName,
		"APP_ROLE_PASSWORD":        strings.ReplaceAll(cfg.DB.AppRolePassword, "'", "''"),
		"ADMIN_ROLE":               cfg.DB.AdminRoleName,
		"ADMIN_ROLE_PASSWORD":      strings.ReplaceAll(cfg.DB.AdminRolePassword, "'", "''"),
		"STORAGE_DEFAULT_PROVIDER": "local",
	}
	if err := migrator.Migrate(ctx, database.MigrationSet{FS: migrations.FS, Dir: ".", Vars: vars}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	appCfg := cfg.DB
	appCfg.DSN = withDatabase(t, cfg.DB.DSN, name)
	appDB, err := database.Open(ctx, appCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { appDB.Close() })
	adminCfg := cfg.DB
	adminCfg.DSN = withDatabase(t, cfg.DB.AdminDSN, name)
	adminDB, err := database.Open(ctx, adminCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adminDB.Close() })

	driver, err := objectstorage.NewDriver(objectstorage.Config{
		Driver: "local", RootDir: t.TempDir(),
		PresignSecret: "test-presign-secret", PublicBaseURL: "http://localhost:8080",
		EncryptionKeyHex: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
	})
	if err != nil {
		t.Fatal(err)
	}
	auditRepo := audit.NewRepository(appDB)
	storageSvc := storage.NewService(appDB, storage.NewRepository(appDB, adminDB), auditRepo, driver, cfg.Storage)

	// Seed through the migrator connection, which bypasses row-level security.
	super := migrator.DB
	f := &fixture{
		svc:     NewService(appDB, NewRepository(appDB), auditRepo, storageSvc, driver, cfg.Storage),
		storage: storageSvc, store: driver.(objectstorage.ProxyDriver), super: super,
		tenantID: security.MustNewUUIDv4(), eventID: security.MustNewUUIDv4(),
		userID: security.MustNewUUIDv4(), otherID: security.MustNewUUIDv4(),
	}
	seed := func(query string, args ...any) {
		t.Helper()
		if _, err := super.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, query)
		}
	}
	for i, id := range []string{f.userID, f.otherID} {
		seed(`INSERT INTO users (id, email, first_name, last_name) VALUES ($1, $2, $3, 'Tester')`,
			id, fmt.Sprintf("user%d@example.com", i), []string{"Budi", "Sinta"}[i])
	}
	seed(`INSERT INTO tenants (id, name, slug, owner_user_id) VALUES ($1, 'T', 't', $2)`, f.tenantID, f.userID)
	seed(`INSERT INTO events (id, tenant_id, name, slug, created_by) VALUES ($1, $2, 'E', 'e', $3)`, f.eventID, f.tenantID, f.userID)
	return f
}

// put stands in for the browser's PUT to the presigned URL.
func (f *fixture) put(t *testing.T, albumID, name string, size int64, body string) {
	t.Helper()
	key := objectKey(albumID, name, size)
	sum, n, err := f.store.Put(objectstorage.BucketPrivate, f.tenantID, key, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.storage.CompletePut(context.Background(), f.tenantID, objectstorage.BucketPrivate, key, sum, n); err != nil {
		t.Fatal(err)
	}
}

func TestGalleryUploadFlow(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	// ---- albums ----
	album, err := f.svc.CreateAlbum(ctx, f.tenantID, f.eventID, f.userID, rbac.RoleStaff, AlbumInput{Name: " Finish Line "})
	if err != nil {
		t.Fatalf("create album: %v", err)
	}
	if album.Name != "Finish Line" || album.IsPublic {
		t.Fatalf("new album should be a trimmed draft, got %+v", album)
	}
	_, err = f.svc.CreateAlbum(ctx, f.tenantID, f.eventID, f.userID, rbac.RoleStaff, AlbumInput{Name: "finish line"})
	var gerr *Error
	if !errors.As(err, &gerr) || gerr.Code != "album_name_taken" {
		t.Fatalf("same name in another case should conflict, got %v", err)
	}
	if _, err := f.svc.CreateAlbum(ctx, f.tenantID, f.eventID, f.userID, "", AlbumInput{Name: "x"}); err == nil {
		t.Fatal("a caller without a role must not create albums")
	}

	// ---- upload-urls: 4 files, one duplicated in the batch ----
	names := []string{"a.jpg", "b.jpg", "c.jpg", "d.jpg"}
	entries := []UploadEntry{{"a.jpg", 100}, {"b.jpg", 200}, {"c.jpg", 300}, {"d.jpg", 400}, {"a.jpg", 100}}
	results, err := f.svc.RequestUploadURLs(ctx, f.tenantID, f.eventID, album.ID, f.otherID, entries)
	if err != nil {
		t.Fatalf("upload urls: %v", err)
	}
	if results[4].Skipped != "duplicate" {
		t.Fatalf("the repeated file should be skipped, got %+v", results[4])
	}
	for i := 0; i < 4; i++ {
		if results[i].StorageID == "" || results[i].UploadURL == "" {
			t.Fatalf("file %d should get an upload URL, got %+v", i, results[i])
		}
	}
	if _, err := f.svc.RequestUploadURLs(ctx, f.tenantID, f.eventID, album.ID, f.otherID, []UploadEntry{{"x.cr3", 10}}); err == nil {
		t.Fatal("an unsupported format must be refused")
	}

	// A retry before the upload was completed reuses the same object.
	again, err := f.svc.RequestUploadURLs(ctx, f.tenantID, f.eventID, album.ID, f.otherID, entries[:1])
	if err != nil || again[0].StorageID != results[0].StorageID {
		t.Fatalf("retry should reuse the object: %v %+v vs %+v", err, again, results[0])
	}

	// ---- complete ----
	for i, n := range names {
		f.put(t, album.ID, n, entries[i].OriginalSize, "bytes-"+n)
	}
	items := make([]CompleteItem, 0, 6)
	for i, n := range names {
		items = append(items, CompleteItem{StorageID: results[i].StorageID, OriginalFilename: n, OriginalSize: entries[i].OriginalSize, Width: 2048, Height: 1365})
	}
	items = append(items, CompleteItem{StorageID: security.MustNewUUIDv4(), OriginalFilename: "ghost.jpg", OriginalSize: 1})
	done, err := f.svc.CompleteUploads(ctx, f.tenantID, f.eventID, album.ID, f.otherID, items)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	photoIDs := map[string]string{}
	for i, n := range names {
		if done[i].Status != CompleteCreated || done[i].PhotoID == "" {
			t.Fatalf("%s should be created, got %+v", n, done[i])
		}
		photoIDs[n] = done[i].PhotoID
	}
	if done[4].Status != CompleteRejected {
		t.Fatalf("an unknown storage id must be rejected, got %+v", done[4])
	}
	repeat, err := f.svc.CompleteUploads(ctx, f.tenantID, f.eventID, album.ID, f.otherID, items[:1])
	if err != nil || repeat[0].Status != CompleteExists || repeat[0].PhotoID != photoIDs["a.jpg"] {
		t.Fatalf("completing twice should return the same photo: %v %+v", err, repeat)
	}
	if _, err := f.svc.CompleteUploads(ctx, f.tenantID, f.eventID, album.ID, f.otherID, []CompleteItem{{StorageID: "not-a-uuid"}}); err == nil {
		t.Fatal("a malformed storage id must be refused")
	}

	// A file that is now in the album is a duplicate.
	dup, err := f.svc.RequestUploadURLs(ctx, f.tenantID, f.eventID, album.ID, f.otherID, entries[:1])
	if err != nil || dup[0].Skipped != "duplicate" {
		t.Fatalf("an uploaded file should now be a duplicate: %v %+v", err, dup)
	}

	// ---- list ----
	page, err := f.svc.ListPhotos(ctx, f.tenantID, f.eventID, PhotoFilter{}, pagination.PageParams{})
	if err != nil || len(page.Items) != 4 {
		t.Fatalf("list: %v, %d photos", err, len(page.Items))
	}
	p := page.Items[0]
	if p.UploaderName != "Sinta Tester" || p.CreatedBy != f.otherID || p.OCRStatus != OCRPending || p.Width != 2048 {
		t.Fatalf("unexpected photo %+v", p)
	}
	preview, original, err := f.svc.PhotoURLs(&p)
	if err != nil || preview == "" || preview != original {
		t.Fatalf("without a thumbnail the preview is the original: %q %q %v", preview, original, err)
	}
	if n := countState(t, f, PhotoFilter{State: StatePreparing}); n != 4 {
		t.Fatalf("all 4 should be 'preparing', got %d", n)
	}

	// Keyset paging walks every photo exactly once.
	seen := map[string]bool{}
	cursor := ""
	for i := 0; i < 10; i++ {
		pg, err := f.svc.ListPhotos(ctx, f.tenantID, f.eventID, PhotoFilter{}, pagination.PageParams{Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range pg.Items {
			if seen[it.ID] {
				t.Fatalf("photo %s listed twice", it.ID)
			}
			seen[it.ID] = true
		}
		if pg.NextCursor == "" {
			break
		}
		cursor = pg.NextCursor
	}
	if len(seen) != 4 {
		t.Fatalf("paging saw %d photos, want 4", len(seen))
	}

	// ---- derived states, against §3.3's rules ----
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := f.super.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	tag := func(photo, bib, source string, conf any) {
		exec(`INSERT INTO photo_tags (id, tenant_id, photo_id, bib_string, bib_number, confidence_score, source)
		      VALUES ($1, $2, $3, $4, NULL, $5, $6)`, security.MustNewUUIDv4(), f.tenantID, photo, bib, conf, source)
	}
	// a: thumbnail ready, still pending  -> detecting
	// b: 1024 at 92% and 5001 at 58%     -> review
	// c: only a manual tag               -> verified
	// d: nothing read                    -> no_bib
	exec(`UPDATE photos SET thumbnail_storage_id = original_storage_id WHERE id = $1`, photoIDs["a.jpg"])
	exec(`UPDATE photos SET ocr_status = 'processed' WHERE id = ANY($1::uuid[])`, "{"+photoIDs["b.jpg"]+","+photoIDs["c.jpg"]+","+photoIDs["d.jpg"]+"}")
	tag(photoIDs["b.jpg"], "1024", "ocr", 0.92)
	tag(photoIDs["b.jpg"], "5001", "ocr", 0.58)
	tag(photoIDs["c.jpg"], "2210", "manual", nil)
	for state, want := range map[State]int{
		StatePreparing: 0, StateDetecting: 1, StateReview: 1, StateVerified: 1, StateAuto: 0, StateFailed: 0, StateNoBIB: 1,
	} {
		if got := countState(t, f, PhotoFilter{State: state}); got != want {
			t.Errorf("state %s: got %d photos, want %d", state, got, want)
		}
	}
	// A failed photo that gets a manual tag reads as verified, and one
	// with only confident OCR tags as auto.
	exec(`UPDATE photos SET ocr_status = 'failed' WHERE id = $1`, photoIDs["d.jpg"])
	if got := countState(t, f, PhotoFilter{State: StateFailed}); got != 1 {
		t.Errorf("failed: got %d, want 1", got)
	}
	tag(photoIDs["d.jpg"], "3000", "ocr", 0.9)
	if got := countState(t, f, PhotoFilter{State: StateAuto}); got != 1 {
		t.Errorf("auto: got %d, want 1", got)
	}

	// bib matches a tag exactly as typed; uploader narrows to a person.
	if got := countState(t, f, PhotoFilter{BIB: "5001"}); got != 1 {
		t.Errorf("bib 5001: got %d, want 1", got)
	}
	if got := countState(t, f, PhotoFilter{BIB: "50"}); got != 0 {
		t.Errorf("bib is an exact match, got %d for a prefix", got)
	}
	if got := countState(t, f, PhotoFilter{UploaderID: f.userID}); got != 0 {
		t.Errorf("uploader filter: got %d, want 0", got)
	}
	if got := countState(t, f, PhotoFilter{UploaderID: f.otherID}); got != 4 {
		t.Errorf("uploader filter: got %d, want 4", got)
	}
	tagged, _ := f.svc.ListPhotos(ctx, f.tenantID, f.eventID, PhotoFilter{BIB: "5001"}, pagination.PageParams{})
	if len(tagged.Items) == 1 && len(tagged.Items[0].Tags) != 2 {
		t.Errorf("a listed photo carries all its tags, got %d", len(tagged.Items[0].Tags))
	}

	// ---- albums: counts, rename, publish, delete ----
	albums, err := f.svc.ListAlbums(ctx, f.tenantID, f.eventID)
	if err != nil || len(albums) != 1 || albums[0].PhotoCount != 4 {
		t.Fatalf("album list: %v %+v", err, albums)
	}
	public := true
	upd, err := f.svc.UpdateAlbum(ctx, f.tenantID, f.eventID, album.ID, f.userID, rbac.RoleStaff, AlbumPatch{IsPublic: &public})
	if err != nil || !upd.IsPublic {
		t.Fatalf("publish: %v %+v", err, upd)
	}
	if err := f.svc.DeleteAlbum(ctx, f.tenantID, f.eventID, album.ID, f.userID, rbac.RoleStaff); err == nil {
		t.Fatal("staff must not delete an album (admin only)")
	}
	if err := f.svc.DeleteAlbum(ctx, f.tenantID, f.eventID, album.ID, f.userID, rbac.RoleAdmin); err != nil {
		t.Fatalf("delete album: %v", err)
	}
	if n := countState(t, f, PhotoFilter{}); n != 0 {
		t.Fatalf("deleting the album should delete its photos, %d left", n)
	}
}

func countState(t *testing.T, f *fixture, filter PhotoFilter) int {
	t.Helper()
	page, err := f.svc.ListPhotos(context.Background(), f.tenantID, f.eventID, filter, pagination.PageParams{Limit: 100})
	if err != nil {
		t.Fatalf("list %+v: %v", filter, err)
	}
	return len(page.Items)
}
