// SPDX-License-Identifier: Apache-2.0
import type { Flow } from "@nurtuo/flow-schema";

/** Minimal typed client for the public REST API (generated from OpenAPI later). */
export class NurtuoClient {
  constructor(private readonly baseUrl: string, private readonly token?: string) {}

  async getFlow(id: string): Promise<Flow> {
    const res = await fetch(`${this.baseUrl}/api/v1/flows/${encodeURIComponent(id)}`, {
      headers: this.token ? { authorization: `Bearer ${this.token}` } : {},
    });
    if (!res.ok) throw new Error(`GET flow ${id}: ${res.status}`);
    return (await res.json()) as Flow;
  }
}
