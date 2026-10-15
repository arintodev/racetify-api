# BIB Detection Job Plan

## Goal

Separate the BIB‑OCR part of the `media.photo_process` job into its own
background job.  The existing `thumbnail_job.go` will now only generate
thumbnails, while a new `bibdetect` job will perform OCR and create
`photo_tags` for each photo.

## Implementation Steps

1. **Remove OCR logic from `thumbnail_job.go`** – drop the `runOCR` calls
   and all related counters (`bibMade`, `bibSkipped`, `bibFailed`).
2. **Add a new package `internal/bibdetect`** with:
   * `Service` – holds a reference to `gallery.Service` and the job queue.
   * `NewService` – constructor.
   * `RegisterJob` – registers `RunBibDetect` with the dispatcher.
   * `RunBibDetect` – processes the payload by invoking `gallery.Service.RunOCR`
     for each photo ID, updates progress and aggregates failures.
3. **Expose the service via `httpapi.Deps`** – add a `BibDetect` field and
   instantiate it in `app.Build`.
4. **Register the job in the worker** – add `bibdetect.RegisterJob` in
   `cmd/worker/main.go`.
5. **Update documentation** – this file explains the new job and its
   registration.

## Pitfalls

* Ensure the `gallery.Service` used by the job contains an instantiated
  OCR client; otherwise the job will fail.
* The `RunBibDetect` method does not set `ocr_status` – this is handled by
  the underlying `gallery.Service.RunOCR` call.
* Existing tests that expect the `media.photo_process` job to run OCR will
  need to be updated to use the new job instead.

This plan keeps the original architecture unchanged while providing a clean
separation of concerns.
