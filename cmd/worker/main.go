// Command worker is the Racetify async-job processor: it shares
// internal/app.Build's exact wiring with cmd/api (docs/phase1-api-plan.md
// §9's "there is exactly one composition root" rule - this binary is a
// pure consumer of the one graph app.Build assembles, never a second place
// that constructs a repository or a service by hand), then runs
// internal/jobqueue.Run's block-dequeue-dispatch-record loop until
// SIGINT/SIGTERM triggers a graceful stop.
//
// No job-type handler is registered yet - this is Step 3 of
// docs/phase1-api-plan.md §8's build order ("jobqueue + cmd/worker
// skeleton ... to de-risk the queue mechanics before real handlers depend
// on it"). Step 4 (participant/import), Step 5 (generator), and Step 6
// (gallery/jobs) each add one dispatcher.Register(...) call below as they
// land.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/racetify/racetify-api/internal/app"
	"github.com/racetify/racetify-api/internal/bibprint"
	"github.com/racetify/racetify-api/internal/certificate"
	"github.com/racetify/racetify-api/internal/config"
	"github.com/racetify/racetify-api/internal/face"
	"github.com/racetify/racetify-api/internal/gallery"
	"github.com/racetify/racetify-api/internal/jobqueue"
	"github.com/racetify/racetify-api/internal/platform/logger"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}
	log := logger.New(cfg.Env)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.Build(ctx, cfg, log)
	if err != nil {
		log.Error("build app", "error", err)
		os.Exit(1)
	}
	defer a.Close()

	dispatcher := jobqueue.NewDispatcher()
	// Phase 1 modules register their job handlers here as they land:
	//   participantimport.RegisterJob(dispatcher, a.Handler.Participant)
	//   generator.RegisterJob(dispatcher, a.Handler.Generator)

	bibprint.RegisterJob(dispatcher, a.Handler.BibPrint)
	certificate.RegisterJob(dispatcher, a.Handler.Certificates)
	// Singular RegisterJob, matching bibprint/certificate above: gallery
	// registers exactly one job type (media.photo_process, thumbnail
	// generation only for now - see thumbnail_job.go's doc comment).
	// Watermarking and OCR, once built, extend this same job's work rather
	// than adding a second job type, so this stays a single call.
	gallery.RegisterJob(dispatcher, a.Handler.Gallery)
	// Face detection (docs/face-search-plan.md) - a second, independent job
	// over the same upload batch gallery.RegisterJob's job type covers; see
	// gallery/photo_service.go's CompleteUploads for the dual-enqueue.
	face.RegisterJob(dispatcher, a.Handler.Face)

	log.Info("racetify-worker starting", "env", cfg.Env)
	jobqueue.Run(ctx, a.Handler.JobQueue, dispatcher, log)
	log.Info("racetify-worker stopped")
}
