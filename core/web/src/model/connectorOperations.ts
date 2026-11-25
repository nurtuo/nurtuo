// SPDX-License-Identifier: AGPL-3.0-only

// Keep in sync with the test-run allowlist in core/control-plane/src/testrun/policy.ts.
export function workspaceOperations(connector: unknown): readonly string[] | undefined {
  if (connector === "http") return ["GET"];
  if (connector === "postgres") return ["QUERY"];
  return undefined;
}

export function configureWorkspaceConnector(config: Record<string, unknown>, connection: { id: string; connector: string }) {
  const operations = workspaceOperations(connection.connector);
  if (!operations) return { ...config, connection: connection.id };
  const next: Record<string, unknown> = {
    ...config,
    connector: connection.connector,
    connection: connection.id,
    operation: config.connector === connection.connector && operations.includes(String(config.operation).toUpperCase())
      ? config.operation
      : operations[0],
  };
  delete next.objectType;
  return next;
}
