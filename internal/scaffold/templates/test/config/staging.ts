import type { EnvPreset } from '../utils/config'

/**
 * Staging / pre-prod plane. No fixed domain is assumed — set LOK8S_TEST_DOMAIN
 * and the URL map derives from it. Capabilities default OFF and are opt-in via
 * env (LOK8S_TEST_CAN_AUTH=true / LOK8S_TEST_HAS_MAILPIT=true /
 * LOK8S_TEST_TOKEN=…) so the same suite runs read-only until creds are wired in.
 * Never check in staging credentials — supply them via env.
 */
export const stagingPreset: EnvPreset = {
  capabilities: {
    canAuth: false,
    hasMailpit: false,
    autoCreateUsers: false,
  },
  oidcClientId: 'app',
}
