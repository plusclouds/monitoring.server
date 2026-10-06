-- Devices discovered by collectors (M4, F02, ADR-0014): an XCP-ng pool's
-- hosts and VMs. managed_by holds the collector check's ID and
-- collector_key the collector's own key (a UUID), so a VM keeps its device
-- across renames and migrations.

-- +goose Up
ALTER TABLE devices ADD COLUMN collector_key text, ADD COLUMN collector_gone_at timestamptz;
CREATE UNIQUE INDEX devices_collector_key ON devices (managed_by, collector_key) WHERE collector_key IS NOT NULL;

-- A collector object may belong to a discovered device (a VM's metrics).
ALTER TABLE check_objects ADD COLUMN device_id uuid;

-- +goose Down
ALTER TABLE check_objects DROP COLUMN device_id;
DROP INDEX devices_collector_key;
ALTER TABLE devices DROP COLUMN collector_gone_at, DROP COLUMN collector_key;
