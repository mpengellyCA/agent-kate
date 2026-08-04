import { createApp } from 'vue'
import App from './App.vue'
import { captureHash } from './api/auth.js'
import './style.css'

// Snapshot before any UI code can rewrite browser history. The pairing secret
// only ever exists in this fragment and is exchanged for an HttpOnly cookie.
captureHash()

// `router.js` creates browser history at module evaluation time. Importing it
// only after the snapshot protects a fresh pairing fragment from being touched
// by routing before bootstrapAuth exchanges and removes it.
const { router } = await import('./router.js')
createApp(App).use(router).mount('#app')
