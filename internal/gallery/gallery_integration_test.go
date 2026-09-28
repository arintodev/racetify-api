//go:build integration

// Integration test of the gallery against a real Postgres. It creates a
// throwaway database next to the one in DATABASE_MIGRATOR_URL, applies every
// migration to it, and drops it afterwards, so it never touches dev data:
//
//	go test -tags=integration ./internal/gallery/... -v
package gallery

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/url"
	"strings"
	"testing"

	_ "github.com/lib/pq"

	"github.com/racetify/racetify-api/internal/audit"
	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/domain"
	"github.com/racetify/racetify-api/internal/jobqueue"
	"github.com/racetify/racetify-api/internal/platform/database"
	"github.com/racetify/racetify-api/internal/platform/objectstorage"
	"github.com/racetify/racetify-api/internal/platform/pagination"
	"github.com/racetify/racetify-api/internal/platform/rbac"
	"github.com/racetify/racetify-api/internal/platform/rediscli"
	"github.com/racetify/racetify-api/internal/security"
	"github.com/racetify/racetify-api/internal/storage"
	"github.com/racetify/racetify-api/internal/watermark"
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
	svc        *Service
	storage    *storage.Service
	store      objectstorage.ProxyDriver
	queue      *jobqueue.Queue
	watermarks *watermark.Service
	super      *sql.DB
	appDB      *database.DB
	adminDB    *database.DB
	tenantID   string
	eventID    string
	userID     string
	otherID    string
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

	// Redis backs jobqueue.Queue.Enqueue (CompleteUploads queues a
	// media.photo_process job). Like Postgres above, this is skipped
	// rather than failed when unreachable in this sandbox.
	redisClient, err := rediscli.New(rediscli.Config{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB})
	if err != nil {
		t.Skipf("redis not reachable: %v", err)
	}
	queue := jobqueue.NewQueue(appDB, jobqueue.NewRepository(appDB, adminDB), redisClient)

	// Seed through the migrator connection, which bypasses row-level security.
	super := migrator.DB
	watermarkRepo := watermark.NewRepository(appDB)
	f := &fixture{
		svc:     NewService(appDB, NewRepository(appDB), auditRepo, storageSvc, driver, cfg.Storage, queue, watermarkRepo),
		storage: storageSvc, store: driver.(objectstorage.ProxyDriver), queue: queue, super: super,
		watermarks: watermark.NewService(appDB, watermarkRepo, auditRepo, driver, cfg.Storage),
		appDB:      appDB, adminDB: adminDB,
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
	f.putBytes(t, albumID, name, size, []byte(body))
}

func (f *fixture) putBytes(t *testing.T, albumID, name string, size int64, body []byte) {
	t.Helper()
	key := objectKey(albumID, name, size)
	sum, n, err := f.store.Put(objectstorage.BucketPrivate, f.tenantID, key, body)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.storage.CompletePut(context.Background(), f.tenantID, objectstorage.BucketPrivate, key, sum, n); err != nil {
		t.Fatal(err)
	}
}

// smallJPEG makes a tiny real JPEG in memory (w x h, solid color), for tests
// that need the thumbnail job to actually decode/resize/re-encode
// something, rather than a fixture file on disk.
func smallJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode fixture jpeg: %v", err)
	}
	return buf.Bytes()
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
	done, jobID, err := f.svc.CompleteUploads(ctx, f.tenantID, f.eventID, album.ID, f.otherID, items)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if jobID == "" {
		t.Fatal("completing new photos should queue a media.photo_process job")
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
	repeat, repeatJobID, err := f.svc.CompleteUploads(ctx, f.tenantID, f.eventID, album.ID, f.otherID, items[:1])
	if err != nil || repeat[0].Status != CompleteExists || repeat[0].PhotoID != photoIDs["a.jpg"] {
		t.Fatalf("completing twice should return the same photo: %v %+v", err, repeat)
	}
	if repeatJobID != "" {
		t.Fatal("completing an already-completed upload should not queue a second job")
	}
	if _, _, err := f.svc.CompleteUploads(ctx, f.tenantID, f.eventID, album.ID, f.otherID, []CompleteItem{{StorageID: "not-a-uuid"}}); err == nil {
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

// uploadPhoto drives the full upload-urls -> PUT -> complete flow for one
// file and returns the photo it made, still ocr_status='pending'.
func (f *fixture) uploadPhoto(t *testing.T, albumID, name string, size int64) string {
	t.Helper()
	ctx := context.Background()
	results, err := f.svc.RequestUploadURLs(ctx, f.tenantID, f.eventID, albumID, f.otherID, []UploadEntry{{name, size}})
	if err != nil {
		t.Fatalf("upload urls: %v", err)
	}
	f.put(t, albumID, name, size, "bytes-"+name)
	done, _, err := f.svc.CompleteUploads(ctx, f.tenantID, f.eventID, albumID, f.otherID,
		[]CompleteItem{{StorageID: results[0].StorageID, OriginalFilename: name, OriginalSize: size, Width: 800, Height: 600}})
	if err != nil || done[0].Status != CompleteCreated {
		t.Fatalf("complete: %v %+v", err, done)
	}
	return done[0].PhotoID
}

func TestGalleryTags(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	album, err := f.svc.CreateAlbum(ctx, f.tenantID, f.eventID, f.userID, rbac.RoleStaff, AlbumInput{Name: "Finish Line"})
	if err != nil {
		t.Fatalf("create album: %v", err)
	}
	photoID := f.uploadPhoto(t, album.ID, "a.jpg", 100)
	if got := countState(t, f, PhotoFilter{State: StatePreparing}); got != 1 {
		t.Fatalf("new photo should be preparing, got %d", got)
	}

	// ---- add: a manual tag on a pending photo settles it to processed ----
	p, err := f.svc.AddTag(ctx, f.tenantID, f.eventID, photoID, f.userID, " 1024 ")
	if err != nil {
		t.Fatalf("add tag: %v", err)
	}
	if p.OCRStatus != OCRProcessed {
		t.Fatalf("a manually tagged pending photo should settle to processed, got %q", p.OCRStatus)
	}
	if len(p.Tags) != 1 || p.Tags[0].BIB != "1024" || p.Tags[0].Source != TagSourceManual || p.Tags[0].Confidence != nil {
		t.Fatalf("unexpected tag %+v", p.Tags)
	}
	tagID := p.Tags[0].ID
	if got := countState(t, f, PhotoFilter{State: StateVerified}); got != 1 {
		t.Fatalf("a photo with only a manual tag should read as verified, got %d", got)
	}

	// A duplicate bib on the same photo is a conflict, not a second tag.
	if _, err := f.svc.AddTag(ctx, f.tenantID, f.eventID, photoID, f.userID, "1024"); err == nil {
		t.Fatal("adding the same bib twice must conflict")
	} else {
		var gerr *Error
		if !errors.As(err, &gerr) || gerr.Code != "tag_bib_exists" {
			t.Fatalf("want tag_bib_exists, got %v", err)
		}
	}
	if _, err := f.svc.AddTag(ctx, f.tenantID, f.eventID, photoID, f.userID, "   "); err == nil {
		t.Fatal("a blank bib must be refused")
	}

	// ---- update: seed a low-confidence OCR tag and confirm/correct it ----
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := f.super.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	ocrTagID := security.MustNewUUIDv4()
	exec(`INSERT INTO photo_tags (id, tenant_id, photo_id, bib_string, bib_number, confidence_score, source)
	      VALUES ($1, $2, $3, '5001', 5001, 0.6, 'ocr')`, ocrTagID, f.tenantID, photoID)

	// Confirm: same bib, becomes manual, loses its confidence.
	p, err = f.svc.UpdateTag(ctx, f.tenantID, f.eventID, photoID, ocrTagID, f.userID, "5001")
	if err != nil {
		t.Fatalf("confirm tag: %v", err)
	}
	var confirmed *Tag
	for i := range p.Tags {
		if p.Tags[i].ID == ocrTagID {
			confirmed = &p.Tags[i]
		}
	}
	if confirmed == nil || confirmed.Source != TagSourceManual || confirmed.Confidence != nil || confirmed.BIB != "5001" {
		t.Fatalf("confirming should keep the bib and drop the confidence, got %+v", confirmed)
	}

	// Correct: change the bib of the tag we just added.
	p, err = f.svc.UpdateTag(ctx, f.tenantID, f.eventID, photoID, tagID, f.userID, "1099")
	if err != nil {
		t.Fatalf("correct tag: %v", err)
	}
	found := false
	for _, tg := range p.Tags {
		if tg.ID == tagID {
			found = tg.BIB == "1099"
		}
	}
	if !found {
		t.Fatal("correcting a tag should change its bib")
	}

	// Correcting a tag onto a bib the photo already has is a conflict.
	if _, err := f.svc.UpdateTag(ctx, f.tenantID, f.eventID, photoID, tagID, f.userID, "5001"); err == nil {
		t.Fatal("correcting onto an existing bib must conflict")
	}
	if _, err := f.svc.UpdateTag(ctx, f.tenantID, f.eventID, photoID, "not-a-uuid", f.userID, "1"); err == nil {
		t.Fatal("a malformed tag id must be rejected")
	}

	// ---- delete ----
	p, err = f.svc.DeleteTag(ctx, f.tenantID, f.eventID, photoID, tagID, f.userID)
	if err != nil {
		t.Fatalf("delete tag: %v", err)
	}
	for _, tg := range p.Tags {
		if tg.ID == tagID {
			t.Fatal("the deleted tag should be gone")
		}
	}
	if _, err := f.svc.DeleteTag(ctx, f.tenantID, f.eventID, photoID, tagID, f.userID); err == nil {
		t.Fatal("deleting an already-deleted tag must 404")
	}
}

func TestGalleryBulkActions(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	a, err := f.svc.CreateAlbum(ctx, f.tenantID, f.eventID, f.userID, rbac.RoleStaff, AlbumInput{Name: "Album A"})
	if err != nil {
		t.Fatalf("create album a: %v", err)
	}
	b, err := f.svc.CreateAlbum(ctx, f.tenantID, f.eventID, f.userID, rbac.RoleStaff, AlbumInput{Name: "Album B"})
	if err != nil {
		t.Fatalf("create album b: %v", err)
	}
	otherEventID := security.MustNewUUIDv4()
	if _, err := f.super.ExecContext(ctx, `INSERT INTO events (id, tenant_id, name, slug, created_by) VALUES ($1, $2, 'E2', 'e2', $3)`,
		otherEventID, f.tenantID, f.userID); err != nil {
		t.Fatalf("seed other event: %v", err)
	}
	foreignAlbum, err := f.svc.CreateAlbum(ctx, f.tenantID, otherEventID, f.userID, rbac.RoleStaff, AlbumInput{Name: "Foreign"})
	if err != nil {
		t.Fatalf("create foreign album: %v", err)
	}

	p1 := f.uploadPhoto(t, a.ID, "1.jpg", 100)
	p2 := f.uploadPhoto(t, a.ID, "2.jpg", 200)
	p3 := f.uploadPhoto(t, a.ID, "3.jpg", 300)

	// Moving into an album of another event is rejected outright.
	if _, err := f.svc.BulkMovePhotos(ctx, f.tenantID, f.eventID, f.userID, []string{p1, p2}, foreignAlbum.ID); err == nil {
		t.Fatal("moving into another event's album must be refused")
	}

	moved, err := f.svc.BulkMovePhotos(ctx, f.tenantID, f.eventID, f.userID, []string{p1, p2}, b.ID)
	if err != nil {
		t.Fatalf("bulk move: %v", err)
	}
	if moved != 2 {
		t.Fatalf("want 2 moved, got %d", moved)
	}
	if got := countState(t, f, PhotoFilter{AlbumID: b.ID}); got != 2 {
		t.Fatalf("album b should now have 2 photos, got %d", got)
	}
	if got := countState(t, f, PhotoFilter{AlbumID: a.ID}); got != 1 {
		t.Fatalf("album a should have 1 photo left, got %d", got)
	}

	// A stray id from outside the event moves nothing extra for it.
	moved, err = f.svc.BulkMovePhotos(ctx, f.tenantID, f.eventID, f.userID, []string{p3, security.MustNewUUIDv4()}, b.ID)
	if err != nil || moved != 1 {
		t.Fatalf("bulk move with one unknown id: %v moved=%d", err, moved)
	}

	if _, err := f.svc.BulkMovePhotos(ctx, f.tenantID, f.eventID, f.userID, nil, b.ID); err == nil {
		t.Fatal("an empty selection must be refused")
	}

	deleted, err := f.svc.BulkDeletePhotos(ctx, f.tenantID, f.eventID, f.userID, []string{p1, p2, p3})
	if err != nil {
		t.Fatalf("bulk delete: %v", err)
	}
	if deleted != 3 {
		t.Fatalf("want 3 deleted, got %d", deleted)
	}
	if got := countState(t, f, PhotoFilter{}); got != 0 {
		t.Fatalf("all photos should be gone, got %d", got)
	}

	// A batch over the cap is refused up front.
	tooMany := make([]string, MaxBatch+1)
	for i := range tooMany {
		tooMany[i] = security.MustNewUUIDv4()
	}
	if _, err := f.svc.BulkDeletePhotos(ctx, f.tenantID, f.eventID, f.userID, tooMany); err == nil {
		t.Fatal("a batch over MaxBatch must be refused")
	}
}

// TestGalleryJobStatusStub locks in GET /api/v1/jobs/{id}'s behaviour for a
// gallery job id that does not exist. Real media.photo_process jobs exist
// now (TestGalleryThumbnailJob below), but any id a client makes up, or any
// tenant/event this test's fixture didn't create, must still 404 rather than
// fabricate progress.
func TestGalleryJobStatusStub(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	queue := jobqueue.NewQueue(f.appDB, jobqueue.NewRepository(f.appDB, f.adminDB), nil)
	if _, err := queue.GetJob(ctx, f.tenantID, security.MustNewUUIDv4()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an unknown job id should 404, got %v", err)
	}
}

// TestGalleryThumbnailJob drives complete -> the real media.photo_process
// job (invoked directly, not through the queue/worker's dequeue loop, so
// the test is deterministic) -> asserts thumbnail_storage_id gets set and
// preview_url stops matching original_url.
func TestGalleryThumbnailJob(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	album, err := f.svc.CreateAlbum(ctx, f.tenantID, f.eventID, f.userID, rbac.RoleStaff, AlbumInput{Name: "Finish Line"})
	if err != nil {
		t.Fatalf("create album: %v", err)
	}

	// A real, decodable JPEG bigger than ThumbnailMaxEdge on its long edge,
	// so the job actually has to resize it (not just pass it through).
	body := smallJPEG(t, 900, 600)
	urls, err := f.svc.RequestUploadURLs(ctx, f.tenantID, f.eventID, album.ID, f.otherID,
		[]UploadEntry{{Filename: "runner.jpg", OriginalSize: int64(len(body))}})
	if err != nil {
		t.Fatalf("upload urls: %v", err)
	}
	f.putBytes(t, album.ID, "runner.jpg", int64(len(body)), body)

	// A second file the job must skip gracefully: HEIC/HEIF has no decoder
	// here (thumbnail_job.go's heicExtensions), so it stays without a
	// thumbnail without failing the batch.
	heicBody := []byte("not a real heic file, decoding is never attempted")
	heicURLs, err := f.svc.RequestUploadURLs(ctx, f.tenantID, f.eventID, album.ID, f.otherID,
		[]UploadEntry{{Filename: "runner.heic", OriginalSize: int64(len(heicBody))}})
	if err != nil {
		t.Fatalf("upload urls (heic): %v", err)
	}
	f.putBytes(t, album.ID, "runner.heic", int64(len(heicBody)), heicBody)

	done, jobID, err := f.svc.CompleteUploads(ctx, f.tenantID, f.eventID, album.ID, f.otherID, []CompleteItem{
		{StorageID: urls[0].StorageID, OriginalFilename: "runner.jpg", OriginalSize: int64(len(body)), Width: 900, Height: 600},
		{StorageID: heicURLs[0].StorageID, OriginalFilename: "runner.heic", OriginalSize: int64(len(heicBody)), Width: 900, Height: 600},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if jobID == "" {
		t.Fatal("expected a queued media.photo_process job id")
	}
	photoID, heicPhotoID := done[0].PhotoID, done[1].PhotoID

	before, err := f.svc.repo.GetPhotoByID(ctx, f.tenantID, photoID)
	if err != nil {
		t.Fatalf("get photo before job: %v", err)
	}
	if before.HasThumbnail() {
		t.Fatal("a photo should have no thumbnail before the job runs")
	}
	previewBefore, originalBefore, err := f.svc.PhotoURLs(before)
	if err != nil || previewBefore != originalBefore {
		t.Fatalf("before the job, preview should equal original: %v %q vs %q", err, previewBefore, originalBefore)
	}

	// Run the job handler directly - the point of this test is the
	// handler's own logic, not the redis dequeue loop.
	job, err := f.queue.GetJob(ctx, f.tenantID, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	result, partialErr, err := f.svc.RunPhotoProcess(ctx, job)
	if err != nil {
		t.Fatalf("run job: %v", err)
	}
	if partialErr != "" {
		t.Fatalf("job should fully succeed (heic is a graceful skip, not a failure): %q", partialErr)
	}
	if result["made"] != 1 || result["skipped"] != 1 || result["failed"] != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}

	after, err := f.svc.repo.GetPhotoByID(ctx, f.tenantID, photoID)
	if err != nil {
		t.Fatalf("get photo after job: %v", err)
	}
	if !after.HasThumbnail() {
		t.Fatal("thumbnail_storage_id should be set after the job runs")
	}
	previewAfter, originalAfter, err := f.svc.PhotoURLs(after)
	if err != nil {
		t.Fatalf("photo urls after job: %v", err)
	}
	if previewAfter == originalAfter {
		t.Fatal("preview_url should now differ from original_url")
	}

	heicPhoto, err := f.svc.repo.GetPhotoByID(ctx, f.tenantID, heicPhotoID)
	if err != nil {
		t.Fatalf("get heic photo: %v", err)
	}
	if heicPhoto.HasThumbnail() {
		t.Fatal("a HEIC photo should be skipped, not thumbnailed")
	}

	// Running the job again for the same photos is a no-op (both already
	// have a thumbnail, or are HEIC), not a second thumbnail or an error.
	result, partialErr, err = f.svc.RunPhotoProcess(ctx, job)
	if err != nil || partialErr != "" {
		t.Fatalf("re-running the job should be harmless: %v %q", err, partialErr)
	}
	if result["skipped"] != 2 || result["made"] != 0 {
		t.Fatalf("a re-run should skip everything: %+v", result)
	}
}

// TestGalleryThumbnailWatermarkCompositing configures one watermark layer
// for the event, anchored at the bottom-right corner with a zero offset (so
// its bottom-right pixel lands exactly on the thumbnail's own bottom-right
// pixel regardless of the exact size the width_percent/aspect_ratio math
// works out to), runs the real media.photo_process job, and asserts that
// corner pixel of the resulting thumbnail differs from what a plain resize
// (resizeToMaxEdge alone, no compositing) would have produced there -
// proving applyWatermarks actually ran as part of the job.
func TestGalleryThumbnailWatermarkCompositing(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	album, err := f.svc.CreateAlbum(ctx, f.tenantID, f.eventID, f.userID, rbac.RoleStaff, AlbumInput{Name: "Finish Line"})
	if err != nil {
		t.Fatalf("create album: %v", err)
	}

	// A solid, distinctly-colored watermark source image - a real decodable
	// JPEG, since applyWatermarks (via loadWatermarks) decodes it the same
	// way the photo's own original is decoded.
	wmBody := smallJPEG(t, 200, 200)
	wmKey := "watermarks/logo.jpg"
	wmTicket, err := f.storage.ProvisionUpload(ctx, f.tenantID, f.userID, "public", wmKey, "image/jpeg")
	if err != nil {
		t.Fatalf("provision watermark upload: %v", err)
	}
	sum, n, err := f.store.Put(objectstorage.BucketPublic, f.tenantID, wmKey, wmBody)
	if err != nil {
		t.Fatalf("put watermark: %v", err)
	}
	if err := f.storage.CompletePut(ctx, f.tenantID, objectstorage.BucketPublic, wmKey, sum, n); err != nil {
		t.Fatalf("complete watermark put: %v", err)
	}
	if _, err := f.watermarks.ReplaceAll(ctx, f.tenantID, f.eventID, f.userID, rbac.RoleAdmin, []watermark.ReplaceItem{{
		StorageID: wmTicket.ObjectID, Name: "Logo", AnchorX: "right", AnchorY: "bottom",
		OffsetXPercent: 0, OffsetYPercent: 0, WidthPercent: 0.5, AspectRatio: 1, Opacity: 1, SortOrder: 0,
	}}); err != nil {
		t.Fatalf("configure watermark: %v", err)
	}

	body := smallJPEG(t, 900, 600)
	urls, err := f.svc.RequestUploadURLs(ctx, f.tenantID, f.eventID, album.ID, f.otherID,
		[]UploadEntry{{Filename: "runner.jpg", OriginalSize: int64(len(body))}})
	if err != nil {
		t.Fatalf("upload urls: %v", err)
	}
	f.putBytes(t, album.ID, "runner.jpg", int64(len(body)), body)
	done, jobID, err := f.svc.CompleteUploads(ctx, f.tenantID, f.eventID, album.ID, f.otherID, []CompleteItem{
		{StorageID: urls[0].StorageID, OriginalFilename: "runner.jpg", OriginalSize: int64(len(body)), Width: 900, Height: 600},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if jobID == "" {
		t.Fatal("expected a queued media.photo_process job id")
	}
	photoID := done[0].PhotoID

	job, err := f.queue.GetJob(ctx, f.tenantID, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if _, partialErr, err := f.svc.RunPhotoProcess(ctx, job); err != nil || partialErr != "" {
		t.Fatalf("run job: %v %q", err, partialErr)
	}

	// What a plain resize (no watermark) would have produced.
	srcImg, _, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("decode original: %v", err)
	}
	plainThumb := resizeToMaxEdge(srcImg, ThumbnailMaxEdge)
	pb := plainThumb.Bounds()
	px, py := pb.Max.X-1, pb.Max.Y-1
	plainR, plainG, plainB, _ := plainThumb.At(px, py).RGBA()

	// What the job actually stored.
	var thumbStorageID string
	if err := f.super.QueryRowContext(ctx, `SELECT thumbnail_storage_id FROM photos WHERE id = $1`, photoID).Scan(&thumbStorageID); err != nil {
		t.Fatalf("query thumbnail storage id: %v", err)
	}
	thumbData, err := f.storage.ReadObject(ctx, f.tenantID, thumbStorageID)
	if err != nil {
		t.Fatalf("read thumbnail: %v", err)
	}
	actualThumb, _, err := image.Decode(bytes.NewReader(thumbData))
	if err != nil {
		t.Fatalf("decode thumbnail: %v", err)
	}
	ab := actualThumb.Bounds()
	if ab.Dx() != pb.Dx() || ab.Dy() != pb.Dy() {
		t.Fatalf("watermarking should not change the thumbnail's own dimensions: plain %v, actual %v", pb, ab)
	}
	ax, ay := ab.Max.X-1, ab.Max.Y-1
	actualR, actualG, actualB, _ := actualThumb.At(ax, ay).RGBA()

	if actualR == plainR && actualG == plainG && actualB == plainB {
		t.Fatalf("the bottom-right corner pixel should differ once the watermark is composited: plain (%d,%d,%d), actual (%d,%d,%d)",
			plainR, plainG, plainB, actualR, actualG, actualB)
	}
}
