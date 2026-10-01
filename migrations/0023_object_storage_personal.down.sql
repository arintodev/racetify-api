DROP POLICY tenant_isolation ON objects;
DROP INDEX objects_personal_bucket_key_uk;
ALTER TABLE objects ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE objects ALTER COLUMN created_by SET NOT NULL;
CREATE POLICY tenant_isolation ON objects
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
