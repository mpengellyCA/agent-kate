import { createApp } from 'vue'
import App from './App.vue'
import { captureHash } from './api/auth.js'
import './style.css'

// Snapshot before any UI code can rewrite browser history. The pairing secret
// only ever exists in this fragment and is exchanged for an HttpOnly cookie.
captureHash()
createApp(App).mount('#app')
