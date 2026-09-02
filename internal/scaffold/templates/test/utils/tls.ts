/**
 * Local self-signed TLS trust for native Node `fetch`.
 *
 * Playwright's request/browser contexts honour `ignoreHTTPSErrors`, but raw
 * `fetch()` (used by the mail + OIDC helpers) does not — it consults Node's
 * trust store. On a local cluster every `*.<domain>` cert is typically issued by
 * a locally-trusted CA (e.g. mkcert), so we point NODE_EXTRA_CA_CERTS at that
 * root. If we can't locate one, we fall back to disabling verification (dev only).
 *
 * This module is import-for-side-effect and idempotent — importing it from any
 * helper guarantees fetch trusts the dev plane. It is a NO-OP outside the dev
 * environment (real certs are publicly trusted).
 *
 * Point LOK8S_TEST_CA at a CA bundle to use it directly; otherwise this tries
 * `mkcert -CAROOT`/rootCA.pem.
 */

import { execSync } from 'node:child_process'
import { existsSync } from 'node:fs'
import { join } from 'node:path'
import { config } from './config'
// Side-effect: map `*.<domain>` -> the gateway LB IP for native fetch/undici on
// the dev plane. NO-OP outside dev. The browser is handled separately via
// --host-resolver-rules in playwright.config.ts. See utils/resolver.ts.
import './resolver'

declare global {
  // eslint-disable-next-line no-var
  var __lok8sTlsConfigured: boolean | undefined
}

function configureTls(): void {
  if (globalThis.__lok8sTlsConfigured) return
  globalThis.__lok8sTlsConfigured = true

  // Real environments use publicly-trusted certs — nothing to do.
  if (config.env !== 'dev') return

  // Respect an explicit operator-provided CA bundle.
  const explicit = process.env.LOK8S_TEST_CA || process.env.NODE_EXTRA_CA_CERTS
  if (explicit && existsSync(explicit)) {
    process.env.NODE_EXTRA_CA_CERTS = explicit
    return
  }

  // Try mkcert's CAROOT (a common local dev CA).
  const candidates = [
    join(process.cwd(), '..', '.bin', 'mkcert'),
    join(process.cwd(), '.bin', 'mkcert'),
    'mkcert',
  ]

  for (const bin of candidates) {
    try {
      const caRoot = execSync(`"${bin}" -CAROOT`, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim()
      const caPem = join(caRoot, 'rootCA.pem')
      if (existsSync(caPem)) {
        process.env.NODE_EXTRA_CA_CERTS = caPem
        return
      }
    }
    catch {
      // try next candidate
    }
  }

  // Last resort for local dev: trust everything. NEVER reached for non-dev.
  if (!process.env.NODE_TLS_REJECT_UNAUTHORIZED) {
    process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0'
  }
}

configureTls()

export {}
