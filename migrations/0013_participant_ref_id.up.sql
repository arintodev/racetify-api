-- Optional reference ID from the organizer's own registration system
-- (order/ticket/registration number). The Wedge strategy ingests
-- participants from an external registration system, and BIB alone is a
-- weak identity: an organizer can reassign a BIB and re-import, which
-- would otherwise create a duplicate person. When ref_id is present, the
-- registration import matches on ref_id first and BIB second, so a BIB
-- change is detected as a change instead of a new participant
-- (docs/phase1-api-plan.md §4.2).
--
-- Plain TEXT, no format assumed: every registration system numbers
-- differently ("INV-2026-000123", "8841", a UUID).
ALTER TABLE participants ADD COLUMN ref_id TEXT NULL;

-- Unique per event when present; most rows imported by hand or from a
-- system without IDs leave it NULL, which the partial index ignores.
CREATE UNIQUE INDEX participants_event_ref_id_uk
    ON participants(event_id, ref_id)
    WHERE ref_id IS NOT NULL;
