// SPDX-License-Identifier: AGPL-3.0-only
import { describe, expect, it } from "vitest";
import { configureWorkspaceConnector, workspaceOperations } from "./connectorOperations";

describe("workspace connector operations", () => {
  it("selects HTTP GET with a saved HTTP connection", () => {
    expect(configureWorkspaceConnector({ connector: "", operation: "" }, { id: "employees-api", connector: "http" }))
      .toEqual({ connector: "http", connection: "employees-api", operation: "GET" });
    expect(workspaceOperations("http")).toEqual(["GET"]);
  });

  it("preserves the case of an existing allowed operation when binding", () => {
    expect(configureWorkspaceConnector({ connector: "http", operation: "get", objectType: "legacy" }, { id: "web", connector: "http" }))
      .toEqual({ connector: "http", connection: "web", operation: "get" });
  });

  it("replaces stale operation fields when moving to PostgreSQL", () => {
    expect(configureWorkspaceConnector({ connector: "http", operation: "GET", objectType: "unused" }, { id: "employees-db", connector: "postgres" }))
      .toEqual({ connector: "postgres", connection: "employees-db", operation: "QUERY" });
    expect(workspaceOperations("postgres")).toEqual(["QUERY"]);
  });
});
