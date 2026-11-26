// SPDX-License-Identifier: AGPL-3.0-only
import assert from "node:assert/strict";
import { test } from "node:test";
import { checkEgress, isBlockedAddress } from "./egress.js";

test("refused ranges", () => {
  for (const ip of ["0.0.0.0", "0.1.2.3", "127.0.0.1", "127.255.255.254", "::1", "::", "10.0.0.5", "172.16.0.1", "172.31.255.255", "192.168.1.1", "100.64.0.1", "100.127.255.255", "169.254.0.1", "169.254.169.254", "fc00::1", "fd00:ec2::254", "fe80::1", "febf::1", "::ffff:127.0.0.1", "::ffff:7f00:1", "::ffff:10.1.2.3", "::ffff:a9fe:a9fe", "not-an-ip"]) assert.equal(isBlockedAddress(ip), true, ip);
});
test("transition prefixes and special-use IPv4 addresses are refused", () => {
  for (const ip of [
    "::127.0.0.1", "::a9fe:a9fe", "64:ff9b::a9fe:a9fe", "2002:7f00:1::",
    "192.0.0.1", "198.18.0.1", "198.19.255.255", "224.0.0.1", "240.0.0.1",
    "::ffff:198.18.0.1",
  ]) assert.equal(isBlockedAddress(ip), true, ip);
});
test("public addresses pass", () => {
  for (const ip of ["8.8.8.8", "172.15.0.1", "172.32.0.1", "100.63.0.1", "100.128.0.1", "169.253.0.1", "2606:4700::1111", "::ffff:8.8.8.8", "fec0::1"]) assert.equal(isBlockedAddress(ip), false, ip);
});
test("checkEgress resolves names (every address), honours the allow switch, fails closed", async () => {
  const lookup = async (h: string) => (h === "mixed" ? ["8.8.8.8", "10.0.0.1"] : h === "pub" ? ["8.8.8.8"] : []);
  await checkEgress("pub", { allowPrivate: false, lookup });
  await assert.rejects(checkEgress("mixed", { allowPrivate: false, lookup }), (e: any) => e.code === "host_not_allowed" && e.status === 400);
  await assert.rejects(checkEgress("nothing", { allowPrivate: false, lookup }), (e: any) => e.code === "host_not_allowed");
  await assert.rejects(checkEgress("127.0.0.1", { allowPrivate: false, lookup }), (e: any) => e.code === "host_not_allowed");
  await assert.rejects(checkEgress("[::1]", { allowPrivate: false, lookup }), (e: any) => e.code === "host_not_allowed");
  await checkEgress("mixed", { allowPrivate: true, lookup });
});
