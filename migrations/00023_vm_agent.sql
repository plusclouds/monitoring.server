-- v0.12.0: vm.agent checks (PlusClouds VM agent telemetry over NATS). A
-- VM's UUID names at most one check on the server: its telemetry has one
-- home.

-- +goose Up
CREATE UNIQUE INDEX checks_vm_agent_uuid ON checks ((lower(config->>'vm_uuid'))) WHERE plugin = 'vm.agent';

-- +goose Down
DROP INDEX checks_vm_agent_uuid;
