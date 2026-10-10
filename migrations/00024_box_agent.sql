-- v0.13.0: box.agent checks (LLM box heartbeats over HTTPS, own push token). A
-- box's ID names at most one check on the server: its heartbeats have one home.

-- +goose Up
CREATE UNIQUE INDEX checks_box_agent_id ON checks ((lower(config->>'box_id'))) WHERE plugin = 'box.agent';

-- +goose Down
DROP INDEX checks_box_agent_id;
