-- Rolled-up device status (v0.2.1): when the host check last entered or
-- left PROBLEM, so a device's up/down time is one indexed lookup.

-- +goose Up
ALTER TABLE check_state ADD COLUMN availability_since timestamptz;
UPDATE check_state SET availability_since = since;
CREATE INDEX incidents_device_active ON incidents (device_id) WHERE status <> 'resolved';

-- +goose Down
DROP INDEX incidents_device_active;
ALTER TABLE check_state DROP COLUMN availability_since;
