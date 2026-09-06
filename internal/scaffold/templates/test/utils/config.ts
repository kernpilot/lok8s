/**
 * Centralized test configuration — the single source of truth for which
 * environment the suite runs against and what every service URL is.
 *
 * ┌──────────────────────────────────────────────────────────────────────────┐
 * │ 3-layer domain parameterization                                            │
 * │   1. Env vars     LOK8S_TEST_DOMAIN / LOK8S_TEST_BASE_URL / _<SVC>_URL      │
 * │   2. Per-env presets  config/<env>.ts  (dev | staging | production)        │
 * │   3. This module  auto-detects the env from the domain, merges env-var      │
 * │                   overrides over the preset, and exposes a per-env URL map  │
 * │                   + capability flags that tests read instead of literals.   │
 * └──────────────────────────────────────────────────────────────────────────┘
 *
 * Tests MUST read `config.urls.*` and capability flags — never hardcode a
 * hostname. The SAME spec then runs against your dev cluster today and a
 * staging / production cluster later by changing only `LOK8S_TEST_DOMAIN`
 * (or the individual URLs).
 *
 * ── Make this yours ──────────────────────────────────────────────────────────
 * The `SERVICES` map below is the ONE place you describe your project's
 * services. Each entry maps a logical role to the subdomain label it lives on
 * (`app` -> app.<domain>, `''` -> the apex). Add/rename/remove rows to match
 * your stack; specs then reference `config.urls.<role>` and pick it up across
 * every environment. The default rows (app/api/auth/website/mail) are a common
 * web-app shape — edit freely.
 */

import { devPreset } from '../config/dev'
import { stagingPreset } from '../config/staging'
import { productionPreset } from '../config/production'

// =============================================================================
// Service catalogue — EDIT ME for your project.
// =============================================================================

/**
 * Logical service role -> subdomain label. `''` means the apex domain itself.
 * This is the project-specific knob; everything else is generic machinery.
 * Override any single URL at runtime with LOK8S_TEST_<ROLE>_URL (uppercased).
 */
export const SERVICES = {
  app: 'app', // primary web app / dashboard
  api: 'api', // backend API
  auth: 'auth', // identity provider (OIDC), if any
  website: '', // marketing / docs site on the apex
  mail: 'mail', // dev mail catcher (e.g. Mailpit), if deployed
} as const

export type ServiceRole = keyof typeof SERVICES
export type ServiceUrls = Record<ServiceRole, string>

// =============================================================================
// Types
// =============================================================================

export type Environment = 'dev' | 'staging' | 'production'

/** A user the suite can authenticate as (browser and/or token flows). */
export interface TestUser {
  username: string
  password: string
  /** Email used for mail-catcher assertions and identity checks. */
  email: string
  displayName?: string
  admin?: boolean
}

/** Capability flags let the SAME spec pass on a full cluster and skip-with-reason where gated. */
export interface Capabilities {
  /** Authenticated journeys (login + bearer-protected API) are exercisable. */
  canAuth: boolean
  /** A dev mail catcher is reachable for email assertions. */
  hasMailpit: boolean
  /** The suite may register/verify throwaway users itself (needs mail + open registration). */
  autoCreateUsers: boolean
}

export interface EnvironmentConfig {
  env: Environment
  /** Bare apex domain, e.g. `example.dev`. */
  domain: string
  urls: ServiceUrls
  testUser: TestUser
  adminUser: TestUser
  capabilities: Capabilities
  /** Optional `username:password` basic-auth guard in front of the mail UI/API. */
  mailpitAuth?: string
  /** OIDC client id used for discovery / token flows (if you have an IdP). */
  oidcClientId: string
}

/** Shape of a per-env preset file in `config/`. Everything optional → env-var/derived defaults win. */
export interface EnvPreset {
  domain?: string
  urls?: Partial<ServiceUrls>
  testUser?: Partial<TestUser>
  adminUser?: Partial<TestUser>
  capabilities?: Partial<Capabilities>
  mailpitAuth?: string
  oidcClientId?: string
}

// =============================================================================
// Environment detection
// =============================================================================

const PRESETS: Record<Environment, EnvPreset> = {
  dev: devPreset,
  staging: stagingPreset,
  production: productionPreset,
}

/**
 * Resolve the bare apex domain from env. Precedence:
 *   LOK8S_TEST_DOMAIN  >  hostname of LOK8S_TEST_BASE_URL  >  'example.dev'
 */
function resolveDomain(): string {
  if (process.env.LOK8S_TEST_DOMAIN) return process.env.LOK8S_TEST_DOMAIN.trim()

  const fromBaseUrl = process.env.LOK8S_TEST_BASE_URL || process.env.BASE_URL
  if (fromBaseUrl) {
    try {
      const host = new URL(fromBaseUrl).hostname
      const parts = host.split('.')
      return parts.length > 2 ? parts.slice(1).join('.') : host
    }
    catch {
      // fall through
    }
  }
  return 'example.dev'
}

/** Map a domain to a logical environment. `.dev` ⇒ dev; explicit `LOK8S_TEST_ENV` always wins. */
function detectEnvironment(domain: string): Environment {
  const explicit = process.env.LOK8S_TEST_ENV?.toLowerCase()
  if (explicit === 'dev' || explicit === 'staging' || explicit === 'production') {
    return explicit
  }
  if (domain.endsWith('.dev') || domain === 'localhost') return 'dev'
  if (domain.includes('staging') || domain.includes('stg')) return 'staging'
  return 'production'
}

// =============================================================================
// URL derivation
// =============================================================================

function envUrlOverride(role: ServiceRole): string | undefined {
  return process.env[`LOK8S_TEST_${role.toUpperCase()}_URL`]
}

/**
 * Derive the full service URL map from the apex domain + the SERVICES catalogue,
 * then layer preset + per-service env-var overrides on top. This is what makes
 * the suite "not hardcoded": every host is `<label>.<domain>` (or the apex)
 * unless explicitly overridden.
 */
function deriveUrls(domain: string, preset: EnvPreset): ServiceUrls {
  const https = (host: string) => `https://${host}`
  const out = {} as ServiceUrls

  for (const role of Object.keys(SERVICES) as ServiceRole[]) {
    const label = SERVICES[role]
    const derived = https(label ? `${label}.${domain}` : domain)
    const withPreset = preset.urls?.[role] ?? derived
    out[role] = envUrlOverride(role) ?? withPreset
  }
  return out
}

// =============================================================================
// Capability resolution
// =============================================================================

function resolveBool(envValue: string | undefined, fallback: boolean): boolean {
  if (envValue === undefined) return fallback
  return envValue === 'true' || envValue === '1'
}

function resolveCapabilities(preset: EnvPreset): Capabilities {
  const presetCaps = preset.capabilities ?? {}

  // A token (or dev-bypass) being present is what unblocks authed flows.
  const tokenPresent = !!process.env.LOK8S_TEST_TOKEN || process.env.LOK8S_TEST_AUTH_DEV === 'true'

  const hasMailpit = resolveBool(process.env.LOK8S_TEST_HAS_MAILPIT, presetCaps.hasMailpit ?? false)

  return {
    canAuth: resolveBool(process.env.LOK8S_TEST_CAN_AUTH, (presetCaps.canAuth ?? false) || tokenPresent),
    hasMailpit,
    autoCreateUsers: resolveBool(
      process.env.LOK8S_TEST_AUTO_CREATE_USERS,
      (presetCaps.autoCreateUsers ?? false) && hasMailpit,
    ),
  }
}

// =============================================================================
// Build the active config
// =============================================================================

function buildConfig(): EnvironmentConfig {
  const domain = resolveDomain()
  const env = detectEnvironment(domain)
  const preset = PRESETS[env]

  const urls = deriveUrls(domain, preset)

  const testUser: TestUser = {
    ...(preset.testUser ?? {}),
    username: process.env.LOK8S_TEST_USER || preset.testUser?.username || 'admin',
    password: process.env.LOK8S_TEST_PASSWORD || preset.testUser?.password || 'admin',
    email: process.env.LOK8S_TEST_EMAIL || preset.testUser?.email || `test-${env}@${domain}`,
  }

  const adminUser: TestUser = {
    ...(preset.adminUser ?? {}),
    username: process.env.LOK8S_TEST_ADMIN_USER || preset.adminUser?.username || 'admin',
    password: process.env.LOK8S_TEST_ADMIN_PASSWORD || preset.adminUser?.password || 'admin',
    email: process.env.LOK8S_TEST_ADMIN_EMAIL || preset.adminUser?.email || `admin-${env}@${domain}`,
    admin: true,
  }

  return {
    env,
    domain,
    urls,
    testUser,
    adminUser,
    capabilities: resolveCapabilities(preset),
    mailpitAuth: process.env.LOK8S_TEST_MAILPIT_AUTH || preset.mailpitAuth,
    oidcClientId: process.env.LOK8S_TEST_OIDC_CLIENT_ID || preset.oidcClientId || 'app',
  }
}

// =============================================================================
// Exports
// =============================================================================

/** The active, fully-resolved configuration for this run. */
export const config: EnvironmentConfig = buildConfig()

/** Convenience: current environment name. */
export const currentEnvironment: Environment = config.env

/** Convenience: capability flags (read these to gate specs). */
export const caps: Capabilities = config.capabilities

/**
 * OIDC discovery URL. Many providers serve discovery at the issuer ROOT
 * (`${issuer}/.well-known/openid-configuration`). Adjust if your IdP differs.
 */
export function oidcDiscoveryUrl(): string {
  return `${config.urls.auth.replace(/\/$/, '')}/.well-known/openid-configuration`
}

/** A guaranteed-unique throwaway email on the active domain (routes to the mail catcher). */
export function uniqueEmail(prefix = 'pw'): string {
  return `${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}@${config.domain}`
}
