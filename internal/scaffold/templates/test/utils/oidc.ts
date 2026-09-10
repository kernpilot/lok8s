/**
 * OIDC helpers (optional — only relevant if your stack has an IdP).
 *
 * Provides:
 *   - OIDC discovery fetch/validation (most providers serve it at the issuer
 *     root `${issuer}/.well-known/openid-configuration`);
 *   - `resolveTestToken()` — the bearer the api-client uses for protected calls,
 *     resolved (in order) from LOK8S_TEST_TOKEN, then a token captured by the
 *     browser-login setup (setup/auth.setup.ts writes it to TOKEN_FILE).
 *
 * Adjust the discovery/JWKS paths if your provider differs.
 */

import { existsSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { type APIRequestContext } from '@playwright/test'
import { config, oidcDiscoveryUrl } from './config'
import './tls'

/**
 * Where setup/auth.setup.ts persists the access token captured from the browser
 * login, so the (otherwise headless) API specs can send a real bearer. Lives
 * under the gitignored test-results dir. Override with LOK8S_TEST_TOKEN_FILE.
 */
export const TOKEN_FILE = process.env.LOK8S_TEST_TOKEN_FILE
  ?? join(process.cwd(), 'test-results', '.auth', 'token.txt')

/** Subset of the OIDC discovery document worth asserting on / using. */
export interface OIDCDiscoveryDocument {
  issuer: string
  authorization_endpoint: string
  token_endpoint: string
  jwks_uri: string
  response_types_supported: string[]
  id_token_signing_alg_values_supported: string[]
  [k: string]: unknown
}

/**
 * Fetch + validate the OIDC discovery document. Uses a Playwright
 * `APIRequestContext` when provided (so it inherits `ignoreHTTPSErrors`);
 * otherwise falls back to native fetch (TLS handled by utils/tls).
 */
export async function fetchOIDCDiscovery(
  request?: APIRequestContext,
  url: string = oidcDiscoveryUrl(),
): Promise<OIDCDiscoveryDocument> {
  let body: unknown
  if (request) {
    const res = await request.get(url)
    if (!res.ok()) throw new Error(`OIDC discovery ${res.status()} at ${url}`)
    body = await res.json()
  }
  else {
    const res = await fetch(url)
    if (!res.ok) throw new Error(`OIDC discovery ${res.status} at ${url}`)
    body = await res.json()
  }

  const doc = body as OIDCDiscoveryDocument
  for (const field of ['issuer', 'authorization_endpoint', 'token_endpoint', 'jwks_uri'] as const) {
    if (!doc[field]) throw new Error(`OIDC discovery missing required field: ${field}`)
  }
  return doc
}

/** Build an Authorization header for a bearer token. */
export function bearer(token: string): { Authorization: string } {
  return { Authorization: `Bearer ${token}` }
}

/**
 * Resolve a usable bearer for protected API tests, in precedence order:
 *   1. LOK8S_TEST_TOKEN env (explicit, e.g. a long-lived service token)
 *   2. TOKEN_FILE — the access token setup/auth.setup.ts captured from the
 *      browser login (the `api` project depends on `auth-setup`, so the file
 *      exists by the time authed API specs run, when canAuth).
 * Returns '' when none is available — callers should gate on `caps.canAuth`.
 *
 * Read lazily (not cached at import) because the api-client constructs its
 * default at module load, which can precede auth.setup writing the file.
 */
export function resolveTestToken(): string {
  if (process.env.LOK8S_TEST_TOKEN) return process.env.LOK8S_TEST_TOKEN
  try {
    if (existsSync(TOKEN_FILE)) {
      const tok = readFileSync(TOKEN_FILE, 'utf8').trim()
      if (tok) return tok
    }
  }
  catch {
    // no captured token — fall through to empty
  }
  return ''
}
