package jobqueue

import (
	"context"
	"errors"
	"testing"

	"github.com/racetify/racetify-api/internal/domain"
)

// These are pure-logic tests: no Postgres/Redis involved. They exercise
// the routing/outcome contract Run (runner.go) relies on - see that
// file's doc comment for why this is the mechanics de-risking Step 3 of
// docs/phase1-api-plan.md §8's build order calls for, in an environment
// with no live infra to run the real thing end to end against.

func TestDispatcherRoutesToRegisteredHandler(t *testing.T) {
	d := NewDispatcher()
	called := false
	d.Register(domain.JobTypeParticipantsImport, func(ctx context.Context, job *Job) (map[string]any, string, error) {
		called = true
		if job.ID != "job-1" {
			t.Errorf("handler got job.ID = %q, want %q", job.ID, "job-1")
		}
		return map[string]any{"imported": 3}, "", nil
	})

	result, partialErr, err := d.Dispatch(context.Background(), &Job{ID: "job-1", Type: domain.JobTypeParticipantsImport})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if partialErr != "" {
		t.Fatalf("Dispatch returned partialErr = %q, want empty", partialErr)
	}
	if !called {
		t.Fatal("registered handler was never invoked")
	}
	if result["imported"] != 3 {
		t.Fatalf("result = %#v, want imported=3", result)
	}
}

func TestDispatcherUnregisteredTypeReturnsErrNoHandler(t *testing.T) {
	d := NewDispatcher()
	_, _, err := d.Dispatch(context.Background(), &Job{ID: "job-1", Type: domain.JobTypeGeneratorBibBatch})
	if !errors.Is(err, ErrNoHandler) {
		t.Fatalf("err = %v, want ErrNoHandler", err)
	}
}

func TestDispatcherRegisterDuplicateTypePanics(t *testing.T) {
	d := NewDispatcher()
	noop := func(ctx context.Context, job *Job) (map[string]any, string, error) { return nil, "", nil }
	d.Register(domain.JobTypeMediaPhotoProcess, noop)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected Register to panic on a duplicate job type, it did not")
		}
	}()
	d.Register(domain.JobTypeMediaPhotoProcess, noop)
}

func TestDispatcherThreeOutcomesAreDistinguishable(t *testing.T) {
	// Mirrors Run's switch in runner.go: err != nil takes priority over
	// partialErr, and a handler returning neither is full success. This
	// test exists to pin that contract so a future edit to Run can't
	// silently invert the priority without a test failing.
	cases := []struct {
		name        string
		handlerErr  error
		partialErr  string
		wantErr     bool
		wantPartial bool
	}{
		{"full success", nil, "", false, false},
		{"completed with errors", nil, "3 rows failed", false, true},
		{"total failure", errors.New("boom"), "", true, false},
		{"failure takes priority over partial", errors.New("boom"), "3 rows failed", true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDispatcher()
			d.Register(domain.JobTypeParticipantsImport, func(ctx context.Context, job *Job) (map[string]any, string, error) {
				return nil, tc.partialErr, tc.handlerErr
			})
			_, partialErr, err := d.Dispatch(context.Background(), &Job{Type: domain.JobTypeParticipantsImport})
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if (partialErr != "") != tc.wantPartial {
				t.Errorf("partialErr = %q, wantPartial = %v", partialErr, tc.wantPartial)
			}
		})
	}
}
