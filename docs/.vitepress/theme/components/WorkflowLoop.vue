<script setup lang="ts">
// The brand story as a diagram: one continuous loop through four
// stations — define, up, provision, observe — drawn in the logo's
// hand-inked style when scrolled into view.
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { withBase } from 'vitepress'

const root = ref<HTMLElement>()
const active = ref(false)
let observer: IntersectionObserver | undefined

const stations = [
  { angle: -90, k: 'define', cmd: 'cluster.lok8s.yaml', href: '/reference/specs', text: 'One folder per FQDN. Spec, targets, secrets — committed.' },
  { angle: 0, k: 'up', cmd: 'lo up', href: '/guide/local-dev', text: 'kind cluster with mirrors, TLS, bootstrap waves, Tilt.' },
  { angle: 90, k: 'ship', cmd: 'lo provision', href: '/guide/capi', text: 'Same folder, real driver — CAPI or KubeOne on Hetzner.' },
  { angle: 180, k: 'observe', cmd: 'lo status', href: '/reference/cli', text: 'Health, targets, GitOps — and back to define.' },
]

function pos(angle: number, r: number) {
  const rad = (angle * Math.PI) / 180
  return { x: 210 + r * Math.cos(rad), y: 210 + r * Math.sin(rad) }
}

onMounted(() => {
  if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
    active.value = true
    return
  }
  observer = new IntersectionObserver(
    (entries) => {
      if (entries[0]?.isIntersecting) {
        active.value = true
        observer?.disconnect()
      }
    },
    { threshold: 0.3 },
  )
  if (root.value) observer.observe(root.value)
})

onBeforeUnmount(() => observer?.disconnect())
</script>

<template>
  <div ref="root" class="loop" :class="{ active }">
    <!-- ring + stations (desktop) -->
    <div class="ring-wrap" aria-hidden="true">
      <svg class="loop-ring-svg" viewBox="0 0 420 420" fill="none">
        <!-- two offset hand-drawn rings -->
        <circle class="ring r1" cx="210" cy="210" r="150" />
        <circle class="ring r2" cx="211" cy="208" r="146" />
        <!-- direction nib -->
        <path class="nib" d="M 348 178 l 14 -6 m -14 6 l 4 15" />
        <g v-for="(s, i) in stations" :key="s.k">
          <circle
            class="node"
            :style="{ transitionDelay: `${0.9 + i * 0.18}s` }"
            :cx="pos(s.angle, 150).x"
            :cy="pos(s.angle, 150).y"
            r="7"
          />
        </g>
      </svg>
      <div class="center">
        <span class="center-line">one</span>
        <span class="center-line">workflow</span>
      </div>
      <div
        v-for="(s, i) in stations"
        :key="s.k"
        class="station"
        :class="`st-${s.k}`"
        :style="{ transitionDelay: `${1.05 + i * 0.18}s` }"
      >
        <a class="cmd" :href="withBase(s.href)">{{ s.cmd }}</a>
        <p>{{ s.text }}</p>
      </div>
    </div>

    <!-- stacked fallback (mobile) -->
    <ol class="stack loop-stack">
      <li v-for="(s, i) in stations" :key="s.k" :style="{ transitionDelay: `${0.2 + i * 0.15}s` }">
        <a class="cmd" :href="withBase(s.href)">{{ s.cmd }}</a>
        <p>{{ s.text }}</p>
      </li>
    </ol>
  </div>
</template>

<style scoped>
.loop {
  position: relative;
}

.ring-wrap {
  position: relative;
  width: min(560px, 92vw);
  margin: 0 auto;
  aspect-ratio: 1;
}

.ring-wrap svg {
  position: absolute;
  inset: 11%;
}

.ring {
  stroke: var(--vp-c-text-1);
  stroke-width: 1.6;
  stroke-linecap: round;
  opacity: 0.5;
  stroke-dasharray: 945;
  stroke-dashoffset: 945;
  transform: rotate(-78deg);
  transform-origin: 210px 210px;
}

.r2 {
  stroke-width: 1.1;
  opacity: 0.3;
  stroke-dasharray: 920;
  stroke-dashoffset: 920;
}

.active .ring {
  transition: stroke-dashoffset 1.7s cubic-bezier(0.6, 0, 0.3, 1);
  stroke-dashoffset: 0;
}

.active .r2 {
  transition-delay: 0.15s;
}

.nib {
  stroke: var(--vp-c-text-1);
  stroke-width: 1.6;
  stroke-linecap: round;
  opacity: 0;
}

.active .nib {
  transition: opacity 0.4s 1.7s;
  opacity: 0.5;
}

.node {
  fill: var(--vp-c-bg);
  stroke: var(--vp-c-brand-1);
  stroke-width: 2.2;
  opacity: 0;
  transition: opacity 0.45s;
}

.active .node {
  opacity: 1;
}

.center {
  position: absolute;
  inset: 0;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  pointer-events: none;
}

.center-line {
  font-size: 22px;
  font-weight: 650;
  letter-spacing: -0.02em;
  line-height: 1.2;
  color: var(--vp-c-text-1);
  opacity: 0;
  transform: translateY(6px);
  transition: opacity 0.6s 1.9s, transform 0.6s 1.9s;
}

.active .center-line {
  opacity: 1;
  transform: none;
}

.station {
  position: absolute;
  width: 200px;
  opacity: 0;
  transform: translateY(8px);
  transition: opacity 0.5s, transform 0.5s cubic-bezier(0.22, 1, 0.36, 1);
}

.active .station {
  opacity: 1;
  transform: none;
}

/* the ring (r=150 in a 420 viewBox, svg inset 11%) spans 22%–78% of
   the wrap — side stations sit flush against those tangents, text
   aligned toward the circle's edge */
.st-define { top: 4.5%; left: 50%; transform: translate(-50%, 8px); text-align: center; }
.active .st-define { transform: translate(-50%, 0); }
.st-up { top: 50%; left: calc(78% + 34px); transform: translate(0, calc(-50% + 8px)); text-align: left; }
.active .st-up { transform: translate(0, -50%); }
.st-ship { bottom: 4.5%; left: 50%; transform: translate(-50%, 8px); text-align: center; }
.active .st-ship { transform: translate(-50%, 0); }
.st-observe { top: 50%; right: calc(78% + 34px); transform: translate(0, calc(-50% + 8px)); text-align: right; }
.active .st-observe { transform: translate(0, -50%); }

.cmd {
  display: inline-block;
  font-family: var(--vp-font-family-mono);
  font-size: 13px;
  font-weight: 500;
  color: var(--vp-c-brand-1);
  background: var(--vp-c-brand-soft);
  border-radius: 4px;
  padding: 2px 8px;
  text-decoration: none;
  transition: background 0.2s ease, color 0.2s ease;
}

/* the command chips link to the matching docs page */
a.cmd:hover {
  background: var(--vp-c-brand-1);
  color: var(--vp-c-bg);
}

.station p,
.stack p {
  margin: 6px 0 0;
  font-size: 13.5px;
  line-height: 1.5;
  color: var(--vp-c-text-2);
}

/* stacked mobile fallback */
.stack {
  display: none;
  list-style: none;
  margin: 0;
  padding: 0 0 0 22px;
  border-left: 1.6px solid color-mix(in srgb, var(--vp-c-text-1) 35%, transparent);
}

.stack li {
  position: relative;
  padding: 0 0 26px;
  opacity: 0;
  transform: translateY(8px);
  transition: opacity 0.5s, transform 0.5s;
}

.active .stack li {
  opacity: 1;
  transform: none;
}

.stack li::before {
  content: '';
  position: absolute;
  left: -27.5px;
  top: 4px;
  width: 9px;
  height: 9px;
  border-radius: 50%;
  background: var(--vp-c-bg);
  border: 2.2px solid var(--vp-c-brand-1);
}

@media (max-width: 860px) {
  .ring-wrap { display: none; }
  .stack { display: block; }
}

@media (prefers-reduced-motion: reduce) {
  .ring, .r2, .nib, .node, .center-line, .station, .stack li {
    transition: none !important;
  }
  .active .ring, .active .r2 { stroke-dashoffset: 0; }
}
</style>

<style>
/* When the living-ink system is running, the hero's particles travel
   down and BECOME this ring — hide the static strokes, keep the
   station nodes. Fallback (reduced motion / no canvas) keeps them.
   On mobile the particles form the stacked timeline's line instead. */
html.ink-alive .loop .ring,
html.ink-alive .loop .r2,
html.ink-alive .loop .nib {
  display: none;
}

html.ink-alive .loop .stack {
  border-left-color: transparent;
}
</style>
