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

Crew yang hanya punya `gallery:upload` selalu melihat foto unggahannya sendiri;
server memaksa filter `uploader_id`, apa pun query-nya.

## Alur unggah

1. `upload-urls` — body `{files: [{filename, original_size}]}` (maks. 50).
   Balasan `{items: [{filename, storage_id, upload_url, expires_at}]}` dalam urutan
   yang sama; file yang sudah ada di album diganti `skipped: "duplicate"`.
   Duplikat = nama + ukuran file asli di album yang sama.
2. Browser mem-PUT ke `upload_url`.
3. `complete` — body `{items: [{storage_id, original_filename, original_size, width, height}]}`
   (maks. 50). Balasan `201 {items: [{storage_id, photo_id, status, reason}], job_id: null}`,
   `status` = `created` | `exists` | `rejected`. Memanggil dua kali aman (`exists`).

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

`preview_url` adalah thumbnail bila sudah ada; **sebelum job thumbnail ada, isinya
sama dengan `original_url`** (file 2048 px, presigned) supaya grid langsung menampilkan
foto. Tanpa watermark sampai job thumbnail dibuat.

## Keputusan

- `photos.created_by` adalah fotografer; tidak ada kolom `uploaded_by`.
- Album baru selalu draft. Nama album unik per event (tidak peka huruf besar/kecil).
- Aturan status turunan ada di dua tempat yang harus sama: `stateCondition` di
  `photo_repository.go` (filter SQL) dan `photoState()` di
  `racetify-app/apps/dashboard/src/lib/gallery.ts`.
- Tag manual pada foto yang masih `pending` akan mengubah `ocr_status` menjadi
  `processed` (belum diterapkan, ada di tahap tagging), supaya foto yang sudah
  ditinjau manusia tampil Terverifikasi dan tidak ditimpa job OCR nanti.

## Belum dikerjakan

Worker `media.photo_process` (thumbnail, watermark, OCR), polling job, dan
`re-run-ocr` ditunda. `complete` mengembalikan `job_id: null` dan foto tetap
`pending`. Yang tersisa untuk klien: ringkasan per status, CRUD tag, pindah/hapus
massal, cleanup objek `pending` > 24 jam, dan gate `gallery:review`.

## Pengujian

`internal/gallery/gallery_integration_test.go` (tag `integration`) membuat database
sementara, menjalankan semua migrasi, dan menguji alur di atas dengan storage lokal:

    go test -tags=integration ./internal/gallery/... -v

Butuh `DATABASE_MIGRATOR_URL` dengan hak membuat database. Tanpa itu test di-skip.
