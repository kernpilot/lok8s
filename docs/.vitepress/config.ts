import { defineConfig } from 'vitepress'

export default defineConfig({
  title: 'lok8s',
  description: 'Kubernetes deployment framework — same workflow from local dev to production',
  // Project pages live at kernpilot.github.io/lok8s/
  base: '/lok8s/',
  cleanUrls: true,
  lastUpdated: true,
  sitemap: {
    hostname: 'https://kernpilot.github.io/lok8s/',
  },
  // Internal/planning docs now live in the PRIVATE kubehz-cluster repo
  // (docs/internal/). lok8s is public — keep only this defensive exclude so a
  // stray internal/** file can never ship to the public site by accident.
  srcExclude: ['internal/**'],
  head: [
    ['meta', { name: 'theme-color', content: '#89DCEB' }],
    ['link', { rel: 'icon', type: 'image/svg+xml', href: '/lok8s/logo.svg' }],
    ['meta', { property: 'og:title', content: 'lok8s — Kubernetes from laptop to production' }],
    ['meta', { property: 'og:description', content: 'One CLI, one folder convention, the same workflow from local kind clusters to bare-metal production.' }],
  ],
  themeConfig: {
    logo: '/logo.svg',
    editLink: {
      pattern: 'https://github.com/kernpilot/lok8s/edit/main/docs/:path',
      text: 'Edit this page on GitHub',
    },
    footer: {
      message: 'Released under the MIT License.',
      copyright: 'Copyright © 2025-present kernpilot',
    },
    outline: { level: [2, 3] },
    nav: [
      { text: 'Guide', link: '/guide/' },
      { text: 'Reference', link: '/reference/cli' },
      {
        text: 'GitHub',
        link: 'https://github.com/kernpilot/lok8s',
      },
    ],
    sidebar: {
      '/guide/': [
        {
          text: 'Introduction',
          items: [
            { text: 'Getting Started', link: '/guide/' },
            { text: 'Concepts', link: '/guide/concepts' },
            { text: 'Addons', link: '/guide/addons' },
          ],
        },
        {
          text: 'Workflows',
          items: [
            { text: 'Local Dev with Tilt', link: '/guide/local-dev' },
            { text: 'Services Configuration', link: '/guide/services' },
            { text: 'Testing', link: '/guide/testing' },
            { text: 'Secrets', link: '/guide/secrets' },
            { text: 'Shared Registries', link: '/guide/shared-registries' },
            { text: 'Deploying to Clusters', link: '/guide/deployment' },
          ],
        },
        {
          text: 'Production',
          items: [
            { text: 'CAPI Clusters', link: '/guide/capi' },
            { text: 'Bare Metal (Hetzner Robot)', link: '/guide/bare-metal' },
            { text: 'Cloud-Init', link: '/guide/cloud-init' },
            { text: 'Networking & Ingress', link: '/guide/networking' },
            { text: 'Security', link: '/guide/security' },
            { text: 'Backups', link: '/guide/backups' },
            { text: 'The Operator', link: '/guide/operator' },
          ],
        },
      ],
      '/reference/': [
        {
          text: 'Reference',
          items: [
            { text: 'CLI', link: '/reference/cli' },
            { text: 'Spec Files', link: '/reference/specs' },
            { text: 'services.yaml + lok8s.yaml Schema', link: '/reference/schema' },
            { text: 'Kind Contract', link: '/reference/kind-contract' },
            { text: 'Kustomize Plugins', link: '/reference/kustomize-plugins' },
          ],
        },
      ],
    },
    search: {
      provider: 'local',
    },
    socialLinks: [
      { icon: 'github', link: 'https://github.com/kernpilot/lok8s' },
    ],
  },
})
