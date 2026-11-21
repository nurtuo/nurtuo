-- SPDX-License-Identifier: AGPL-3.0-only
-- Desired state per runtime: one row, generation bumps on every change.
CREATE TABLE desired_state (
  runtime_id  text PRIMARY KEY REFERENCES runtimes(id) ON DELETE CASCADE,
  environment text NOT NULL,
  generation  bigint NOT NULL DEFAULT 0,
  items       jsonb NOT NULL DEFAULT '{}',  -- {componentId: revisionId}
  updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE deployments (
  id           text PRIMARY KEY,
  component_id text NOT NULL REFERENCES components(id),
  revision_id  text NOT NULL REFERENCES revisions(id),
  runtime_id   text NOT NULL REFERENCES runtimes(id),
  environment  text NOT NULL,
  generation   bigint NOT NULL,
  status       text NOT NULL DEFAULT 'pending'
               CHECK (status IN ('pending','running','failed','stopped')),
  message      text NOT NULL DEFAULT '',
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX deployments_runtime_idx ON deployments(runtime_id, created_at DESC);

-- Transactional outbox: written in the same tx as the desired-state change.
CREATE TABLE outbox (
  id           bigserial PRIMARY KEY,
  topic        text NOT NULL,
  payload      jsonb NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  published_at timestamptz
);
CREATE INDEX outbox_unpublished_idx ON outbox(id) WHERE published_at IS NULL;
