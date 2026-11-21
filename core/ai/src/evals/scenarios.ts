// SPDX-License-Identifier: AGPL-3.0-only
// The eval corpus: scenarios (goal + expectations) and the reference op lists that solve them.
// An LLM agent receives only `goal`; the reference ops are the scripted baseline and a regression
// test for the tools, validator and schemas.
import { FLOW_SCHEMA_VERSION, type Flow } from "@nurtuo/flow-schema";
import type { Op, Scenario } from "./scenario.js";

export const getEmployee: Scenario = {
  id: "get-employee",
  title: "GetEmployee: HTTP listener -> Postgres lookup -> 404 or mapped response",
  flowId: "get-employee",
  flowName: "GetEmployee",
  goal:
    "Expose GET /employees/{id} over HTTP. Look the employee up in Postgres. If found, return a JSON object with the id and the full name " +
    "(first and last name joined); if not found, end with HTTP status 404.",
  expect: {
    start: { type: "listener", connector: "http" },
    connectors: [{ connector: "postgres", operation: "QUERY" }],
    minStepKinds: { decision: 1, map: 1, returnDocuments: 1, stop: 1 },
    pathKinds: ["start", "connector", "decision", "map", "returnDocuments"],
    edges: ["found:true", "found:false"],
    minRevision: 1,
    maxToolCalls: 12,
  },
};

export const getEmployeeOps: Op[][] = [
  [
    { op: "addStep", step: { id: "start", kind: "start", config: { type: "listener", connector: "http", path: "/employees/{id}", method: "GET" } } },
    { op: "addStep", after: { step: "start", port: "default" }, step: { id: "get_employee", kind: "connector", label: "Load employee", config: { connector: "postgres", operation: "QUERY", objectType: "employee" } } },
    { op: "addStep", after: { step: "get_employee", port: "default" }, step: { id: "found", kind: "decision", config: { cel: "doc.id != ''" } } },
  ],
  [
    { op: "addStep", after: { step: "found", port: "true" }, step: { id: "to_response", kind: "map", config: { jsonata: "{ 'id': id, 'name': first_name & ' ' & last_name }" } } },
    { op: "addStep", after: { step: "to_response", port: "default" }, step: { id: "return", kind: "returnDocuments", config: {} } },
    { op: "addStep", after: { step: "found", port: "false" }, step: { id: "not_found", kind: "stop", config: { status: 404, message: "employee not found" } } },
  ],
];

export const httpMapSftp: Scenario = {
  id: "http-map-sftp",
  title: "HTTP -> Map -> SFTP: nightly export of an API to a file server",
  flowId: "orders-to-sftp",
  flowName: "Orders to SFTP",
  goal:
    "Every night at 02:00 call GET /orders on an HTTP API, reshape each order to { orderId, total, customer } and upload the result to an SFTP server. " +
    "If anything fails, notify the ops channel.",
  expect: {
    start: { type: "schedule", cron: "0 2 * * *" },
    connectors: [{ connector: "http", operation: "GET" }, { connector: "sftp", operation: "CREATE" }],
    minStepKinds: { map: 1, tryCatch: 1, notify: 1 },
    pathKinds: ["start", "tryCatch", "connector", "map", "connector", "returnDocuments"],
    minRevision: 1,
    maxToolCalls: 12,
  },
};

export const httpMapSftpOps: Op[][] = [
  [
    { op: "addStep", step: { id: "start", kind: "start", config: { type: "schedule", cron: "0 2 * * *" } } },
    { op: "addStep", after: { step: "start", port: "default" }, step: { id: "guard", kind: "tryCatch", config: { retries: 2 } } },
    { op: "addStep", after: { step: "guard", port: "try" }, step: { id: "fetch_orders", kind: "connector", label: "GET /orders", config: { connector: "http", operation: "GET", objectType: "/orders" } } },
    { op: "addStep", after: { step: "fetch_orders", port: "default" }, step: { id: "reshape", kind: "map", config: { jsonata: "$.{ 'orderId': id, 'total': total, 'customer': customer.name }" } } },
    { op: "addStep", after: { step: "reshape", port: "default" }, step: { id: "upload", kind: "connector", label: "Upload to SFTP", config: { connector: "sftp", operation: "CREATE", objectType: "/outbound/orders.json" } } },
  ],
  [
    { op: "addStep", after: { step: "upload", port: "default" }, step: { id: "done", kind: "returnDocuments", config: {} } },
    { op: "addStep", after: { step: "guard", port: "catch" }, step: { id: "alert", kind: "notify", config: { channel: "ops", template: "Nightly order export failed" } } },
    { op: "addStep", after: { step: "alert", port: "default" }, step: { id: "failed", kind: "stop", config: { status: 500, message: "export failed" } } },
  ],
];

/** A flow with typical mistakes: a decision wired with the wrong port, a missing branch, a stray step. */
const brokenFlow: Flow = {
  schema: FLOW_SCHEMA_VERSION, id: "broken-sync", name: "Broken sync", revision: 1,
  steps: [
    { id: "start", kind: "start", config: { type: "manual" } },
    { id: "check", kind: "decision", config: { cel: "doc.ok" } },
    { id: "send", kind: "connector", config: { connector: "http", operation: "POST" } },
    { id: "end", kind: "returnDocuments", config: {} },
    { id: "orphan", kind: "map", config: { jsonata: "$" } },
  ],
  edges: { "start:default": "check", "check:default": "send", "send:default": "end" },
};

export const repairFlow: Scenario = {
  id: "repair-broken-flow",
  title: "Repair: fix an invalid flow using diagnostics",
  flowId: "broken-sync",
  flowName: "Broken sync",
  goal: "The flow broken-sync does not validate. Repair it with minimal changes so that it validates, keeping the HTTP POST step on the true branch.",
  setup: async (store) => { await store.saveFlow(structuredClone(brokenFlow), 0); },
  expect: {
    connectors: [{ connector: "http", operation: "POST" }],
    minStepKinds: { decision: 1, connector: 1 },
    edges: ["check:true", "check:false"],
    minRevision: 2,
    maxToolCalls: 10,
  },
};

/** Reference repair: the `default` edge of the decision is invalid; re-wire true/false and drop the orphan. */
export const repairFlowOps: Op[][] = [
  [
    { op: "disconnect", from: "check", port: "default" },
    { op: "connect", from: "check", port: "true", to: "send" },
    { op: "addStep", after: { step: "check", port: "false" }, step: { id: "skip", kind: "stop", config: { message: "nothing to send" } } },
    { op: "removeStep", id: "orphan" },
  ],
];

export const SCENARIOS: { scenario: Scenario; reference: Op[][] }[] = [
  { scenario: getEmployee, reference: getEmployeeOps },
  { scenario: httpMapSftp, reference: httpMapSftpOps },
  { scenario: repairFlow, reference: repairFlowOps },
];
