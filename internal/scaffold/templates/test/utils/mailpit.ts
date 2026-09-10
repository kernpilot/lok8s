/**
 * Mailpit helpers — provider-agnostic email assertions.
 *
 * Mailpit (https://mailpit.axllent.org) is a common dev SMTP sink + REST API.
 * Point `config.urls.mail` at it (e.g. https://mail.<domain>) and your app's
 * notifications (password reset, verification, etc.) become assertable.
 *
 * These helpers:
 *   - poll `/api/v1/messages`, filtered by recipient + a `minTimestamp` so
 *     parallel tests never pick up each other's (or stale) mail;
 *   - fetch full content via `/api/v1/message/{id}`;
 *   - support a basic-auth guard in front of Mailpit (config.mailpitAuth);
 *   - clean the mailbox between runs.
 *
 * All requests use native `fetch`. For self-signed local TLS, the suite loads a
 * local CA into NODE_EXTRA_CA_CERTS (see utils/tls.ts).
 */

import { config } from './config'
import './tls' // side-effect: trust local CA for native fetch

// =============================================================================
// Types (Mailpit API v1)
// =============================================================================

export interface MailpitAddress {
  Name?: string
  Address: string
}

/** A message summary as returned by `GET /api/v1/messages`. */
export interface MailpitMessage {
  ID: string
  MessageID?: string
  From: MailpitAddress
  To: MailpitAddress[]
  Cc?: MailpitAddress[]
  Subject: string
  /** RFC3339 timestamp. */
  Created: string
  Snippet: string
}

interface MailpitMessagesResponse {
  messages: MailpitMessage[]
  total: number
  count: number
}

/** Full content as returned by `GET /api/v1/message/{id}`. */
export interface MailpitMessageContent {
  ID: string
  Subject: string
  From: MailpitAddress
  To: MailpitAddress[]
  HTML?: string
  Text?: string
  Date?: string
}

// =============================================================================
// Internals
// =============================================================================

function requireMailpitUrl(): string {
  const url = config.urls.mail
  if (!url) {
    throw new Error(
      'Mailpit is not configured for this environment. Gate email tests behind '
      + '`config.capabilities.hasMailpit` with test.skip(!caps.hasMailpit, …).',
    )
  }
  return url.replace(/\/$/, '')
}

/** Build headers, adding Basic auth when a `user:pass` guard is configured. */
function mailpitHeaders(extra: Record<string, string> = {}): Record<string, string> {
  const headers: Record<string, string> = { ...extra }
  if (config.mailpitAuth) {
    headers.Authorization = `Basic ${Buffer.from(config.mailpitAuth).toString('base64')}`
  }
  return headers
}

const sleep = (ms: number) => new Promise<void>(r => setTimeout(r, ms))

// =============================================================================
// Public API
// =============================================================================

/**
 * Poll Mailpit for messages to `recipient`, newest first.
 *
 * @param recipient    Recipient email to filter on (case-insensitive).
 * @param maxWaitMs    Total time to keep polling before giving up (default 30s).
 * @param minTimestamp Only return mail created strictly after this ISO time —
 *                     record it *before* you trigger the email to dodge races.
 * @param pollIntervalMs Delay between polls (default 1s).
 * @returns Matching messages (possibly empty if the deadline passed).
 */
export async function getMailpitMessages(
  recipient: string,
  maxWaitMs = 30_000,
  minTimestamp?: string,
  pollIntervalMs = 1_000,
): Promise<MailpitMessage[]> {
  const base = requireMailpitUrl()
  const deadline = Date.now() + maxWaitMs
  const minDate = minTimestamp ? new Date(minTimestamp) : null
  const wanted = recipient.toLowerCase()

  let lastError: unknown = null

  do {
    try {
      const res = await fetch(`${base}/api/v1/messages?limit=200`, { headers: mailpitHeaders() })
      if (!res.ok) throw new Error(`Mailpit API ${res.status} ${res.statusText}`)

      const data = (await res.json()) as MailpitMessagesResponse
      let messages = data.messages ?? []

      messages = messages.filter(m => m.To?.some(to => to.Address.toLowerCase() === wanted))
      if (minDate) messages = messages.filter(m => new Date(m.Created) > minDate)

      messages.sort((a, b) => new Date(b.Created).getTime() - new Date(a.Created).getTime())

      if (messages.length > 0) return messages
    }
    catch (err) {
      lastError = err
    }

    if (Date.now() >= deadline) break
    await sleep(pollIntervalMs)
  } while (Date.now() < deadline)

  if (lastError) {
    // eslint-disable-next-line no-console
    console.warn(`Mailpit poll for <${recipient}> ended after errors:`, lastError)
  }
  return []
}

/**
 * Wait for the FIRST message to a recipient (optionally matching a subject
 * substring/regex). Returns null on timeout — callers decide whether that's a
 * skip or a failure.
 */
export async function waitForMessage(
  recipient: string,
  opts: { maxWaitMs?: number; minTimestamp?: string; subject?: string | RegExp } = {},
): Promise<MailpitMessage | null> {
  const { maxWaitMs = 30_000, minTimestamp, subject } = opts
  const messages = await getMailpitMessages(recipient, maxWaitMs, minTimestamp)
  if (messages.length === 0) return null
  if (!subject) return messages[0] ?? null

  const matcher = (s: string) =>
    subject instanceof RegExp ? subject.test(s) : s.toLowerCase().includes(subject.toLowerCase())
  return messages.find(m => matcher(m.Subject)) ?? null
}

/** Fetch full message content (prefers HTML, falls back to Text). */
export async function getMailpitMessageContent(id: string): Promise<string> {
  const base = requireMailpitUrl()
  const res = await fetch(`${base}/api/v1/message/${id}`, { headers: mailpitHeaders() })
  if (!res.ok) throw new Error(`Mailpit message ${id}: ${res.status} ${res.statusText}`)
  const data = (await res.json()) as MailpitMessageContent
  return data.HTML || data.Text || ''
}

/** Extract the first actionable link from email HTML/text (prefers links to `host`). */
export function extractActionLink(content: string, host = config.urls.auth): string | null {
  const esc = host.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const pattern = new RegExp(`${esc}[^"'\\s<>)]*`, 'i')
  const match = content.match(pattern)
  if (match) return match[0]

  const anyUrl = content.match(/https?:\/\/[^"'\s<>)]+/i)
  return anyUrl ? anyUrl[0] : null
}

/** Pull a numeric OTP / verification code (default 6 digits) out of email content. */
export function extractCode(content: string, digits = 6): string | null {
  const re = new RegExp(`\\b(\\d{${digits}})\\b`)
  const m = content.match(re)
  return m ? m[1]! : null
}

/** Delete all messages from Mailpit. Best-effort — never throws. */
export async function cleanupMailpit(): Promise<void> {
  const url = config.urls.mail
  if (!url) return
  try {
    await fetch(`${url.replace(/\/$/, '')}/api/v1/messages`, {
      method: 'DELETE',
      headers: mailpitHeaders(),
    })
  }
  catch {
    // Cleanup is advisory; swallow transport errors.
  }
}

/** Quick reachability probe (used by the global setup to report Mailpit status). */
export async function isMailpitReachable(): Promise<boolean> {
  const url = config.urls.mail
  if (!url) return false
  try {
    const res = await fetch(`${url.replace(/\/$/, '')}/api/v1/messages?limit=1`, {
      headers: mailpitHeaders(),
      signal: AbortSignal.timeout(5_000),
    })
    return res.ok
  }
  catch {
    return false
  }
}
