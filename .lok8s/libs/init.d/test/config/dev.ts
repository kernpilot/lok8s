import type { EnvPreset } from '../utils/config'

/**
 * Local dev cluster (kind, `*.<domain>` behind your ingress/gateway).
 *
 * Capabilities default ON for a full local stack; flip per-flag via env
 * (LOK8S_TEST_CAN_AUTH / LOK8S_TEST_HAS_MAILPIT) as your stack comes up.
 * Admin credentials are the dev defaults below (override via env).
 */
export const devPreset: EnvPreset = {
  capabilities: {
    canAuth: false, // set true once your login + token issuance works in dev
    hasMailpit: false, // set true once a mail catcher is deployed at mail.<domain>
    autoCreateUsers: false,
  },
  testUser: {
    username: 'admin',
    password: 'admin',
  },
  adminUser: {
    username: 'admin',
    password: 'admin',
  },
  oidcClientId: 'app',
}
