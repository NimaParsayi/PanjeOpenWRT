// Panje service worker: app shell works offline-ish; API calls are never cached.
const V = "panje-v1";
const SHELL = ["/", "/icon.png", "/logo.png", "/vazirmatn.woff2", "/flags.woff2", "/manifest.webmanifest"];
self.addEventListener("install", e => {
  e.waitUntil(caches.open(V).then(c => c.addAll(SHELL)).then(() => self.skipWaiting()));
});
self.addEventListener("activate", e => {
  e.waitUntil(caches.keys().then(ks => Promise.all(ks.filter(k => k !== V).map(k => caches.delete(k)))).then(() => self.clients.claim()));
});
self.addEventListener("fetch", e => {
  const r = e.request, u = new URL(r.url);
  if (r.method !== "GET" || u.origin !== location.origin || u.pathname.startsWith("/api/")) return;
  if (r.mode === "navigate") { // network first so updates show up, cache as fallback
    e.respondWith(fetch(r).then(res => { caches.open(V).then(c => c.put("/", res.clone())); return res; })
      .catch(() => caches.match("/")));
    return;
  }
  e.respondWith(caches.match(r).then(hit => hit || fetch(r).then(res => {
    if (res.ok) caches.open(V).then(c => c.put(r, res.clone()));
    return res;
  })));
});
