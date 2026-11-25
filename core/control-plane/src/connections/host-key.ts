// SPDX-License-Identifier: AGPL-3.0-only
import { ECDH } from "node:crypto";

const curves = new Map([
  ["nistp256", "prime256v1"],
  ["nistp384", "secp384r1"],
  ["nistp521", "secp521r1"],
]);

export function validHostKey(algorithm: string, key: Buffer): boolean {
  let offset = 0;
  const read = () => {
    const size = key.readUInt32BE(offset);
    offset += 4;
    if (size > key.length - offset) throw new RangeError("truncated SSH field");
    const field = key.subarray(offset, offset + size);
    offset += size;
    return field;
  };
  const positiveInteger = () => {
    const bytes = read();
    return bytes.length > 0 && bytes[0]! < 128 &&
      (bytes[0] !== 0 || (bytes.length > 1 && bytes[1]! >= 128));
  };
  try {
    if (!read().equals(Buffer.from(algorithm))) return false;
    let valid: boolean;
    switch (algorithm) {
      case "ssh-ed25519":
        valid = read().length === 32;
        break;
      case "ssh-rsa":
        valid = positiveInteger() && positiveInteger();
        break;
      case "ssh-dss":
        valid = positiveInteger() && positiveInteger() && positiveInteger() && positiveInteger();
        break;
      default: {
        const name = read().toString("ascii");
        const curve = curves.get(name);
        if (!curve || algorithm !== `ecdsa-sha2-${name}`) return false;
        const point = read();
        valid = point[0] === 4 && Buffer.isBuffer(ECDH.convertKey(point, curve));
      }
    }
    return valid && offset === key.length;
  } catch {
    return false;
  }
}
