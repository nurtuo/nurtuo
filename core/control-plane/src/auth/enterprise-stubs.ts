// SPDX-License-Identifier: AGPL-3.0-only
// SAML 2.0 SSO and SCIM 2.0 provisioning: interfaces only. The routes exist (so clients and
// IdP admins get a clear answer and openapi.yaml is complete) but return 501. SAML signature
// validation (XML-DSig + canonicalisation) is deliberately NOT implemented here; see docs/auth.md.
import type { ApiResponse } from "../api.js";

/** What a future SAML service-provider implementation must provide. */
export interface SamlProvider {
  /** SP metadata XML (entityID, ACS URL, signing/encryption certs). */
  metadata(): Promise<string>;
  /** Builds the AuthnRequest redirect. RelayState must be signed/bound to the browser like the OIDC state. */
  beginLogin(relayState: string): Promise<{ redirectUrl: string }>;
  /** Validates the posted SAMLResponse (signature, audience, conditions, InResponseTo, replay) and returns the asserted identity. */
  consumeAssertion(samlResponse: string, relayState: string): Promise<{ issuer: string; nameId: string; email?: string; groups?: string[] }>;
}

export interface ScimUser { id: string; userName: string; active: boolean; emails?: Array<{ value: string; primary?: boolean }>; externalId?: string }
/** What a future SCIM 2.0 backend must provide (RFC 7643/7644). Authenticated by a dedicated SCIM bearer token. */
export interface ScimBackend {
  listUsers(filter: string | undefined, startIndex: number, count: number): Promise<{ total: number; users: ScimUser[] }>;
  getUser(id: string): Promise<ScimUser | undefined>;
  createUser(u: Omit<ScimUser, "id">): Promise<ScimUser>;
  replaceUser(id: string, u: Omit<ScimUser, "id">): Promise<ScimUser | undefined>;
  patchUser(id: string, ops: unknown[]): Promise<ScimUser | undefined>;
  /** Deprovisioning must disable the user, revoke sessions and API tokens, but keep audit history. */
  deleteUser(id: string): Promise<boolean>;
}

export function notImplemented(feature: "saml" | "scim"): ApiResponse {
  const scim = feature === "scim";
  return {
    status: 501,
    headers: scim ? { "content-type": "application/scim+json" } : undefined,
    body: scim
      ? { schemas: ["urn:ietf:params:scim:api:messages:2.0:Error"], status: "501", detail: "SCIM provisioning is not implemented yet (see docs/auth.md)" }
      : { error: { code: "not_implemented", message: "SAML SSO is not implemented yet (see docs/auth.md); use OIDC" } },
  };
}
