<script setup lang="ts">
// Passive hero anchor for the living-ink system (InkCanvas, mounted at
// layout level). Renders the ink wash and the static mark — the static
// mark doubles as the position/size anchor for the particles and stays
// visible as the reduced-motion / no-JS fallback.
import { withBase } from 'vitepress'
</script>

<template>
  <div class="enso">
    <div class="wash" aria-hidden="true" />
    <img
      class="mark-static enso-mark-anchor"
      :src="withBase('/logo.svg')"
      alt="lok8s"
    />
  </div>
</template>

<style scoped>
.enso {
  position: relative;
  width: min(360px, 70vw);
  aspect-ratio: 1;
  margin: 0 auto;
}

.wash {
  position: absolute;
  inset: -14%;
  border-radius: 50%;
  background: radial-gradient(
    closest-side,
    color-mix(in srgb, var(--vp-c-brand-1) 15%, transparent) 0%,
    color-mix(in srgb, var(--vp-c-brand-1) 6%, transparent) 55%,
    transparent 100%
  );
  filter: blur(8px);
  animation: enso-wash 9s ease-in-out infinite;
}

.mark-static {
  position: absolute;
  inset: 0;
  width: 100%;
  height: 100%;
}

.dark .mark-static {
  filter: invert(1) brightness(1.05);
}

@keyframes enso-wash {
  0%, 100% { transform: scale(1); opacity: 0.9; }
  50% { transform: scale(1.07); opacity: 1; }
}

@media (prefers-reduced-motion: reduce) {
  .wash { animation: none; }
}
</style>

<style>
/* the particles form the mark while the ink system runs; the static
   img keeps its layout box as the anchor (visibility, not display) */
html.ink-alive .enso .mark-static {
  visibility: hidden;
}
</style>
