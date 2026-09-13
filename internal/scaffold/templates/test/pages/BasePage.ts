import type { Page, Response } from '@playwright/test'

/**
 * Base page object. Every service POM extends this. Each POM declares its own
 * `serviceUrl` (from `config.urls.*`) so navigation is absolute and
 * domain-agnostic — no reliance on the project-level `baseURL`, which lets one
 * spec drive multiple services (app + auth + website) in a single flow.
 */
export abstract class BasePage {
  readonly page: Page
  /** Absolute origin for this service, e.g. https://app.example.dev. */
  protected abstract readonly serviceUrl: string

  constructor(page: Page) {
    this.page = page
  }

  /** Build an absolute URL on this page's service for a relative path. */
  protected url(path = '/'): string {
    const base = this.serviceUrl.replace(/\/$/, '')
    return path.startsWith('/') ? `${base}${path}` : `${base}/${path}`
  }

  /**
   * Navigate to a path on this service. Defaults to `domcontentloaded`
   * (not `networkidle`) — auth redirects and SSR keep connections open and can
   * stall `networkidle`.
   */
  async goto(path = '/', waitUntil: 'load' | 'domcontentloaded' | 'commit' = 'domcontentloaded'): Promise<Response | null> {
    return this.page.goto(this.url(path), { waitUntil })
  }

  currentUrl(): string {
    return this.page.url()
  }

  async title(): Promise<string> {
    return this.page.title()
  }

  async bodyText(): Promise<string> {
    return (await this.page.textContent('body')) ?? ''
  }
}
