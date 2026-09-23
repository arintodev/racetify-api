package jobqueue

import "time"

// JobDTO is the wire shape for GET /api/v1/jobs/{id} and GET /api/v1/jobs
// (docs/phase1-api-plan.md §4.5) - status/progress/result/error, the
// fields a client polling a long-running import/generation/photo-process
// job actually needs.
type JobDTO struct {
	ID              string         `json:"id"`
	Type            string         `json:"type"`
	Status          string         `json:"status"`
	ProgressCurrent int            `json:"progress_current"`
	ProgressTotal   int            `json:"progress_total"`
	Result          map[string]any `json:"result,omitempty"`
	Error           *string        `json:"error,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	StartedAt       *time.Time     `json:"started_at,omitempty"`
	FinishedAt      *time.Time     `json:"finished_at,omitempty"`
}

func jobResponse(j *Job) JobDTO {
	return JobDTO{
		ID:              j.ID,
		Type:            string(j.Type),
		Status:          string(j.Status),
		ProgressCurrent: j.ProgressCurrent,
		ProgressTotal:   j.ProgressTotal,
		Result:          j.Result,
		Error:           j.Error,
		CreatedAt:       j.CreatedAt,
		StartedAt:       j.StartedAt,
		FinishedAt:      j.FinishedAt,
	}
}
