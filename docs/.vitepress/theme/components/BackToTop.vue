<script setup lang="ts">
// Floating back-to-top, bottom-left, in the site's hand-inked style.
// Appears after scrolling past ~60% of a viewport; smooth-scrolls to
// the top (instant under reduced motion).
import { onBeforeUnmount, onMounted, ref } from 'vue'

const visible = ref(false)

function onScroll() {
  visible.value = window.scrollY > window.innerHeight * 0.6
}

function toTop() {
  const reduced = window.matchMedia('(prefers-reduced-motion: reduce)').matches
  window.scrollTo({ top: 0, behavior: reduced ? 'auto' : 'smooth' })
}

onMounted(() => {
  window.addEventListener('scroll', onScroll, { passive: true })
  onScroll()
})

onBeforeUnmount(() => window.removeEventListener('scroll', onScroll))
</script>

<template>
  <Transition name="btt">
    <button
      v-if="visible"
      class="back-to-top"
      type="button"
      aria-label="Back to top"
      @click="toTop"
    >
      <svg viewBox="0 0 24 16" fill="none" aria-hidden="true">
        <path d="M3 12 c4 -3 7 -6 9 -8 c2 2 5 5 9 8" />
      </svg>
    </button>
  </Transition>
</template>

<style scoped>
.back-to-top {
  position: fixed;
  left: 18px;
  bottom: 18px;
  z-index: 30;
  width: 42px;
  height: 42px;
  display: flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
  color: var(--vp-c-text-1);
  background: color-mix(in srgb, var(--vp-c-bg) 82%, transparent);
  backdrop-filter: blur(6px);
  border: 1.7px solid var(--vp-c-text-1);
  /* the sketchy ink border the buttons use */
  border-radius: 255px 18px 225px 18px / 18px 225px 18px 255px;
  transition:
    transform 0.2s cubic-bezier(0.22, 1, 0.36, 1),
    border-color 0.2s,
    color 0.2s;
}

.back-to-top:hover {
  transform: translateY(-2px) rotate(-2deg);
  border-color: var(--vp-c-brand-1);
  color: var(--vp-c-brand-1);
}

.back-to-top svg {
  width: 20px;
}

.back-to-top path {
  stroke: currentColor;
  stroke-width: 1.8;
  stroke-linecap: round;
}

.btt-enter-active,
.btt-leave-active {
  transition: opacity 0.25s, transform 0.25s cubic-bezier(0.22, 1, 0.36, 1);
}

.btt-enter-from,
.btt-leave-to {
  opacity: 0;
  transform: translateY(10px);
}

@media (prefers-reduced-motion: reduce) {
  .back-to-top,
  .btt-enter-active,
  .btt-leave-active {
    transition: none;
  }
}
</style>
