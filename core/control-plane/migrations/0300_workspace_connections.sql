-- SPDX-License-Identifier: AGPL-3.0-only
-- Workspace-scoped connections for canvas test runs (docs/specs/2026-11-23-canvas-run.md, D3/D8).
-- Settings hold only secret:// references, never secret values. Numbered clear of 0100 (auth) and 0200 (workspace isolation).
CREATE TABLE connections (
  workspace_id text        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  id           text        NOT NULL,
  connector    text        NOT NULL,
  settings     jsonb       NOT NULL,
  incarnation  uuid,
  generation   uuid,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, id)
);
