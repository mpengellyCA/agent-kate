import { createRouter, createWebHistory } from 'vue-router'

// App owns the visual shell, while this router owns the address bar and browser
// history.  Keeping the records component-free lets the shell keep its secure
// boot states (pairing, revocation and offline) outside normal navigation.
const RouteShell = { render: () => null }

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', name: 'projects', component: RouteShell },
    { path: '/projects/:project', name: 'project', component: RouteShell, props: true },
    { path: '/agents/:threadId', name: 'agent', component: RouteShell, props: true },
    { path: '/:pathMatch(.*)*', redirect: { name: 'projects' } },
  ],
})
