/**
 * Local host resolution for a dev cluster (`*.<domain>` -> the gateway LB IP).
 *
 * WHY THIS EXISTS
 * ---------------
 * A local (kind) cluster's ingress/gateway LoadBalancer is reachable on a
 * private pool IP (e.g. a metallb address) that is routable from your host but
 * is NOT a public address, and `*.<domain>` may not resolve publicly. The two
 * options are (a) an /etc/hosts entry, or (b) per-process resolution overrides.
 * On machines/sandboxes where /etc/hosts is not writable, (b) is the only path —
 * and it is the more hermetic choice (no host-level mutation, no dependency on
 * external DNS having the right records).
 *
 * This module is import-for-side-effect and idempotent. It patches Node's
 * `dns.lookup` (and `dns.promises.lookup`) so native `fetch`/undici and every
 * http helper in the suite resolve dev hosts to the LB IP. The BROWSER runs in
 * a separate process and is handled instead by `--host-resolver-rules` in
 * playwright.config.ts (see hostResolverRule() below). NO-OP outside the dev
 * environment (real DNS is authoritative).
 *
 * Set the target IP with LOK8S_TEST_LB_IP. With no LB IP set and no override,
 * this is a NO-OP (relies on real DNS / /etc/hosts). Disable explicitly with
 * LOK8S_TEST_NO_RESOLVER=1.
 */

import dns from 'node:dns'
import { config } from './config'

/** The gateway/LB IP every `*.<domain>` name maps to in dev (empty = disabled). */
export function lbIp(): string {
  return process.env.LOK8S_TEST_LB_IP || ''
}

function enabled(): boolean {
  return config.env === 'dev' && process.env.LOK8S_TEST_NO_RESOLVER !== '1' && !!lbIp()
}

/**
 * The Chromium `--host-resolver-rules` value mapping the dev domain (apex +
 * wildcard) to the LB IP. Returned for playwright.config.ts to feed into the
 * browser projects' launchOptions.args. Empty string when disabled.
 */
export function hostResolverRule(): string {
  if (!enabled()) return ''
  const ip = lbIp()
  return `MAP ${config.domain} ${ip},MAP *.${config.domain} ${ip}`
}

declare global {
  // eslint-disable-next-line no-var
  var __lok8sResolverConfigured: boolean | undefined
}

function matchesDevDomain(hostname: string): boolean {
  const d = config.domain
  return hostname === d || hostname.endsWith(`.${d}`)
}

function configureResolver(): void {
  if (globalThis.__lok8sResolverConfigured) return
  globalThis.__lok8sResolverConfigured = true

  if (!enabled()) return

  const ip = lbIp()

  // Patch the callback-style dns.lookup (what undici/native fetch use under the
  // hood). Map dev hosts to the LB IP (IPv4); delegate everything else.
  const originalLookup = dns.lookup.bind(dns)
  // @ts-expect-error — overloaded signature; we forward unmatched calls verbatim.
  dns.lookup = (hostname: string, options: unknown, callback?: unknown) => {
    const cb = (typeof options === 'function' ? options : callback) as
      | ((err: NodeJS.ErrnoException | null, address: string | dns.LookupAddress[], family?: number) => void)
      | undefined
    if (matchesDevDomain(hostname) && cb) {
      const all = typeof options === 'object' && options !== null && (options as dns.LookupOptions).all
      if (all) {
        cb(null, [{ address: ip, family: 4 }] as dns.LookupAddress[])
      }
      else {
        cb(null, ip as unknown as dns.LookupAddress[], 4)
      }
      return
    }
    // @ts-expect-error — forward to the original overload unchanged.
    return originalLookup(hostname, options, callback)
  }

  // Patch the promise variant too (helpers that call dns.promises.lookup).
  const originalPromisesLookup = dns.promises.lookup.bind(dns.promises)
  // @ts-expect-error — overloaded signature.
  dns.promises.lookup = async (hostname: string, options?: dns.LookupOptions) => {
    if (matchesDevDomain(hostname)) {
      return options?.all ? [{ address: ip, family: 4 }] : { address: ip, family: 4 }
    }
    // @ts-expect-error — forward unchanged.
    return originalPromisesLookup(hostname, options)
  }
}

configureResolver()

export {}
