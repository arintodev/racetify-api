DROP POLICY IF EXISTS tenant_isolation ON audit_logs;
ALTER TABLE audit_logs NO FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_logs DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON oauth_clients;
ALTER TABLE oauth_clients NO FORCE ROW LEVEL SECURITY;
ALTER TABLE oauth_clients DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON invitations;
ALTER TABLE invitations NO FORCE ROW LEVEL SECURITY;
ALTER TABLE invitations DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON tenant_members;
ALTER TABLE tenant_members NO FORCE ROW LEVEL SECURITY;
ALTER TABLE tenant_members DISABLE ROW LEVEL SECURITY;

-- Same {{APP_ROLE}}/{{ADMIN_ROLE}} templating as 0003_row_level_security.up.sql
-- - see that file's doc comment.
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM {{APP_ROLE}}, {{ADMIN_ROLE}};
REVOKE USAGE ON SCHEMA public FROM {{APP_ROLE}}, {{ADMIN_ROLE}};
