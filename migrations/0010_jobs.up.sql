-- Phase 1: generic async-job table (docs/phase1-api-plan.md §2/§6), shared
-- by every Phase 1 background flow - CSV import, BIB batch generation,
-- photo thumbnail+watermark+OCR - rather than one table per job type.
-- Mirrors the README's own prediction that the objects table's status
-- column pattern is one a job-status column can reuse directly.
CREATE TABLE jobs (
    id                UUID PRIMARY KEY,
    tenant_id         UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    -- participants.import | generator.bib_batch | media.photo_process -
    -- see internal/domain/job.go's JobType.
    type              TEXT NOT NULL,
    -- queued | processing | completed | completed_with_errors | failed -
    -- see internal/domain/job.go's JobStatus.
    status            TEXT NOT NULL DEFAULT 'queued',
    payload           JSONB NOT NULL DEFAULT '{}',
    result            JSONB NULL,
    error             TEXT NULL,
    progress_current  INTEGER NOT NULL DEFAULT 0,
    progress_total    INTEGER NOT NULL DEFAULT 0,
    created_by        UUID NOT NULL REFERENCES users(id),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at        TIMESTAMPTZ NULL,
    finished_at       TIMESTAMPTZ NULL
);

CREATE TRIGGER jobs_set_updated_at
    BEFORE UPDATE ON jobs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX jobs_tenant_id_idx ON jobs(tenant_id);
CREATE INDEX jobs_type_idx ON jobs(type);
CREATE INDEX jobs_status_idx ON jobs(status);

ALTER TABLE jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE jobs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON jobs
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
