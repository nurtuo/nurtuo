-- SPDX-License-Identifier: AGPL-3.0-only
CREATE TABLE runtimes (
  id           text PRIMARY KEY,
  name         text NOT NULL,
  environment  text NOT NULL,
  status       text NOT NULL DEFAULT 'offline' CHECK (status IN ('online','offline')),
  version      text NOT NULL DEFAULT '',
  labels       jsonb NOT NULL DEFAULT '{}',
  last_seen_at timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX runtimes_environment_idx ON runtimes(environment);
