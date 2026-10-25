-- v0.15.0: host.agent checks (the monitoring agent in a Docker image, own push
-- token). An agent's ID names at most one check on the server: its reports
-- have one home.

-- +goose Up
CREATE UNIQUE INDEX checks_host_agent_id ON checks ((lower(config->>'agent_id'))) WHERE plugin = 'host.agent';

-- +goose Down
DROP INDEX checks_host_agent_id;
