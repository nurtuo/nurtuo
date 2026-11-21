-- SPDX-License-Identifier: AGPL-3.0-only
CREATE TABLE components (
  id          text PRIMARY KEY,
  name        text NOT NULL UNIQUE,
  kind        text NOT NULL CHECK (kind IN ('flow','map','connector','script')),
  description text NOT NULL DEFAULT '',
  created_at  timestamptz NOT NULL DEFAULT now()
);

-- Immutable: rows are never updated or deleted by the application.
CREATE TABLE revisions (
  id           text PRIMARY KEY,
  component_id text NOT NULL REFERENCES components(id) ON DELETE RESTRICT,
  number       integer NOT NULL CHECK (number > 0),
  digest       text NOT NULL,
  content      jsonb NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (component_id, number)
);

CREATE FUNCTION revisions_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'revisions are immutable'; END $$;
CREATE TRIGGER revisions_no_update BEFORE UPDATE ON revisions
  FOR EACH ROW EXECUTE FUNCTION revisions_immutable();
