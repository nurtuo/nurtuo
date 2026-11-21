// SPDX-License-Identifier: AGPL-3.0-only
import type { Endpoint } from "./endpoints.js";
import { notImplemented } from "./enterprise-stubs.js";

const saml: Endpoint = async () => notImplemented("saml");
const scim: Endpoint = async () => notImplemented("scim");

export const enterpriseStubs: Record<string, Endpoint> = {
  "auth.saml_login": saml, "auth.saml_acs": saml, "auth.saml_metadata": saml,
  "scim.users.list": scim, "scim.users.create": scim, "scim.users.get": scim, "scim.users.replace": scim,
  "scim.users.patch": scim, "scim.users.delete": scim, "scim.groups.list": scim, "scim.groups.create": scim,
};
