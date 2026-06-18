import DefaultTheme from 'vitepress/theme'
import type { Theme } from 'vitepress'
import Layout from './Layout.vue'
import HomeLanding from './components/HomeLanding.vue'
import TerminalDemo from './components/TerminalDemo.vue'

import '@fontsource-variable/inter'
import '@fontsource/jetbrains-mono/400.css'
import '@fontsource/jetbrains-mono/500.css'
import './custom.css'

export default {
  extends: DefaultTheme,
  Layout,
  enhanceApp({ app }) {
    app.component('HomeLanding', HomeLanding)
    app.component('TerminalDemo', TerminalDemo)
  },
} satisfies Theme
