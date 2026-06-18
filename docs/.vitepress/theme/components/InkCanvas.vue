<script setup lang="ts">
// The living-ink engine, mounted at layout level so the mark survives
// SPA navigation. The logo's ink pixels become ~2200 dashes with four
// homes: (0) the hero ensō on the landing page, (1) the workflow ring
// (or the stacked timeline's line on mobile), (2) the small closing
// ensō, and (3) the navbar logo slot on every other page — navigate
// into the docs and the ink travels up into the nav and becomes the
// logo. Threshold crossings and route changes roll fresh journeys
// (flock / flocks / scatter) that wander across the site before
// reassembling. Clicks explode the mark page-wide; the cursor bends
// it. Reduced motion (or any failure) leaves the static SVGs alone.
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useData, useRoute, withBase } from 'vitepress'

const { isDark } = useData()
const route = useRoute()

const canvas = ref<HTMLCanvasElement>()
const navCanvas = ref<HTMLCanvasElement>()
const ready = ref(false)
const navBarExists = ref(false)

let raf = 0
let running = false
const cleanupFns: Array<() => void> = []
let resetAnchors = () => {}

onMounted(async () => {
  if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) return
  navBarExists.value = !!document.querySelector('.VPNavBar')
  ready.value = true
  try {
    await start()
  } catch {
    ready.value = false
    document.documentElement.classList.remove('ink-alive')
  }
})

watch(
  () => route.path,
  () => resetAnchors(),
)

async function start() {
  await new Promise((r) => requestAnimationFrame(r))
  const el = canvas.value
  if (!el) throw new Error('no canvas')

  const ctx = el.getContext('2d')!
  const navEl2 = navCanvas.value
  const nctx = navEl2 ? navEl2.getContext('2d') : null
  const dpr = Math.min(window.devicePixelRatio || 1, 2)

  let vw = 0
  let vh = 0
  let navH = 0
  const resize = () => {
    vw = window.innerWidth
    vh = window.innerHeight
    el.width = vw * dpr
    el.height = vh * dpr
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
    if (navEl2 && nctx) {
      const r = navEl2.parentElement?.getBoundingClientRect()
      navH = r?.height ?? 0
      navEl2.width = vw * dpr
      navEl2.height = navH * dpr
      nctx.setTransform(dpr, 0, 0, dpr, 0, 0)
    }
  }
  resize()
  window.addEventListener('resize', resize, { passive: true })
  cleanupFns.push(() => window.removeEventListener('resize', resize))

  // ── sample ink pixels from the real logo ──────────────────
  const img = new Image()
  img.src = withBase('/logo.svg')
  await img.decode()

  const S = 480
  const off = document.createElement('canvas')
  off.width = S
  off.height = S
  const octx = off.getContext('2d', { willReadFrequently: true })!
  octx.drawImage(img, 0, 0, S, S)
  const data = octx.getImageData(0, 0, S, S).data

  const pts: { x: number; y: number }[] = []
  for (let y = 0; y < S; y += 2) {
    for (let x = 0; x < S; x += 2) {
      if (data[(y * S + x) * 4 + 3] > 60) {
        pts.push({ x: x / S, y: y / S })
      }
    }
  }
  if (pts.length < 200) throw new Error('sampling failed')

  let minX = 1, minY = 1, maxX = 0, maxY = 0
  for (const p of pts) {
    if (p.x < minX) minX = p.x
    if (p.x > maxX) maxX = p.x
    if (p.y < minY) minY = p.y
    if (p.y > maxY) maxY = p.y
  }
  const span = Math.max(maxX - minX, maxY - minY) || 1
  const FILL = 0.86
  for (const p of pts) {
    p.x = ((p.x - (minX + maxX) / 2) / span) * FILL
    p.y = ((p.y - (minY + maxY) / 2) / span) * FILL
  }

  for (let i = pts.length - 1; i > 0; i--) {
    const j = (Math.random() * (i + 1)) | 0
    ;[pts[i], pts[j]] = [pts[j], pts[i]]
  }
  const N = Math.min(2200, pts.length)

  // ── anchors ────────────────────────────────────────────────
  const q = (sel: string) => {
    const e = document.querySelector(sel)
    return e && (e as HTMLElement).isConnected ? e : null
  }
  let heroEl = q('.enso-mark-anchor')
  let ringEl: Element | null = null
  let stackEl: Element | null = null
  let closeEl: Element | null = null
  let navEl: Element | null = null
  let navBarEl: Element | null = null
  resetAnchors = () => {
    heroEl = null
    ringEl = null
    stackEl = null
    closeEl = null
    navEl = null
    navBarEl = null
  }

  const TAU = Math.PI * 2
  const OPENING = -Math.PI / 4

  // ── particles ──────────────────────────────────────────────
  // layout: x, y, vx, vy, phi, rUnit, activateAt, orient,
  //         releaseAt, assembleAt, wpx, wpy
  const STRIDE = 12
  const P = new Float32Array(N * STRIDE)
  const navBoot = !heroEl
  const bootRect = (heroEl ?? q('.VPNavBarTitle .logo'))?.getBoundingClientRect()
  const b0x = bootRect ? bootRect.left + bootRect.width / 2 : vw / 2
  const b0y = bootRect ? bootRect.top + bootRect.height / 2 : vh / 2
  const b0r = navBoot ? 130 : (bootRect?.width ?? 300)
  for (let i = 0; i < N; i++) {
    const t = pts[i]
    const phi = Math.atan2(t.y, t.x)
    const rUnit = Math.hypot(t.x, t.y)
    const a = Math.random() * Math.PI * 2
    const rr = b0r * (0.1 + Math.random() * 0.7)
    const o = i * STRIDE
    P[o] = b0x + Math.cos(a) * rr
    P[o + 1] = b0y + Math.sin(a) * rr
    P[o + 2] = -Math.sin(a) * (0.6 + Math.random() * 1.2)
    P[o + 3] = Math.cos(a) * (0.6 + Math.random() * 1.2)
    P[o + 4] = phi
    P[o + 5] = rUnit
    P[o + 6] = 600 + Math.random() * 1600
    P[o + 7] = phi + Math.PI / 2
  }

  const rollJourney = (nowEl: number) => {
    const mode = Math.random()
    const clusters: number[][] = []
    let nC = 0
    if (mode < 0.38) nC = 1
    else if (mode < 0.72) nC = 2 + ((Math.random() * 3) | 0)
    for (let c = 0; c < nC; c++) {
      clusters.push([
        vw * (0.12 + Math.random() * 0.76),
        vh * (0.12 + Math.random() * 0.76),
      ])
    }
    for (let i = 0; i < N; i++) {
      const o = i * STRIDE
      const rel =
        ((((P[o + 4] - OPENING) % TAU) + TAU) % TAU) / TAU
      if (nC) {
        const c = clusters[i % nC]
        P[o + 8] = nowEl + rel * 420 + Math.random() * 160
        P[o + 9] = P[o + 8] + 750 + Math.random() * 550
        P[o + 10] = c[0] + (Math.random() - 0.5) * 170
        P[o + 11] = c[1] + (Math.random() - 0.5) * 170
      } else {
        P[o + 8] = nowEl + rel * 600 + Math.random() * 500
        P[o + 9] = P[o + 8] + 500 + Math.random() * 1500
        P[o + 10] = vw * (0.06 + Math.random() * 0.88)
        P[o + 11] = vh * (0.06 + Math.random() * 0.88)
      }
    }
  }

  // ── pointer + explosion ────────────────────────────────────
  const mouse = { x: -9999, y: -9999, vx: 0, vy: 0 }
  const onMove = (e: MouseEvent) => {
    mouse.vx = e.clientX - mouse.x
    mouse.vy = e.clientY - mouse.y
    mouse.x = e.clientX
    mouse.y = e.clientY
  }
  const onOut = () => {
    mouse.x = -9999
    mouse.y = -9999
  }
  window.addEventListener('mousemove', onMove, { passive: true })
  window.addEventListener('mouseleave', onOut)

  const t0 = performance.now()
  let booted = false

  const explode = (e: PointerEvent) => {
    const now = performance.now() - t0
    booted = true
    const ex = e.clientX
    const ey = e.clientY
    for (let i = 0; i < N; i++) {
      const o = i * STRIDE
      const dx = P[o] - ex
      const dy = P[o + 1] - ey
      const d = Math.max(Math.hypot(dx, dy), 24)
      const boost = 16 * Math.exp(-d / 300) + 2.5
      P[o + 2] += (dx / d) * boost + (Math.random() - 0.5) * 2.4
      P[o + 3] += (dy / d) * boost + (Math.random() - 0.5) * 2.4
      P[o + 6] = now + 550 + Math.random() * 1200
    }
  }
  window.addEventListener('pointerdown', explode, { passive: true })

  cleanupFns.push(() => {
    window.removeEventListener('mousemove', onMove)
    window.removeEventListener('mouseleave', onOut)
    window.removeEventListener('pointerdown', explode)
  })

  // ── state ──────────────────────────────────────────────────
  // stations: 0 hero ensō, 1 ring/line, 2 closing ensō, 3 nav logo
  let morphDir = navBoot ? 3 : 0
  let prevDir = morphDir
  let passT0 = -1
  let lastScrollY = window.scrollY

  document.documentElement.classList.add('ink-alive')
  cleanupFns.push(() =>
    document.documentElement.classList.remove('ink-alive', 'ink-nav'),
  )

  running = true
  const frame = (now: number) => {
    if (!running) return
    const elapsed = now - t0
    if (elapsed > 2600) booted = true
    const theta = ((elapsed / 1000) * Math.PI * 2) / 140

    const scrollDelta = window.scrollY - lastScrollY
    lastScrollY = window.scrollY
    const scrolling = Math.abs(scrollDelta) > 1.5

    ctx.clearRect(0, 0, vw, vh)
    if (nctx) nctx.clearRect(0, 0, vw, navH)
    const ink = isDark.value ? 'rgba(226, 230, 240,' : 'rgba(30, 30, 40,'

    // ── anchors (re-queried after route changes) ─────────────
    if (!heroEl) heroEl = q('.enso-mark-anchor')
    if (!ringEl) ringEl = q('.loop-ring-svg')
    if (!stackEl) stackEl = q('.loop-stack')
    if (!closeEl) closeEl = q('.close-enso-anchor')
    if (!navEl) navEl = q('.VPNavBarTitle .logo')
    if (!navBarEl) navBarEl = q('.VPNavBar')

    // the navbar scrolls with the page on mobile (position: relative) —
    // route strokes by its CURRENT viewport band, drawing into the nav
    // layer in its local coordinates
    let navTop = -9999
    let navBot = -9999
    if (navBarEl && nctx) {
      const nbr = navBarEl.getBoundingClientRect()
      navTop = nbr.top
      navBot = nbr.top + nbr.height
    }

    let mx = 0, my = 0, ms = 0
    if (heroEl) {
      const rect = heroEl.getBoundingClientRect()
      mx = rect.left + rect.width / 2
      my = rect.top + rect.height / 2
      ms = rect.width
    }
    let ncx = 0, ncy = 0, nw = 0
    if (navEl) {
      const nRect = navEl.getBoundingClientRect()
      ncx = nRect.left + nRect.width / 2
      ncy = nRect.top + nRect.height / 2
      nw = nRect.width * 1.25
    }
    let rcx = 0, rcy = 0, rr = 0
    let lineX = 0, lineY = 0, lineH = 0
    let aTop = Infinity
    if (ringEl) {
      const rRect = ringEl.getBoundingClientRect()
      if (rRect.width > 0) {
        rcx = rRect.left + rRect.width / 2
        rcy = rRect.top + rRect.height / 2
        rr = (rRect.width * 150) / 420
        aTop = rRect.top
      } else if (stackEl) {
        const sRect = stackEl.getBoundingClientRect()
        if (sRect.height > 0) {
          // center the line on the station dots: each li::before sits
          // 20.8px left of the li's content edge (left -27.5 + half of
          // its 13.4px outer box)
          const li0 = stackEl.children[0] as HTMLElement | undefined
          lineX = li0 ? li0.getBoundingClientRect().left - 20.8 : sRect.left
          // run the line a good way past the first dot, like it flows in
          lineY = sRect.top - 46
          lineH = sRect.height - 30 + 46
          aTop = sRect.top
        }
      }
    }
    let ccx = 0, ccy = 0, cw = 0
    let cTop = Infinity
    if (closeEl) {
      const cRect = closeEl.getBoundingClientRect()
      ccx = cRect.left + cRect.width / 2
      ccy = cRect.top + cRect.height / 2
      cw = cRect.width
      cTop = cRect.top
    }

    // ── station selection ────────────────────────────────────
    let want = morphDir
    if (!heroEl) {
      // not on the landing page: the ink lives in the nav logo slot
      want = 3
    } else if (morphDir === 3) {
      // back on the landing page: pick by scroll position
      want = cTop < vh * 0.96 ? 2 : aTop < vh * 0.7 ? 1 : 0
    } else {
      if (morphDir < 2 && cTop < vh * 0.96) want = 2
      else if (morphDir === 0 && aTop < vh * 0.7) want = 1
      else if (morphDir === 2 && cTop > vh * 1.18)
        want = aTop < vh * 0.7 ? 1 : 0
      else if (morphDir === 1 && aTop > vh * 1.02) want = 0
    }
    if (want !== morphDir) {
      prevDir = morphDir
      morphDir = want
      passT0 = elapsed
      rollJourney(elapsed)
    }

    const navSettled = morphDir === 3

    // the station nodes act like a weaker cursor: the ink parts
    // around them instead of running through — on the desktop ring
    // and along the mobile timeline alike
    const nodePts: number[] = []
    if (rr) {
      for (const na of [-Math.PI / 2, 0, Math.PI / 2, Math.PI]) {
        nodePts.push(rcx + Math.cos(na) * rr, rcy + Math.sin(na) * rr)
      }
    } else if (lineH && stackEl) {
      for (const li of Array.from(stackEl.children)) {
        const lr = (li as HTMLElement).getBoundingClientRect()
        nodePts.push(lineX, lr.top + 8)
      }
    }
    const NODE_R = 28

    for (let i = 0; i < N; i++) {
      const o = i * STRIDE
      let x = P[o]
      // page-anchored stations scroll rigidly with the content; the nav
      // logo is viewport-fixed, so its ink ignores scroll entirely
      let y = P[o + 1] - (morphDir === 3 ? 0 : scrollDelta)
      let vx = P[o + 2]
      let vy = P[o + 3]

      const phi = P[o + 4] + theta
      const cphi = Math.cos(phi)
      const sphi = Math.sin(phi)
      const hand = rr * (1 + (((i * 37) % 11) / 11 - 0.5) * 0.05)
      const lrel =
        ((((P[o + 4] - OPENING) % TAU) + TAU) % TAU) / TAU
      const ljit = ((((i * 37) % 11) / 11 - 0.5) * 3.5)

      const shape = (dir: number): [number, number] => {
        if (dir === 1 && rr) return [rcx + cphi * hand, rcy + sphi * hand]
        if (dir === 1 && lineH) {
          // the line flows like the ring rotates (one loop / 140s):
          // ink runs down the timeline and travels back up to the top
          const flow = (lrel + elapsed / 140000) % 1
          return [lineX + ljit, lineY + flow * lineH]
        }
        if (dir === 2 && cw)
          return [ccx + cphi * P[o + 5] * cw, ccy + sphi * P[o + 5] * cw]
        if (dir === 3 && nw)
          return [ncx + cphi * P[o + 5] * nw, ncy + sphi * P[o + 5] * nw]
        if (heroEl) return [mx + cphi * P[o + 5] * ms, my + sphi * P[o + 5] * ms]
        if (nw) return [ncx + cphi * P[o + 5] * nw, ncy + sphi * P[o + 5] * nw]
        return [vw / 2, vh / 2]
      }
      const [sx, sy] = shape(morphDir)

      // silent wrap on the flowing line: when a particle's slot loops
      // from the bottom back to the top, move it there directly instead
      // of letting the spring drag a streak up the whole page
      if (
        morphDir === 1 &&
        lineH &&
        elapsed > P[o + 6] &&
        (passT0 < 0 || elapsed > P[o + 9]) &&
        y - sy > lineH * 0.5
      ) {
        y = sy - 4
        if (vy > 0.4) vy = 0.4
      }
      const [px, py] = shape(prevDir)

      const active = elapsed > P[o + 6]
      let dampA = 0.88
      let freeFly = !active // exploded / booting ink flies ballistic
      if (active) {
        if (passT0 >= 0 && elapsed < P[o + 8]) {
          vx += (px - x) * 0.055
          vy += (py - y) * 0.055
          vx += (Math.random() - 0.5) * 0.028
          vy += (Math.random() - 0.5) * 0.028
        } else if (passT0 >= 0 && elapsed < P[o + 9]) {
          freeFly = true
          const w =
            (elapsed - P[o + 8]) / Math.max(P[o + 9] - P[o + 8], 1)
          const sw = w * w * (3 - 2 * w)
          const chx = P[o + 10] * (1 - sw) + sx * sw
          const chy = P[o + 11] * (1 - sw) + sy * sw
          const k = 0.013 + 0.042 * sw
          vx += (chx - x) * k
          vy += (chy - y) * k
          const n = 0.3 * (1 - sw) + 0.03
          vx += (Math.random() - 0.5) * n
          vy += (Math.random() - 0.5) * n
          dampA = 0.955 - 0.075 * sw
        } else {
          const k = Math.min((elapsed - P[o + 6]) / 1400, 1) * 0.055
          vx += (sx - x) * k
          vy += (sy - y) * k
          // micro-drift scales with the mark: a 30px nav logo must sit
          // still where a 360px hero ensō may breathe
          const n = morphDir === 3 ? 0.005 : 0.028
          vx += (Math.random() - 0.5) * n
          vy += (Math.random() - 0.5) * n
        }
      } else if (!booted) {
        const dxc = x - b0x
        const dyc = y - b0y
        const dc = Math.hypot(dxc, dyc) || 1
        vx += (-dyc / dc) * 0.045 - (dxc / dc) * 0.012
        vy += (dxc / dc) * 0.045 - (dyc / dc) * 0.012
        vx += (Math.random() - 0.5) * 0.12
        vy += (Math.random() - 0.5) * 0.12
      } else {
        vx += (Math.random() - 0.5) * 0.05
        vy += (Math.random() - 0.5) * 0.05
      }

      const mdx = x - mouse.x
      const mdy = y - mouse.y
      const md = mdx * mdx + mdy * mdy
      const R = (heroEl ? ms : 160) * 0.16
      if (!scrolling && md < R * R) {
        const d = Math.sqrt(md) || 1
        const f = (1 - d / R) * 2.2
        vx += (mdx / d) * f + mouse.vx * 0.06 * (1 - d / R)
        vy += (mdy / d) * f + mouse.vy * 0.06 * (1 - d / R)
      }

      for (let np = 0; np < nodePts.length; np += 2) {
        const ndx = x - nodePts[np]
        const ndy = y - nodePts[np + 1]
        const nd = ndx * ndx + ndy * ndy
        if (nd < NODE_R * NODE_R) {
          const d = Math.sqrt(nd) || 1
          const f = (1 - d / NODE_R) * 1.3
          vx += (ndx / d) * f
          vy += (ndy / d) * f
        }
      }

      const damp = active ? dampA : 0.965
      vx *= damp
      vy *= damp
      x += vx
      y += vy

      // free-flying ink bounces off the window edges (with a little
      // energy lost); spring-assembled ink may still follow scrolled
      // stations off-screen
      if (freeFly) {
        if (x < 2 && vx < 0) {
          vx = -vx * 0.72
          x = 2
        } else if (x > vw - 2 && vx > 0) {
          vx = -vx * 0.72
          x = vw - 2
        }
        if (y < 2 && vy < 0) {
          vy = -vy * 0.72
          y = 2
        } else if (y > vh - 2 && vy > 0) {
          vy = -vy * 0.72
          y = vh - 2
        }
      }

      P[o] = x
      P[o + 1] = y
      P[o + 2] = vx
      P[o + 3] = vy

      if (x < -40 || x > vw + 40 || y < -40 || y > vh + 40) continue
      // the nav mark is tiny — draw a subset so it stays line art
      if (navSettled && (i & 3) !== 0) continue

      const sp = Math.hypot(vx, vy)
      let dirX: number
      let dirY: number
      if (sp > 0.25) {
        dirX = vx / sp
        dirY = vy / sp
        P[o + 7] = Math.atan2(dirY, dirX)
      } else {
        dirX = Math.cos(P[o + 7])
        dirY = Math.sin(P[o + 7])
      }
      const len = 1.1 + Math.min(sp * 1.8, 6)
      const a = active
        ? Math.min(0.62 + sp * 0.05, 0.85)
        : Math.min(0.3 + sp * 0.1, 0.7)
      // strokes over the navbar render on its own layer (above its
      // background, below its text) in the navbar's local coordinates;
      // the rest renders on the page layer behind all content
      const inNav = nctx && y >= navTop && y < navBot
      const g = inNav ? nctx! : ctx
      const gy = inNav ? y - navTop : y
      g.strokeStyle = ink + a + ')'
      g.lineWidth = navSettled ? 1 : 1.15
      g.beginPath()
      g.moveTo(x - dirX * len * 0.6, gy - dirY * len * 0.6)
      g.lineTo(x + dirX * len * 0.4, gy + dirY * len * 0.4)
      g.stroke()
    }

    raf = requestAnimationFrame(frame)
  }
  raf = requestAnimationFrame(frame)
}

onBeforeUnmount(() => {
  running = false
  cancelAnimationFrame(raf)
  cleanupFns.forEach((fn) => fn())
})
</script>

<template>
  <Teleport v-if="ready" to="body">
    <canvas ref="canvas" class="enso-ink" aria-hidden="true" />
  </Teleport>
  <Teleport v-if="ready && navBarExists" to=".VPNavBar">
    <canvas ref="navCanvas" class="enso-ink-nav" aria-hidden="true" />
  </Teleport>
</template>

<style>
.enso-ink {
  position: fixed;
  inset: 0;
  width: 100vw;
  height: 100vh;
  pointer-events: none;
  /* behind all page content */
  z-index: -1;
}

/* The nav strip gets its own ink layer INSIDE the navbar: above the
   navbar's background (which must keep masking scrolled content),
   below the navbar's text/controls. */
.enso-ink-nav {
  position: absolute;
  inset: 0;
  width: 100%;
  height: 100%;
  pointer-events: none;
  z-index: 0;
}

html.ink-alive .VPNavBar > .wrapper,
html.ink-alive .VPNavBar > .divider {
  position: relative;
  z-index: 1;
}

/* the ink replaces the static logos while it runs */
html.ink-alive .VPNavBarTitle .logo {
  visibility: hidden;
}

/* The static desktop sidebar masks nothing (content never scrolls
   beneath it) — its surface counts as background, so the ink shows
   through under the sidebar text. The mobile drawer keeps its bg. */
@media (min-width: 960px) {
  html.ink-alive .VPSidebar {
    background-color: transparent !important;
    /* keep the column readable without its tint */
    border-right: 1px solid var(--vp-c-divider);
  }
}
</style>
