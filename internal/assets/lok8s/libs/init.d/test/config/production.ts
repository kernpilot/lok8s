import type { EnvPreset } from '../utils/config'

/**
 * Production plane. Smoke-only by default: no mail catcher, no auto-created
 * users, and authenticated journeys require an explicitly supplied real test
 * account (LOK8S_TEST_USER / LOK8S_TEST_PASSWORD) plus LOK8S_TEST_CAN_AUTH=true.
 * This keeps an accidental `npm test` against prod from mutating data or
 * spamming real mailboxes — it degrades to public, read-only checks.
 */
export const productionPreset: EnvPreset = {
  capabilities: {
    canAuth: false,
    hasMailpit: false,
    autoCreateUsers: false,
  },
  oidcClientId: 'app',
}
