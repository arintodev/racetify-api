DROP POLICY tenant_isolation ON face_embeddings;
ALTER TABLE face_embeddings DROP CONSTRAINT face_embeddings_subject_xor;
ALTER TABLE face_embeddings DROP CONSTRAINT face_embeddings_ref_id_format;
ALTER TABLE face_embeddings DROP COLUMN ref_id;
ALTER TABLE face_embeddings ADD COLUMN event_id UUID REFERENCES events(id) ON DELETE CASCADE;
ALTER TABLE face_embeddings ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE face_embeddings ALTER COLUMN tenant_id SET NOT NULL;
CREATE POLICY tenant_isolation ON face_embeddings
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY tenant_isolation ON faces;
DROP INDEX faces_user_uk;
DROP INDEX faces_tenant_ref_uk;
ALTER TABLE faces DROP CONSTRAINT faces_subject_xor;
ALTER TABLE faces DROP CONSTRAINT faces_ref_id_format;
ALTER TABLE faces DROP COLUMN ref_id;
ALTER TABLE faces ADD COLUMN event_id UUID REFERENCES events(id) ON DELETE CASCADE;
ALTER TABLE faces ADD COLUMN consent_revoked_at TIMESTAMPTZ NULL;
ALTER TABLE faces ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE faces ALTER COLUMN tenant_id SET NOT NULL;
CREATE INDEX faces_event_id_idx ON faces(event_id);
CREATE UNIQUE INDEX faces_tenant_event_user_uk ON faces (tenant_id, event_id, user_id);
CREATE POLICY tenant_isolation ON faces
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
