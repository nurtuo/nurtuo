-- SPDX-License-Identifier: AGPL-3.0-only
-- Identity, workspaces and RBAC reference data. Numbered from 0100 to stay clear of other branches.

CREATE TABLE users (
  id             text PRIMARY KEY,
  email          text NOT NULL,
  display_name   text NOT NULL DEFAULT '',
  password_hash  text,                                   -- scrypt$N$r$p$salt$hash; NULL for SSO-only accounts
  status         text NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
  failed_logins  integer NOT NULL DEFAULT 0,
  locked_until   timestamptz,
  created_at     timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT users_email_lower CHECK (email = lower(email))
);
CREATE UNIQUE INDEX users_email_key ON users (email);

CREATE TABLE external_identities (
  provider  text NOT NULL,                               -- e.g. oidc:https://issuer.example
  subject   text NOT NULL,
  user_id   text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  PRIMARY KEY (provider, subject)
);
CREATE INDEX external_identities_user ON external_identities (user_id);

CREATE TABLE workspaces (
  id          text PRIMARY KEY,
  -- slugs must not look like UUIDs, so "id or slug" lookups can never be ambiguous
  slug        text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,62}$' AND slug !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
  name        text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE roles (
  name         text PRIMARY KEY,
  builtin      boolean NOT NULL DEFAULT true,
  description  text NOT NULL DEFAULT ''
);

CREATE TABLE permissions (
  id        text PRIMARY KEY,                            -- resource:action
  resource  text NOT NULL,
  action    text NOT NULL
);

CREATE TABLE role_permissions (
  role        text NOT NULL REFERENCES roles(name) ON DELETE CASCADE,
  permission  text NOT NULL REFERENCES permissions(id) ON DELETE CASCADE,
  PRIMARY KEY (role, permission)
);

CREATE TABLE memberships (
  workspace_id  text NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  user_id       text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role          text NOT NULL REFERENCES roles(name),
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, user_id)
);
CREATE INDEX memberships_user ON memberships (user_id);
