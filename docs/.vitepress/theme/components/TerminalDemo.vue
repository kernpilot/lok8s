<script setup lang="ts">
// Animated terminal: types the lok8s quick-start when scrolled into
// view, once. Skips straight to the final frame when the user prefers
// reduced motion.
import { onBeforeUnmount, onMounted, ref } from 'vue'

interface Line {
  kind: 'cmd' | 'out' | 'ok'
  text: string
  delay?: number
}

const script: Line[] = [
  { kind: 'cmd', text: 'lo use lok8s.dev' },
  { kind: 'out', text: 'Active domain: lok8s.dev', delay: 200 },
  { kind: 'cmd', text: 'lo up', delay: 500 },
  { kind: 'out', text: 'Creating Docker network lok8s (10.125.125.0/24)', delay: 350 },
  { kind: 'out', text: 'Starting registry mirrors (build, cache, io-docker, …)', delay: 300 },
  { kind: 'out', text: 'Creating kind cluster: local', delay: 600 },
  { kind: 'out', text: 'Bootstrap: cilium ✓  metallb ✓', delay: 700 },
  { kind: 'out', text: 'TLS: *.lok8s.dev (mkcert)', delay: 300 },
  { kind: 'ok', text: 'Tilt started on http://localhost:10350', delay: 400 },
  { kind: 'cmd', text: 'lo status', delay: 600 },
  { kind: 'ok', text: 'Running', delay: 250 },
]

const visible = ref<Line[]>([])
const typed = ref('')
const typing = ref(false)
const done = ref(false)
const root = ref<HTMLElement>()

let observer: IntersectionObserver | undefined
let timers: ReturnType<typeof setTimeout>[] = []

function later(fn: () => void, ms: number) {
  timers.push(setTimeout(fn, ms))
}

function play(index = 0) {
  if (index >= script.length) {
    done.value = true
    return
  }
  const line = script[index]
  later(() => {
    if (line.kind === 'cmd') {
      typing.value = true
      typed.value = ''
      let i = 0
      const tick = () => {
        if (i <= line.text.length) {
          typed.value = line.text.slice(0, i)
          i++
          later(tick, 28 + Math.random() * 40)
        } else {
          typing.value = false
          visible.value.push(line)
          play(index + 1)
        }
      }
      tick()
    } else {
      visible.value.push(line)
      play(index + 1)
    }
  }, line.delay ?? 100)
}

function start() {
  if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
    visible.value = script
    done.value = true
    return
  }
  play()
}

onMounted(() => {
  observer = new IntersectionObserver(
    (entries) => {
      if (entries[0]?.isIntersecting) {
        observer?.disconnect()
        start()
      }
    },
    { threshold: 0.35 },
  )
  if (root.value) observer.observe(root.value)
})

onBeforeUnmount(() => {
  observer?.disconnect()
  timers.forEach(clearTimeout)
})
</script>

<template>
  <div ref="root" class="term" role="img" aria-label="Terminal demo: lo use lok8s.dev, lo up provisions the cluster, lo status reports Running">
    <div class="term-bar">
      <span class="dot r" /><span class="dot y" /><span class="dot g" />
      <span class="term-title">lok8s — local cluster in one command</span>
    </div>
    <div class="term-body">
      <div v-for="(line, i) in visible" :key="i" :class="['line', line.kind]">
        <template v-if="line.kind === 'cmd'"><span class="prompt">$</span> {{ line.text }}</template>
        <template v-else>{{ line.text }}</template>
      </div>
      <div v-if="typing" class="line cmd">
        <span class="prompt">$</span> {{ typed }}<span class="caret" />
      </div>
      <div v-else-if="done" class="line cmd">
        <span class="prompt">$</span> <span class="caret" />
      </div>
    </div>
  </div>
</template>

<style scoped>
.term {
  max-width: 720px;
  margin: 0 auto;
  border: 1px solid var(--vp-c-divider);
  border-radius: 12px;
  overflow: hidden;
  background: var(--vp-c-bg-alt);
  box-shadow: 0 12px 40px -18px color-mix(in srgb, var(--vp-c-brand-1) 30%, transparent);
  font-family: var(--vp-font-family-mono);
  font-size: 13.5px;
  line-height: 1.7;
}

.term-bar {
  display: flex;
  align-items: center;
  gap: 7px;
  padding: 10px 14px;
  border-bottom: 1px solid var(--vp-c-divider);
  background: var(--vp-c-bg-soft);
}

.dot {
  width: 11px;
  height: 11px;
  border-radius: 50%;
  opacity: 0.85;
}
.dot.r { background: #f38ba8; }
.dot.y { background: #f9e2af; }
.dot.g { background: #a6e3a1; }

.term-title {
  margin-left: 8px;
  font-size: 12px;
  color: var(--vp-c-text-3);
  font-family: var(--vp-font-family-base);
}

.term-body {
  padding: 16px 18px;
  min-height: 280px;
  text-align: left;
}

.line { white-space: pre-wrap; word-break: break-word; }
.line.cmd { color: var(--vp-c-text-1); }
.line.out { color: var(--vp-c-text-2); }
.line.ok { color: #40a02b; }
.dark .line.ok { color: #a6e3a1; }

.prompt { color: var(--vp-c-brand-1); font-weight: 500; }

.caret {
  display: inline-block;
  width: 8px;
  height: 1.1em;
  margin-left: 2px;
  vertical-align: text-bottom;
  background: var(--vp-c-brand-1);
  animation: blink 1.1s steps(1) infinite;
}

@keyframes blink {
  50% { opacity: 0; }
}

@media (prefers-reduced-motion: reduce) {
  .caret { animation: none; }
}
</style>
