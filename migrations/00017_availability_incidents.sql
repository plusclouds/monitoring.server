-- Suppression by discovered devices (M4, F05): an incident is an
-- availability incident when it says its device is down. A host check's
-- incident always was one; now a collector object can be one too (an
-- XCP-ng host or VM), so a host that goes down suppresses its VMs.

-- +goose Up
ALTER TABLE incidents ADD COLUMN availability boolean NOT NULL DEFAULT false;
UPDATE incidents i SET availability = true
  FROM checks c WHERE c.id = i.check_id AND c.is_host_check AND i.object_key IS NULL;
CREATE INDEX incidents_availability ON incidents (device_id) WHERE availability AND status <> 'resolved';

-- +goose Down
DROP INDEX incidents_availability;
ALTER TABLE incidents DROP COLUMN availability;
