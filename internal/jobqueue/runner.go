package jobqueue

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// pollBlockDuration is how long each Dequeue call blocks waiting for a job
// before looping back to check ctx.Done() - short enough that shutdown
// (cmd/worker's signal.NotifyContext on SIGINT/SIGTERM) is noticed
// promptly, long enough that an idle worker isn't hammering Redis with
// near-instant BRPOP round trips.
const pollBlockDuration = 5 * time.Second

// dequeueErrorBackoff pauses the loop briefly after a genuine Dequeue
// error (Redis unreachable, a protocol error) so a persistent outage
// doesn't turn into a tight, log-spamming retry loop.
const dequeueErrorBackoff = 2 * time.Second

// Run is cmd/worker/main.go's poll loop: block-dequeue a job, dispatch it
// to whichever handler dispatcher has registered for its type, and record
// the outcome via queue's Mark* methods - looping until ctx is canceled.
//
// This is Step 3 of docs/phase1-api-plan.md §8's build order: "jobqueue +
// cmd/worker skeleton with one trivial job type, to de-risk the queue
// mechanics before real handlers depend on it." No handler is registered
// on dispatcher yet at this step (Steps 4-6 each register one as they're
// built), so in production this loop currently just idles, blocking on an
// empty list - the mechanics themselves (Enqueue -> Redis -> Dequeue ->
// Dispatch -> Mark*) are proven by this package's own unit tests
// (dispatcher_test.go), since no live Postgres/Redis is reachable from
// this sandboxed build environment to exercise the real thing end to end.
func Run(ctx context.Context, queue *Queue, dispatcher *Dispatcher, log *slog.Logger) {
	for {
		if ctx.Err() != nil {
			log.Info("jobqueue: shutting down")
			return
		}

		job, err := queue.Dequeue(ctx, pollBlockDuration)
		if err != nil {
			if errors.Is(err, ErrEmpty) || errors.Is(err, context.Canceled) {
				continue
			}
			log.Error("jobqueue: dequeue failed", "error", err)
			time.Sleep(dequeueErrorBackoff)
			continue
		}

		log.Info("jobqueue: processing job", "job_id", job.ID, "type", job.Type, "tenant_id", job.TenantID)
		if err := queue.MarkProcessing(ctx, job); err != nil {
			log.Error("jobqueue: mark processing failed", "job_id", job.ID, "error", err)
			continue
		}

		result, partialErr, err := dispatcher.Dispatch(ctx, job)
		switch {
		case err != nil:
			log.Error("jobqueue: job failed", "job_id", job.ID, "type", job.Type, "error", err)
			if markErr := queue.MarkFailed(ctx, job, err.Error()); markErr != nil {
				log.Error("jobqueue: mark failed failed", "job_id", job.ID, "error", markErr)
			}
		case partialErr != "":
			log.Warn("jobqueue: job completed with errors", "job_id", job.ID, "type", job.Type, "error", partialErr)
			if markErr := queue.MarkCompletedWithErrors(ctx, job, result, partialErr); markErr != nil {
				log.Error("jobqueue: mark completed_with_errors failed", "job_id", job.ID, "error", markErr)
			}
		default:
			log.Info("jobqueue: job completed", "job_id", job.ID, "type", job.Type)
			if markErr := queue.MarkCompleted(ctx, job, result); markErr != nil {
				log.Error("jobqueue: mark completed failed", "job_id", job.ID, "error", markErr)
			}
		}
	}
}
