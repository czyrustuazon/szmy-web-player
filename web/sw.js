// Minimal service worker: makes the app installable and keeps the shell
// available offline. API traffic (audio, art, JSON) is never cached.
const SHELL = 'mmp-shell-v3';
const FILES = ['/', '/style.css', '/icon.svg', '/generic.svg', '/js/main.js', '/js/api.js', '/js/player.js', '/js/queue.js', '/js/viz.js', '/js/ui.js', '/js/util.js', '/js/uploader.js', '/js/uploadview.js', '/js/fuzzy.js', '/js/searchstate.js'];

self.addEventListener('install', (e) => {
  e.waitUntil(caches.open(SHELL).then((c) => c.addAll(FILES)).then(() => self.skipWaiting()));
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== SHELL).map((k) => caches.delete(k))))
      .then(() => self.clients.claim()),
  );
});

self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url);
  if (e.request.method !== 'GET' || url.origin !== location.origin || url.pathname.startsWith('/api/') || url.pathname.startsWith('/auth/') || url.pathname === '/healthz') return;
  // Network first so a new build is picked up immediately; cache is the offline fallback.
  e.respondWith(
    fetch(e.request)
      .then((res) => {
        if (res.ok) {
          const copy = res.clone();
          caches.open(SHELL).then((c) => c.put(e.request, copy));
        }
        return res;
      })
      .catch(() => caches.match(e.request)),
  );
});
