// SPDX-License-Identifier: AGPL-3.0-only
import { randomBytes } from "node:crypto";

export async function openTestSchema(connectionString: string) {
  const { default: pg } = await import("pg");
  const schema = `test_${randomBytes(8).toString("hex")}`;
  const admin = new pg.Pool({ connectionString });
  const isolated = new URL(connectionString);
  const options = isolated.searchParams.get("options");
  isolated.searchParams.set("options", [options, `-c search_path=${schema}`].filter(Boolean).join(" "));
  try {
    await admin.query(`CREATE SCHEMA ${schema}`);
  } catch (error) {
    await admin.end();
    throw error;
  }
  const pool = new pg.Pool({ connectionString: isolated.toString() });
  return {
    pool,
    async close() {
      try {
        await pool.end();
        await admin.query(`DROP SCHEMA ${schema} CASCADE`);
      } finally {
        await admin.end();
      }
    },
  };
}
