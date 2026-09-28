# Media Gallery API: kontrak dan keputusan

Melengkapi `phase1-api-plan.md` §4.4 dan `racetify-app/docs/media-gallery-ux.md`.
Ini yang benar-benar dibangun; bagian yang belum dikerjakan ada di bawah.

## Route

Semua route berada di bawah event (`/api/v1/events/{id}/...`), bukan `/albums/{aid}`
seperti di plan §4.4. Alasannya: `RequireEventAccess` menentukan tenant dari `{id}`
event, sehingga crew tanpa keanggotaan tenant tetap bisa masuk (pola yang sama
dengan modul peserta).

| Route | Akses |
|---|---|
| `GET /events/{id}/albums` | Staff+, atau crew dengan `gallery:upload` / `gallery:review` |
| `POST /events/{id}/albums` | Staff+ |
| `PATCH /events/{id}/albums/{aid}` | Staff+ (nama, deskripsi, `is_public`) |
| `DELETE /events/{id}/albums/{aid}` | Admin+ (foto ikut terhapus) |
| `POST /events/{id}/albums/{aid}/photos/upload-urls` | Staff+, atau crew `gallery:upload` |
| `POST /events/{id}/albums/{aid}/photos/complete` | Staff+, atau crew `gallery:upload` |
| `GET /events/{id}/photos` | Staff+, atau crew `gallery:upload` / `gallery:review` |
| `POST /events/{id}/photos/{pid}/tags` | Staff+, atau crew `gallery:review` |
| `PATCH /events/{id}/photos/{pid}/tags/{tid}` | Staff+, atau crew `gallery:review` |
| `DELETE /events/{id}/photos/{pid}/tags/{tid}` | Staff+, atau crew `gallery:review` |
| `POST /events/{id}/photos/bulk-move` | Staff+, atau crew `gallery:review` |
| `POST /events/{id}/photos/bulk-delete` | Staff+, atau crew `gallery:review` |

Crew yang hanya punya `gallery:upload` selalu melihat foto unggahannya sendiri;
server memaksa filter `uploader_id`, apa pun query-nya.

## Alur unggah

1. `upload-urls` — body `{files: [{filename, original_size}]}` (maks. 50).
   Balasan `{items: [{filename, storage_id, upload_url, expires_at}]}` dalam urutan
   yang sama; file yang sudah ada di album diganti `skipped: "duplicate"`.
   Duplikat = nama + ukuran file asli di album yang sama.
2. Browser mem-PUT ke `upload_url`.
3. `complete` — body `{items: [{storage_id, original_filename, original_size, width, height}]}`
   (maks. 50). Balasan `201 {items: [{storage_id, photo_id, status, reason}], job_id}`,
   `status` = `created` | `exists` | `rejected`. Memanggil dua kali aman (`exists`).
   `job_id` adalah satu job `media.photo_process` (lihat "Job thumbnail" di bawah)
   yang mencakup semua foto yang baru dibuat di panggilan ini; `null` bila tidak
   ada foto baru (semua `exists`/`rejected`), atau bila enqueue gagal setelah
   foto-nya sendiri sudah tersimpan aman (lihat keputusan di "Job thumbnail").

Kunci objek dibuat deterministik dari album + nama + ukuran file
(`photos/{albumId}/{hash}.{ext}`), jadi meminta URL lagi setelah gagal memakai ulang
objek yang sama, tidak menumpuk objek yatim.

Endpoint `storage/objects/upload-url` bawaan hanya untuk Admin+, sehingga gallery
memakai `storage.Service.ProvisionUpload` dan `ConfirmStored` (tanpa cek role;
otorisasi ada di gate route). `ConfirmStored` melakukan HEAD ke bucket untuk driver r2.

Batas: file > 20 MB setelah resize ditolak saat `complete` (`rejected`).
Driver `local` membatasi body PUT 25 MiB.

## Daftar foto

`GET /events/{id}/photos?album_id=&state=&bib=&uploader_id=&mine=&limit=&cursor=`
Keyset, terbaru dulu. `state` adalah salah satu dari 7 status turunan §3.3
(`preparing`, `detecting`, `review`, `verified`, `auto`, `failed`, `no_bib`).
`bib` mencocokkan tag apa adanya (tanpa normalisasi). Tiap foto membawa `tags`,
`preview_url`, dan `original_url`.

`preview_url` adalah thumbnail bila sudah ada (`thumbnail_storage_id` terisi);
sebelum job-nya selesai (atau untuk foto HEIC/HEIF yang di-skip, lihat di bawah),
isinya sama dengan `original_url` (file 2048 px, presigned) supaya grid tetap
menampilkan sesuatu. Thumbnail belum ber-watermark — itu masih di daftar
"belum dikerjakan".

## Tag: tambah, konfirmasi/koreksi, hapus

- `POST /events/{id}/photos/{pid}/tags` — body `{bib: string}`. Menambah tag
  manual. `bib` di-trim, wajib diisi, maks 20 karakter. BIB yang sudah ada di
  foto yang sama (`photo_tags_photo_bib_uk`, migrations/0016) ditolak
  `409 tag_bib_exists` — baik itu tag manual lain maupun bacaan OCR yang
  kebetulan sama. Balasan `201` berisi `PhotoDTO` terbaru (bukan cuma tag-nya),
  supaya klien langsung dapat status turunan yang baru tanpa fetch ulang.
- `PATCH /events/{id}/photos/{pid}/tags/{tid}` — body `{bib: string}`. Satu
  endpoint ini melayani dua aksi UI: **konfirmasi** (bib dikirim balik sama
  persis) dan **koreksi** (bib diganti) — keduanya membuat tag menjadi
  `manual` dan menghapus `confidence_score`-nya, sama seperti
  `confirmTag`/`updateTag` di `gallery-store.ts`. Box (lokasi hasil OCR) tetap
  dipertahankan supaya inspector masih bisa menunjuk ke mana tag itu berasal.
  Balasan `200` dengan `PhotoDTO` terbaru.
- `DELETE /events/{id}/photos/{pid}/tags/{tid}` — hapus satu tag. Balasan
  `200` dengan `PhotoDTO` terbaru (bukan `204`, beda dari `DeleteAlbum`,
  supaya klien dapat tag list yang sudah bersih dalam satu response).
- Tag manual pada foto yang masih `pending` (lewat add atau confirm/correct,
  bukan delete) mengubah `ocr_status` foto itu menjadi `processed`
  (`Service.settlePending`, `tag_service.go`), supaya foto yang sudah ditinjau
  manusia tampil Terverifikasi dan tidak ditimpa job OCR nanti.
- Tabel `photo_tags` sudah ada sejak migrations/0009 + 0016 (kolom box,
  `source` check constraint, dan unique index per-bib-per-foto sudah lengkap);
  tidak ada migrasi baru untuk fitur ini.

## Pindah dan hapus massal

- `POST /events/{id}/photos/bulk-move` — body `{ids: string[], album_id: string}`.
  Bukan `filter`-based seperti `internal/participant`'s bulk endpoints — grid
  galeri memilih foto satu per satu di satu halaman, jadi daftar id polos
  sudah cukup. Album tujuan divalidasi ada di event yang sama (`GetAlbum`);
  album dari event lain (atau tidak ada) ditolak `404`. Balasan
  `{moved: number}` — id yang bukan milik event ini (atau sudah tidak ada)
  ikut terhitung sebagai tidak berpindah, bukan error, sama seperti perilaku
  update/delete massal `internal/participant`.
- `POST /events/{id}/photos/bulk-delete` — body `{ids: string[]}`, hard
  delete (tag ikut terhapus, `ON DELETE CASCADE`). Balasan
  `{deleted: number}`.
- Maksimum 50 id per request (`MaxBatch`, konstanta yang sama dengan
  `upload-urls`/`complete`).
- Objek storage foto yang dihapus **tidak** dibersihkan di sini — sama seperti
  `DeleteAlbum`, itu domain terpisah, masuk daftar "belum dikerjakan" di
  bawah.

## Job thumbnail (`media.photo_process`)

**Ruang lingkup: hanya thumbnail.** Job ini *hanya* membuat thumbnail. Ia
tidak menambahkan watermark dan tidak melakukan OCR/deteksi BIB — keduanya
tetap di daftar "belum dikerjakan" di bawah, sudah diberi tahu ke user dan
sengaja ditunda (keputusan vendor/produk tersendiri, di luar cakupan kerja
ini).

`CompleteUploads` (`photo_service.go`) meng-enqueue satu job
`media.photo_process` per panggilan `complete`, mencakup setiap foto yang
baru dibuat (bukan satu job per foto — sama seperti pola batch
`certificates.batch`/`generator.bib_batch`). Payload-nya (`thumbnail_job.go`):

```json
{"event_id": "...", "album_id": "...", "photo_ids": ["...", "..."]}
```

Tidak ada data pribadi di payload, sama seperti job batch lain di codebase
ini. Handler-nya (`Service.RunPhotoProcess`, didaftarkan lewat
`gallery.RegisterJob` di `cmd/worker/main.go`) memproses tiap `photo_id`:

1. Ambil baris foto (`Repository.GetPhotoByID` — tenant+id saja, tidak perlu
   event/album seperti `GetPhoto` yang dipakai endpoint HTTP).
2. Kalau `thumbnail_storage_id` sudah terisi (job diulang), atau ekstensinya
   `.heic`/`.heif`, foto itu di-skip — bukan gagal. **HEIC/HEIF tidak
   didekode**: pustaka standar Go tidak punya decoder-nya, dan menambah
   `libheif` (cgo) atau decoder pure-Go besar adalah keputusan dependency
   tersendiri yang tidak diambil dalam kerja ini. `preview_url` foto itu
   tetap sama dengan `original_url` selamanya sampai ada keputusan lain.
3. Unduh original lewat `storage.Service.ReadObject` (perlu driver storage
   yang proxy bytes-nya sendiri — "local"; dengan driver "r2" job ini gagal
   dengan pesan jelas, sama seperti keterbatasan `ReadObject`/`StoreGenerated`
   yang sudah ada untuk `bibprint`/`certificate`).
4. Decode (JPEG/PNG lewat `image`/`image/jpeg`/`image/png` stdlib), resize
   dengan `golang.org/x/image/draw` (`draw.CatmullRom`, resampler kualitas
   tinggi — job ini jalan sekali per foto di luar request path, jadi biaya
   ekstranya wajar), sisi terpanjang dibatasi **640 px** (`ThumbnailMaxEdge`).
   640 dipilih (bukan 480) supaya masih tajam di layar 2x/3x untuk kotak
   grid ~200-300 px CSS; original sendiri sudah di-resize klien ke 2048 px
   (`RESIZE_MAX_EDGE` di `gallery.ts`), jadi thumbnail grid tidak perlu
   sebesar itu. Foto yang kedua sisinya sudah ≤ 640 px tidak di-resize ulang.
5. Encode ulang sebagai JPEG, quality **82** (`ThumbnailJPEGQuality`) — client
   me-resize original dengan quality 90 (`RESIZE_QUALITY`); thumbnail dilihat
   kecil di grid, jadi kualitas sedikit lebih rendah tidak kelihatan dan
   memperkecil ukuran total payload grid.
6. Simpan bytes-nya lewat `storage.Service.StoreGenerated` (bucket private,
   sama seperti original — belum ada keputusan bucket publik untuk thumbnail;
   itu bagian dari kerja watermark/publikasi yang ditunda), kuncinya
   `photos/{albumId}/thumb/{photoId}.jpg`.
7. `Repository.UpdatePhotoThumbnail` mengisi `thumbnail_storage_id`, sehingga
   `PhotoURLs`/`preview_url` langsung memakai thumbnail di request berikutnya.

Satu foto gagal (file korup, format tak didukung selain HEIC/HEIF) dicatat
sebagai gagal dan tidak menghentikan foto lain dalam batch yang sama —
`partialErr` job membawa ringkasannya, job tetap `completed_with_errors`,
bukan `failed` total (pola yang sama dengan `certificate.RunBatch`).

Tidak ada endpoint job-status baru dibuat khusus untuk gallery: klien memoll
`job_id` dari `complete` lewat endpoint generik yang sudah ada,
`GET /api/v1/jobs/{id}` (`internal/jobqueue`, dipakai bersama oleh CSV import
dan cetak BIB/sertifikat). Endpoint itu membalas `404 not_found` untuk id
yang tidak punya baris `jobs` — termasuk gallery, sama seperti modul lain.

## Keputusan

- `photos.created_by` adalah fotografer; tidak ada kolom `uploaded_by`.
- Album baru selalu draft. Nama album unik per event (tidak peka huruf besar/kecil).
- Aturan status turunan ada di dua tempat yang harus sama: `stateCondition` di
  `photo_repository.go` (filter SQL) dan `photoState()` di
  `racetify-app/apps/dashboard/src/lib/gallery.ts`.
- Tag manual pada foto yang masih `pending` mengubah `ocr_status` menjadi
  `processed` — lihat "Tag: tambah, konfirmasi/koreksi, hapus" di atas
  (sudah diterapkan).

## Belum dikerjakan

- **Watermark** pada thumbnail atau original — belum dibangun sama sekali,
  ditunda dengan sengaja (keputusan produk/vendor tersendiri, sudah
  disampaikan ke user). Thumbnail yang ada sekarang polos, tanpa watermark.
- **OCR/deteksi BIB beneran** dan `re-run-ocr` — juga belum dibangun.
  `ocr_status` tetap `pending` sampai fitur ini ada; job `media.photo_process`
  yang ada sekarang **tidak** menyentuh OCR sama sekali, hanya thumbnail
  (lihat "Job thumbnail" di atas).
- Ringkasan per status, cleanup objek `pending` > 24 jam.
- Thumbnail untuk foto HEIC/HEIF (tidak ada decoder Go tanpa dependency besar
  baru — lihat "Job thumbnail"); foto itu tetap memakai original sebagai
  preview-nya selamanya sampai ada keputusan dependency yang terpisah.

## Pengujian

`internal/gallery/gallery_integration_test.go` (tag `integration`) membuat database
sementara, menjalankan semua migrasi, dan menguji alur di atas dengan storage lokal:
`TestGalleryUploadFlow` (upload sampai status turunan), `TestGalleryTags`
(tambah/konfirmasi/koreksi/hapus tag + flip `ocr_status`), `TestGalleryBulkActions`
(pindah dan hapus massal, termasuk validasi album tujuan satu event),
`TestGalleryJobStatusStub` (endpoint generik `/api/v1/jobs/{id}` membalas 404
untuk id yang tidak ada), dan `TestGalleryThumbnailJob` (upload foto JPEG asli,
`complete` meng-enqueue job, job dijalankan langsung lewat
`Service.RunPhotoProcess` — bukan lewat redis dequeue loop, supaya
deterministik — lalu memastikan `thumbnail_storage_id` terisi dan
`preview_url` berbeda dari `original_url`; sekaligus memastikan foto HEIC
di-skip tanpa gagal dan menjalankan job dua kali aman/idempoten).

    go test -tags=integration ./internal/gallery/... -v

Butuh `DATABASE_MIGRATOR_URL` dengan hak membuat database, dan Redis (job
`complete` meng-enqueue lewat `jobqueue.Queue`, yang push ke Redis). Tanpa
salah satunya, test di-skip.
