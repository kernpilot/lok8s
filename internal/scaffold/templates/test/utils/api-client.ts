/**
 * Typed HTTP client SKELETON for your backend API (`config.urls.api`).
 *
 * This is a starting point — add a typed method per endpoint you test. The
 * low-level `get/post/delete/patch` return the raw `Response` so specs can
 * assert on status codes (401/404/…); add typed convenience wrappers for the
 * happy paths (see `health()` below as the worked example).
 *
 * Uses native `fetch` with the dev CA trusted via utils/tls and the dev-host
 * resolver applied via utils/resolver. Carries a bearer when one is resolvable
 * (see utils/oidc resolveTestToken — env token or the captured login token).
 */

import { config } from './config'
import { resolveTestToken } from './oidc'
import './tls'

// =============================================================================
// Response shapes — extend for your API.
// =============================================================================

export interface HealthResponse {
  status: string
  [k: string]: unknown
}

// =============================================================================
// Client
// =============================================================================

export class ApiClient {
  readonly baseUrl: string
  private token: string

  constructor(baseUrl: string = config.urls.api, token: string = resolveTestToken()) {
    this.baseUrl = baseUrl.replace(/\/$/, '')
    this.token = token
  }

  /** Set/replace the bearer used for protected calls. */
  withToken(token: string): this {
    this.token = token
    return this
  }

  hasToken(): boolean {
    return !!this.token
  }

  private headers(json = true): Record<string, string> {
    const h: Record<string, string> = { Accept: 'application/json' }
    if (json) h['Content-Type'] = 'application/json'
    if (this.token) h.Authorization = `Bearer ${this.token}`
    return h
  }

  /** Low-level GET returning the raw Response (for status-code assertions). */
  get(path: string): Promise<Response> {
    return fetch(`${this.baseUrl}${path}`, { headers: this.headers(false) })
  }

  /** Low-level POST returning the raw Response. */
  post(path: string, body?: unknown): Promise<Response> {
    return fetch(`${this.baseUrl}${path}`, {
      method: 'POST',
      headers: this.headers(),
      body: body === undefined ? undefined : JSON.stringify(body),
    })
  }

  delete(path: string): Promise<Response> {
    return fetch(`${this.baseUrl}${path}`, { method: 'DELETE', headers: this.headers(false) })
  }

  patch(path: string, body: unknown): Promise<Response> {
    return fetch(`${this.baseUrl}${path}`, {
      method: 'PATCH',
      headers: this.headers(),
      body: JSON.stringify(body),
    })
  }

  // --- Worked example: a typed health endpoint. Replace with your endpoints. --

  async health(): Promise<HealthResponse> {
    const res = await this.get('/api/health')
    return res.json() as Promise<HealthResponse>
  }
}

/** Build a fresh client for the active API URL (optionally with an explicit token). */
export function apiClient(token?: string): ApiClient {
  return new ApiClient(config.urls.api, token ?? resolveTestToken())
}
