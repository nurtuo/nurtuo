-- SPDX-License-Identifier: AGPL-3.0-only
-- Canvas Run permissions (docs/specs/2026-11-23-canvas-run.md D6). Mirrors src/auth/permissions.ts; crypto.test.ts fails on drift.
INSERT INTO permissions(id, resource, action) VALUES
  ('connection:read', 'connection', 'read'),
  ('connection:write', 'connection', 'write'),
  ('connection:delete', 'connection', 'delete'),
  ('secret:write', 'secret', 'write'),
  ('secret:delete', 'secret', 'delete'),
  ('test_run:execute', 'test_run', 'execute');

INSERT INTO role_permissions(role, permission) VALUES
  ('owner', 'connection:read'),
  ('owner', 'connection:write'),
  ('owner', 'connection:delete'),
  ('owner', 'secret:write'),
  ('owner', 'secret:delete'),
  ('owner', 'test_run:execute'),
  ('admin', 'connection:read'),
  ('admin', 'connection:write'),
  ('admin', 'connection:delete'),
  ('admin', 'secret:write'),
  ('admin', 'secret:delete'),
  ('admin', 'test_run:execute'),
  ('editor', 'connection:read'),
  ('editor', 'connection:write'),
  ('editor', 'connection:delete'),
  ('editor', 'secret:write'),
  ('editor', 'secret:delete'),
  ('editor', 'test_run:execute'),
  ('viewer', 'connection:read'),
  ('deployer', 'connection:read');

UPDATE roles SET description = 'Create and edit components and revisions, manage connections and secrets, test-run flows' WHERE name = 'editor';
