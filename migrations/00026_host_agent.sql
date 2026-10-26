-- v0.15.0: host.agent checks (the monitoring agent in a Docker image, own push
-- token). An agent's ID names at most one check per tenant: its reports have
-- one home. Scoped to the tenant so one tenant cannot block, or probe for, the
-- IDs another tenant uses (box.agent is global because one provisioner owns all boxes).

-- +goose Up
CREATE UNIQUE INDEX checks_host_agent_id ON checks (tenant_id, (lower(config->>'agent_id'))) WHERE plugin = 'host.agent';

-- +goose Down
DROP INDEX checks_host_agent_id;
