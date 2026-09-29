-- Adds what the public Runner Portal microsite (internal/portal, this
-- migration's companion Go package) needs on events: a brand color for
-- the public page's theme, and a slug that is unique across ALL tenants,
-- not just within one - the public route is GET /api/v1/public/events/
-- {slug}, with no tenant segment in the URL at all (0006_events_races.
-- up.sql deferred this until "subdomain routing... needs it literally
-- 1:1"; the portal shipping without a tenant segment in its URL is that
-- point arriving).
--
-- Run this only after checking for existing slug collisions across
-- tenants (`SELECT slug, COUNT(*) FROM events GROUP BY slug HAVING
-- COUNT(*) > 1`) - the ADD CONSTRAINT below fails outright if any exist,
-- and this migration does not attempt to auto-rename them.
ALTER TABLE events ADD COLUMN primary_color TEXT NULL;

ALTER TABLE events ADD CONSTRAINT events_slug_uk UNIQUE (slug);
