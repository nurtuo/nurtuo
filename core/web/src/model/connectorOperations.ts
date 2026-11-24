// SPDX-License-Identifier: AGPL-3.0-only

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
    operation: config.connector === connection.connector && operations.includes(String(config.operation))
      ? config.operation
      : operations[0],
  };
  if (config.connector !== connection.connector) delete next.objectType;
  return next;
}
