<script setup lang="ts">
// Fully custom landing page in the logo's hand-inked language.
// Structure: full-viewport intro with the self-drawing ensō →
// the workflow loop → terminal demo → typographic capability list →
// closing CTA. No stock VitePress home components.
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { withBase } from 'vitepress'
import EnsoHero from './EnsoHero.vue'
import TerminalDemo from './TerminalDemo.vue'
import WorkflowLoop from './WorkflowLoop.vue'

const copied = ref(false)
const INSTALL = 'curl -fsSL https://get.lok8s.io | sh'

async function copyInstall() {
  try {
    await navigator.clipboard.writeText(INSTALL)
    copied.value = true
    setTimeout(() => (copied.value = false), 1600)
  } catch {}
}

// reveal-on-scroll for sections
let observer: IntersectionObserver | undefined
onMounted(() => {
  // the landing has no sidebar — its mobile local-nav strip ("Return
  // to top") is wasted chrome; the floating arrow covers that job
  document.documentElement.classList.add('page-home')
  if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
    document.querySelectorAll('.reveal').forEach((el) => el.classList.add('in'))
    return
  }
  observer = new IntersectionObserver(
    (entries) => {
      for (const e of entries) {
        if (e.isIntersecting) {
          e.target.classList.add('in')
          observer?.unobserve(e.target)
        }
      }
    },
    { threshold: 0.18 },
  )
  document.querySelectorAll('.reveal').forEach((el) => observer!.observe(el))
})
onBeforeUnmount(() => {
  observer?.disconnect()
  document.documentElement.classList.remove('page-home')
})

const capabilities = [
  { k: 'drivers', text: 'kind for local & CI · Cluster API · KubeOne · KKP' },
  { k: 'bootstrap', text: 'ordered addon waves with health gates — Cilium, MetalLB, yours' },
  { k: 'dev loop', text: 'Tilt live-reload, pull-through mirrors, trusted local TLS' },
  { k: 'secrets', text: 'deterministic kustomize generator — cache-first, SHA-pinned' },
  { k: 'bare metal', text: 'Hetzner Robot via rescue + installimage, next to cloud VMs' },
  { k: 'agents', text: 'every command doubles as an MCP tool with safety annotations' },
]
</script>

<template>
  <div class="landing">
    <!-- oversized cropped ink arcs in the page background -->
    <svg class="bg-arc a1" viewBox="0 0 600 600" fill="none" aria-hidden="true">
      <circle cx="300" cy="300" r="280" />
      <circle cx="304" cy="297" r="270" />
    </svg>
    <svg class="bg-arc a2" viewBox="0 0 600 600" fill="none" aria-hidden="true">
      <circle cx="300" cy="300" r="280" />
    </svg>

    <!-- ── intro ─────────────────────────────────────── -->
    <section class="intro">
      <div class="intro-text">
        <p class="kicker">kubernetes deployment framework</p>
        <h1>
          <span class="row">From laptop</span>
          <span class="row">to production.</span>
          <span class="row accent">One loop.</span>
        </h1>
        <p class="sub">
          One CLI, one folder convention — the same commands carry a cluster
          from local kind to bare-metal production. No rewrite at the border.
        </p>
        <div class="cta">
          <a class="btn ink" :href="withBase('/guide/')">Get started</a>
          <a class="btn ghost" :href="withBase('/guide/concepts')">Concepts</a>
          <a class="btn ghost" href="https://github.com/kernpilot/lok8s" target="_blank" rel="noreferrer">GitHub</a>
        </div>
        <button class="install" type="button" :title="copied ? 'Copied' : 'Copy'" @click="copyInstall">
          <span class="ps1">$</span> {{ INSTALL }}
          <span class="copy">{{ copied ? '✓ copied' : 'copy' }}</span>
        </button>
      </div>
      <div class="intro-mark">
        <EnsoHero />
      </div>
      <a class="scroll-cue" href="#loop" aria-label="Scroll to the workflow loop">
        <svg viewBox="0 0 24 16" fill="none"><path d="M3 4 c4 3 7 6 9 8 c2 -2 5 -5 9 -8" /></svg>
      </a>
    </section>

    <!-- ── the loop ──────────────────────────────────── -->
    <section id="loop" class="block reveal">
      <h2>The loop is the product</h2>
      <p class="lead">
        A cluster is a folder named by its FQDN. The folder doesn't care where
        it runs — only the driver changes.
      </p>
      <WorkflowLoop />
    </section>

    <!-- ── terminal ──────────────────────────────────── -->
    <section class="block reveal">
      <h2>See it run</h2>
      <p class="lead">Docker is the only prerequisite — the installer pulls the rest.</p>
      <TerminalDemo />
    </section>

    <!-- ── capabilities, typographic ─────────────────── -->
    <section class="block reveal">
      <h2>What's in the box</h2>
      <ul class="caps">
        <li v-for="c in capabilities" :key="c.k">
          <span class="cap-k">{{ c.k }}</span>
          <span class="cap-t">{{ c.text }}</span>
        </li>
      </ul>
    </section>

    <!-- ── close ─────────────────────────────────────── -->
    <section class="block last reveal">
      <svg class="mini-enso close-enso-anchor" viewBox="0 0 100 100" fill="none" aria-hidden="true">
        <path d="M 79 35 A 31 31 0 1 0 83 55" />
        <path d="M 74 32 A 27 26 0 1 0 79 58" />
      </svg>
      <h2>Close the loop.</h2>
      <p class="lead">
        Start on your laptop today. Keep the folder when it's time for real
        servers.
      </p>
      <div class="cta center">
        <a class="btn ink" :href="withBase('/guide/')">Get started</a>
        <a class="btn ghost" :href="withBase('/reference/cli')">CLI reference</a>
      </div>
    </section>
  </div>
</template>

<style scoped>
.landing {
  position: relative;
  overflow: hidden;
  /* paper grain */
  background-image: url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='180' height='180'%3E%3Cfilter id='n'%3E%3CfeTurbulence type='fractalNoise' baseFrequency='0.9' numOctaves='2' stitchTiles='stitch'/%3E%3CfeColorMatrix type='saturate' values='0'/%3E%3CfeComponentTransfer%3E%3CfeFuncA type='linear' slope='0.05'/%3E%3C/feComponentTransfer%3E%3C/filter%3E%3Crect width='100%25' height='100%25' filter='url(%23n)'/%3E%3C/svg%3E");
}

/* ── background arcs ─────────────────────────────── */
.bg-arc {
  position: absolute;
  pointer-events: none;
}

.bg-arc circle {
  stroke: var(--vp-c-text-1);
  stroke-width: 1.1;
  opacity: 0.06;
}

.a1 {
  width: 920px;
  top: -460px;
  right: -340px;
}

.a2 {
  width: 760px;
  bottom: -420px;
  left: -380px;
}

/* ── intro ───────────────────────────────────────── */
.intro {
  position: relative;
  min-height: calc(100vh - var(--vp-nav-height) - 64px);
  max-width: 1152px;
  margin: 0 auto;
  padding: 48px 24px 72px;
  display: grid;
  grid-template-columns: minmax(0, 1.1fr) minmax(0, 0.9fr);
  align-items: center;
  gap: 24px;
}

.kicker {
  font-family: var(--vp-font-family-mono);
  font-size: 12.5px;
  letter-spacing: 0.14em;
  text-transform: uppercase;
  color: var(--vp-c-brand-1);
  margin: 0 0 14px;
  animation: land-rise 0.5s 0.05s cubic-bezier(0.22, 1, 0.36, 1) backwards;
}

h1 {
  margin: 0;
  font-size: clamp(40px, 6vw, 68px);
  line-height: 1.04;
  font-weight: 750;
  letter-spacing: -0.035em;
  color: var(--vp-c-text-1);
}

h1 .row {
  display: block;
  animation: land-rise 0.55s cubic-bezier(0.22, 1, 0.36, 1) backwards;
}

h1 .row:nth-child(1) { animation-delay: 0.1s; }
h1 .row:nth-child(2) { animation-delay: 0.18s; }
h1 .row:nth-child(3) { animation-delay: 0.26s; }

h1 .accent {
  position: relative;
  width: fit-content;
  color: var(--vp-c-brand-1);
}

/* hand-drawn underline under the accent row */
h1 .accent::after {
  content: '';
  position: absolute;
  left: 2%;
  bottom: -6px;
  width: 96%;
  height: 10px;
  background: url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 200 10' preserveAspectRatio='none'%3E%3Cpath d='M2 7 C 40 3, 90 8, 130 5 S 190 4, 198 6' fill='none' stroke='%2389dceb' stroke-width='2.4' stroke-linecap='round' opacity='0.85'/%3E%3C/svg%3E") no-repeat center / 100% 100%;
}

.sub {
  margin: 22px 0 0;
  max-width: 46ch;
  font-size: 17px;
  line-height: 1.65;
  color: var(--vp-c-text-2);
  animation: land-rise 0.55s 0.34s cubic-bezier(0.22, 1, 0.36, 1) backwards;
}

.cta {
  display: flex;
  flex-wrap: wrap;
  gap: 12px;
  margin-top: 30px;
  animation: land-rise 0.55s 0.42s cubic-bezier(0.22, 1, 0.36, 1) backwards;
}

.cta.center {
  justify-content: center;
  animation: none;
}

/* sketchy ink buttons */
.btn {
  display: inline-block;
  padding: 9px 22px;
  font-size: 14.5px;
  font-weight: 600;
  text-decoration: none;
  color: var(--vp-c-text-1);
  border: 1.7px solid var(--vp-c-text-1);
  border-radius: 255px 18px 225px 18px / 18px 225px 18px 255px;
  transition: transform 0.2s cubic-bezier(0.22, 1, 0.36, 1), background-color 0.2s, color 0.2s, border-color 0.2s;
}

.btn:hover {
  transform: translateY(-1.5px) rotate(-0.4deg);
}

.btn.ink {
  background: var(--vp-c-text-1);
  color: var(--vp-c-bg);
}

.btn.ink:hover {
  background: var(--vp-c-brand-1);
  border-color: var(--vp-c-brand-1);
  color: #16161f;
}

.btn.ghost:hover {
  border-color: var(--vp-c-brand-1);
  color: var(--vp-c-brand-1);
}

.install {
  display: inline-flex;
  align-items: center;
  gap: 10px;
  margin-top: 26px;
  padding: 8px 14px;
  font-family: var(--vp-font-family-mono);
  font-size: 13px;
  color: var(--vp-c-text-2);
  background: var(--vp-c-bg-soft);
  border: 1px dashed color-mix(in srgb, var(--vp-c-text-1) 35%, transparent);
  border-radius: 8px;
  cursor: pointer;
  transition: border-color 0.2s, color 0.2s;
  animation: land-rise 0.55s 0.5s cubic-bezier(0.22, 1, 0.36, 1) backwards;
}

.install:hover {
  border-color: var(--vp-c-brand-1);
  color: var(--vp-c-text-1);
}

.install .ps1 {
  color: var(--vp-c-brand-1);
}

.install .copy {
  font-size: 11px;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--vp-c-text-3);
}

.intro-mark {
  position: relative;
}

.scroll-cue {
  position: absolute;
  left: 50%;
  bottom: 18px;
  transform: translateX(-50%);
  width: 30px;
  opacity: 0.45;
  transition: opacity 0.2s;
  animation: cue-bob 2.6s ease-in-out 1.4s infinite;
}

.scroll-cue:hover { opacity: 0.9; }

.scroll-cue path {
  stroke: var(--vp-c-text-1);
  stroke-width: 1.8;
  stroke-linecap: round;
}

/* ── shared section styles ───────────────────────── */
.block {
  position: relative;
  max-width: 1024px;
  margin: 0 auto;
  padding: 84px 24px 24px;
  text-align: center;
}

.block.last {
  padding-bottom: 110px;
}

.block h2 {
  margin: 0 0 10px;
  font-size: clamp(26px, 3.4vw, 34px);
  font-weight: 700;
  letter-spacing: -0.025em;
  color: var(--vp-c-text-1);
}

.lead {
  margin: 0 auto 40px;
  max-width: 56ch;
  color: var(--vp-c-text-2);
  line-height: 1.65;
}

.lead code {
  font-family: var(--vp-font-family-mono);
  font-size: 0.92em;
  color: var(--vp-c-brand-1);
}

.reveal {
  opacity: 0;
  transform: translateY(22px);
  transition: opacity 0.7s cubic-bezier(0.22, 1, 0.36, 1), transform 0.7s cubic-bezier(0.22, 1, 0.36, 1);
}

.reveal.in {
  opacity: 1;
  transform: none;
}

/* ── capabilities ────────────────────────────────── */
.caps {
  list-style: none;
  margin: 0;
  padding: 0;
  max-width: 640px;
  margin-inline: auto;
  text-align: left;
}

.caps li {
  display: grid;
  grid-template-columns: 120px 1fr;
  gap: 18px;
  align-items: baseline;
  padding: 13px 4px;
  border-bottom: 1px solid color-mix(in srgb, var(--vp-c-text-1) 12%, transparent);
  transition: border-color 0.25s;
}

.caps li:first-child {
  border-top: 1px solid color-mix(in srgb, var(--vp-c-text-1) 12%, transparent);
}

.caps li:hover {
  border-bottom-color: var(--vp-c-brand-1);
}

.cap-k {
  font-family: var(--vp-font-family-mono);
  font-size: 12.5px;
  letter-spacing: 0.08em;
  text-transform: uppercase;
  color: var(--vp-c-brand-1);
}

.cap-t {
  font-size: 15px;
  line-height: 1.55;
  color: var(--vp-c-text-2);
}

/* ── close ───────────────────────────────────────── */
.mini-enso {
  width: 72px;
  margin: 0 auto 14px;
  display: block;
}

.mini-enso path {
  stroke: var(--vp-c-text-1);
  stroke-width: 2;
  stroke-linecap: round;
  opacity: 0.55;
}

/* ── animation + responsive ──────────────────────── */
@keyframes land-rise {
  from { opacity: 0; transform: translateY(16px); }
  to { opacity: 1; transform: none; }
}

@keyframes cue-bob {
  0%, 100% { transform: translateX(-50%) translateY(0); }
  50% { transform: translateX(-50%) translateY(6px); }
}

@media (max-width: 860px) {
  .intro {
    grid-template-columns: 1fr;
    text-align: center;
    padding-top: 28px;
    gap: 8px;
  }

  .intro-text { order: 2; }
  .intro-mark { order: 1; }

  .sub { margin-inline: auto; }
  .cta { justify-content: center; }
  h1 .accent { margin-inline: auto; }
  .install { max-width: 100%; overflow-x: auto; text-align: left; }
  .scroll-cue { display: none; }
}

@media (prefers-reduced-motion: reduce) {
  .kicker, h1 .row, .sub, .cta, .install { animation: none; }
  .scroll-cue { animation: none; }
  .reveal { opacity: 1; transform: none; transition: none; }
  .btn:hover { transform: none; }
}
</style>

<style>
/* When the living-ink system runs, the particles themselves form the
   closing ensō — keep the static one for layout/position, invisibly.
   (visibility, not display: the anchor's rect still drives the ink.) */
html.ink-alive .close-enso-anchor path {
  visibility: hidden;
}
</style>
