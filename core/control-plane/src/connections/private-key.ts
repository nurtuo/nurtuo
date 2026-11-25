// SPDX-License-Identifier: AGPL-3.0-only
import { spawn } from "node:child_process";
import { ApiError } from "../domain.js";

export async function validatePrivateKey(privateKey: string, passphrase?: string): Promise<void> {
  const result = await new Promise<number | null>((resolve) => {
    const child = spawn(process.env.NURTUO_FLOW_RUN_BIN || "nurtuo", ["_validate-sftp-key"], {
      env: { PATH: process.env.PATH },
      stdio: ["pipe", "ignore", "ignore"],
      timeout: 5_000,
      killSignal: "SIGKILL",
    });
    child.on("error", () => resolve(null));
    child.on("close", resolve);
    child.stdin.on("error", () => { /* Early exit is reported by the child. */ });
    child.stdin.end(JSON.stringify({ privateKey, passphrase }));
  });
  if (result === 1) {
    throw new ApiError(400, "invalid_credential_set", "private key could not be opened with the supplied passphrase");
  }
  if (result !== 0) {
    throw new ApiError(503, "private_key_validation_unavailable", "private key validation is unavailable");
  }
}
