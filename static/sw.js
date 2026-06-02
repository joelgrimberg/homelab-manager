/* Homelab Manager service worker — push only. */

self.addEventListener("install", (event) => {
  self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(self.clients.claim());
});

self.addEventListener("push", (event) => {
  let payload = { title: "Homelab", body: "" };
  if (event.data) {
    try {
      payload = Object.assign(payload, event.data.json());
    } catch (_) {
      payload.body = event.data.text();
    }
  }
  const options = {
    body: payload.body,
    icon: "/icons/icon-192.png",
    badge: "/icons/icon-192.png",
    tag: payload.tag || "homelab",
    renotify: true,
    actions: payload.actions || [],
    data: payload.data || {},
  };
  event.waitUntil(self.registration.showNotification(payload.title, options));
});

self.addEventListener("notificationclick", (event) => {
  const data = (event.notification && event.notification.data) || {};
  const action = event.action;
  event.notification.close();

  event.waitUntil((async () => {
    if (action === "snooze" && data.name && data.minutes) {
      try {
        await fetch("/api/snooze", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            name: data.name,
            mode: "postpone",
            delay_minutes: data.minutes,
          }),
        });
      } catch (_) {
        // Best-effort; the user can retry from the in-app banner.
      }
      return;
    }

    // Body tap → open click_url (defaults to "/").
    const target = data.click_url || "/";
    const allClients = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
    for (const client of allClients) {
      if ("focus" in client) {
        if (client.navigate) {
          try { await client.navigate(target); } catch (_) {}
        }
        return client.focus();
      }
    }
    if (self.clients.openWindow) {
      return self.clients.openWindow(target);
    }
  })());
});
