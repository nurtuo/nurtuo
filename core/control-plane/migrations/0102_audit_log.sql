-- SPDX-License-Identifier: AGPL-3.0-only
-- Append-only audit log. workspace_id is deliberately NOT a foreign key: audit rows must survive
-- workspace/user deletion and may reference no workspace (e.g. failed logins).
CREATE TABLE audit_log (
  seq           bigserial PRIMARY KEY,
  id            text NOT NULL UNIQUE,
  ts            timestamptz NOT NULL DEFAULT now(),
  workspace_id  text,
  actor_type    text NOT NULL CHECK (actor_type IN ('user','token','anonymous')),
  actor_id      text,
  action        text NOT NULL,
  resource      text NOT NULL,
  resource_id   text,
  before_hash   text,
  after_hash    text,
  ip            text,
  request_id    text NOT NULL,
  status        integer NOT NULL,
  metadata      jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_log_ws_seq ON audit_log (workspace_id, seq DESC);
CREATE INDEX audit_log_ws_actor ON audit_log (workspace_id, actor_id, seq DESC);
CREATE INDEX audit_log_ws_action ON audit_log (workspace_id, action, seq DESC);

CREATE FUNCTION audit_log_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'audit_log is append-only (% blocked)', TG_OP USING ERRCODE = 'integrity_constraint_violation';
END $$;

CREATE TRIGGER audit_log_no_update_delete BEFORE UPDATE OR DELETE ON audit_log
  FOR EACH ROW EXECUTE FUNCTION audit_log_immutable();
CREATE TRIGGER audit_log_no_truncate BEFORE TRUNCATE ON audit_log
  FOR EACH STATEMENT EXECUTE FUNCTION audit_log_immutable();
