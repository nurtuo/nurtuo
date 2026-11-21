-- SPDX-License-Identifier: AGPL-3.0-only
CREATE TABLE api_tokens (
  id            text PRIMARY KEY,
  workspace_id  text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  created_by    text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name          text NOT NULL,
  prefix        text NOT NULL UNIQUE,                    -- public lookup part of nrt_<prefix>_<secret>
  hash          text NOT NULL,                           -- sha256 hex of the secret; the secret is never stored
  scopes        text[] NOT NULL,                         -- subset of permissions.id
  expires_at    timestamptz,
  last_used_at  timestamptz,
  revoked_at    timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX api_tokens_workspace ON api_tokens (workspace_id);

CREATE TABLE sessions (
  id          text PRIMARY KEY,
  user_id     text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at  timestamptz NOT NULL DEFAULT now(),
  expires_at  timestamptz NOT NULL,
  revoked_at  timestamptz,
  ip          text,
  user_agent  text
);
CREATE INDEX sessions_user ON sessions (user_id);
