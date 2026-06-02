(function () {
  "use strict";

  const ENTRY = "night-sleep";
  const STATUS_POLL_MS = 5000;
  const TICK_MS = 250;
  const IMMINENT_S = 60;          // last minute → orange/pulse
  const SNOOZE_MINUTES = 15;

  const timerEl = document.getElementById("countdown-timer");
  const eyebrowEl = document.getElementById("countdown-eyebrow");
  const deadlineEl = document.getElementById("countdown-deadline");
  const snoozeBtn = document.getElementById("countdown-snooze");
  const logEl = document.getElementById("countdown-log");
  const logLinesEl = document.getElementById("countdown-log-lines");

  let deadline = null;          // Date | null
  let lastState = null;         // string | null
  let tickHandle = null;
  let pollHandle = null;
  let sseSource = null;
  let logOpen = false;

  function fmtHMS(ms) {
    const total = Math.max(0, Math.floor(ms / 1000));
    const h = Math.floor(total / 3600);
    const m = Math.floor((total % 3600) / 60);
    const s = total % 60;
    const pad = (n) => String(n).padStart(2, "0");
    return pad(h) + ":" + pad(m) + ":" + pad(s);
  }

  function fmtTime(d) {
    return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  }

  function tick() {
    if (!deadline) {
      timerEl.textContent = "--:--:--";
      timerEl.classList.remove("imminent", "fired");
      return;
    }
    const ms = deadline - new Date();
    timerEl.textContent = fmtHMS(ms);

    const secs = ms / 1000;
    timerEl.classList.toggle("imminent", secs > 0 && secs <= IMMINENT_S);
    timerEl.classList.toggle("fired", ms <= 0);
  }

  function startTicking() {
    if (tickHandle) clearInterval(tickHandle);
    tick();
    tickHandle = setInterval(tick, TICK_MS);
  }

  function updateUIFromStatus(data) {
    lastState = data.state;

    // Pick the deadline: a future deferred-fire from snooze wins; else the
    // recurring next fire. If neither is in the future, deadline = null.
    const now = new Date();
    let next = null;
    const snoozes = data.snoozes || {};
    const s = snoozes[ENTRY];
    if (s && s.deferred_fire_at) {
      const d = new Date(s.deferred_fire_at);
      if (d > now) next = d;
    }
    if (!next) {
      const nf = (data.next_fires || {})[ENTRY];
      if (nf) {
        const d = new Date(nf);
        if (d > now) next = d;
      }
    }
    deadline = next;

    // Eyebrow + deadline text.
    if (data.state === "night-sleeping" || data.state === "night" || data.state === "asleep") {
      eyebrowEl.textContent = "Lights out";
      deadlineEl.textContent = data.state === "night-sleeping" ? "Stopping…" : "";
    } else if (deadline) {
      eyebrowEl.textContent = (s && s.deferred_fire_at) ? "Snoozed" : "Sleeping in";
      deadlineEl.textContent = "at " + fmtTime(deadline);
    } else {
      eyebrowEl.textContent = "No sleep scheduled";
      deadlineEl.textContent = "";
    }

    // On-page snooze button: visible only when night mode is configured
    // (next_fires has the entry), not already night, and not transitioning.
    const canSnooze = !!((data.next_fires || {})[ENTRY])
      && data.state !== "night" && data.state !== "asleep"
      && !data.transitioning;
    snoozeBtn.hidden = !canSnooze;

    // Reveal log panel once the orchestrator is actually night-sleeping.
    if (data.state === "night-sleeping" && !logOpen) {
      openLog();
    }
    if (data.state !== "night-sleeping" && data.transitioning === false && logOpen
        && (data.state === "night" || data.state === "asleep")) {
      // Sleep finished — keep the log visible but stop SSE.
      closeSSE();
    }

    tick();
  }

  async function pollStatus() {
    try {
      const resp = await fetch("/api/status", { cache: "no-store" });
      if (!resp.ok) throw new Error("HTTP " + resp.status);
      const data = await resp.json();
      updateUIFromStatus(data);
    } catch (err) {
      console.error("countdown status poll failed:", err);
    } finally {
      if (pollHandle) clearTimeout(pollHandle);
      pollHandle = setTimeout(pollStatus, STATUS_POLL_MS);
    }
  }

  function openLog() {
    logOpen = true;
    logEl.hidden = false;
    appendLogLine("> homelab.sleep initiated", "");
    openSSE();
  }

  function openSSE() {
    if (sseSource) return;
    if (typeof EventSource === "undefined") {
      appendLogLine("> (SSE not supported in this browser)", "error");
      return;
    }
    sseSource = new EventSource("/api/events");
    sseSource.addEventListener("message", (ev) => {
      try {
        const e = JSON.parse(ev.data);
        renderEvent(e);
      } catch (_) { /* ignore */ }
    });
    sseSource.addEventListener("error", () => {
      // Browser will auto-reconnect; nothing to do.
    });
  }

  function closeSSE() {
    if (sseSource) {
      sseSource.close();
      sseSource = null;
    }
  }

  function renderEvent(e) {
    switch (e.type) {
      case "sleep_start":
        appendLogLine("> orchestrator: night sleep starting", "");
        return;
      case "vm_action": {
        const verb = e.action === "stop" ? "stopping" : "starting";
        const label = e.name ? (e.name + " (" + e.vmid + ")") : ("VM " + e.vmid);
        const tier = e.tier ? ("tier " + e.tier + ": ") : "";
        appendLogLine("> " + tier + verb + " " + label, "");
        return;
      }
      case "vm_error":
        appendLogLine("> error VM " + e.vmid + ": " + e.error, "error");
        return;
      case "sleep_complete":
        appendLogLine("> sleep complete", "done");
        return;
      default:
        appendLogLine("> " + JSON.stringify(e), "");
    }
  }

  function appendLogLine(text, cls) {
    const li = document.createElement("li");
    li.textContent = text;
    if (cls) li.className = cls;
    logLinesEl.appendChild(li);
    // Keep latest in view.
    li.scrollIntoView({ block: "end", behavior: "smooth" });
  }

  async function snooze() {
    snoozeBtn.disabled = true;
    const originalLabel = snoozeBtn.textContent;
    try {
      const resp = await fetch("/api/snooze", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          name: ENTRY,
          mode: "postpone",
          delay_minutes: SNOOZE_MINUTES,
        }),
      });
      if (!resp.ok) {
        const t = await resp.text();
        throw new Error(t || "HTTP " + resp.status);
      }
      snoozeBtn.textContent = "Snoozed ✓";
      // Re-poll status so the deadline re-anchors immediately.
      await pollStatus();
      setTimeout(() => { snoozeBtn.textContent = originalLabel; }, 1500);
    } catch (err) {
      console.error(err);
      snoozeBtn.textContent = "Failed — retry";
      setTimeout(() => { snoozeBtn.textContent = originalLabel; }, 2000);
    } finally {
      snoozeBtn.disabled = false;
    }
  }

  snoozeBtn.addEventListener("click", snooze);

  // Pause polling when the page is hidden to save battery on iOS.
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) {
      if (pollHandle) { clearTimeout(pollHandle); pollHandle = null; }
    } else {
      pollStatus();
    }
  });

  startTicking();
  pollStatus();
})();
