(function () {
  "use strict";

  const POLL_NORMAL = 5000;
  const POLL_FAST = 2000;

  let currentState = null;
  let pollInterval = POLL_NORMAL;
  let pollTimer = null;
  let expanded = {};
  let advancedMode = false;

  const badge = document.getElementById("state-badge");
  const toggle = document.getElementById("mode-toggle");
  const toggleInput = document.getElementById("mode-input");
  const spinner = document.getElementById("spinner");
  const tiersEl = document.getElementById("tiers");
  const instancesPaneEl = document.getElementById("instances-pane");
  const lastUpdated = document.getElementById("last-updated");
  const labelAwake = document.getElementById("mode-label-awake");
  const labelNight = document.getElementById("mode-label-night");
  const advancedInput = document.getElementById("advanced-input");
  const snoozeCard = document.getElementById("snooze-card");
  const snoozeLabel = document.getElementById("snooze-label");
  const snoozeActions = document.getElementById("snooze-actions");
  const pushCardEl = document.getElementById("push-card");
  const pushEnableBtn = document.getElementById("push-enable-btn");
  const pushLabel = document.getElementById("push-label");
  const pushTestRow = document.getElementById("push-test-row");
  const pushTestBtn = document.getElementById("push-test-btn");
  const scheduleBody = document.getElementById("schedule-body");
  const viewMain = document.getElementById("view-main");
  const viewSettings = document.getElementById("view-settings");
  const headerMain = document.getElementById("header-main");
  const headerSettings = document.getElementById("header-settings");
  const settingsOpenBtn = document.getElementById("settings-open-btn");
  const settingsCloseBtn = document.getElementById("settings-close-btn");

  let scheduleEntries = [];

  function showSettings() {
    headerMain.hidden = true;
    viewMain.hidden = true;
    headerSettings.hidden = false;
    viewSettings.hidden = false;
    // Lazy-load schedule on first open.
    if (scheduleEntries.length === 0) loadSchedule();
  }
  function showMain() {
    headerSettings.hidden = true;
    viewSettings.hidden = true;
    headerMain.hidden = false;
    viewMain.hidden = false;
  }
  settingsOpenBtn.addEventListener("click", showSettings);
  settingsCloseBtn.addEventListener("click", showMain);

  advancedInput.addEventListener("change", function () {
    advancedMode = advancedInput.checked;
    if (currentState) renderTiers(currentState.tiers);
  });

  // Desktop (>= 1024px) uses a two-column layout where the right pane is
  // always-on. Force advancedMode so the left-column tier headers also show
  // their wake/sleep toggle. On mobile the user controls it via the switch.
  const desktopMql = window.matchMedia("(min-width: 1024px)");
  function syncAdvancedToViewport() {
    if (desktopMql.matches) {
      advancedMode = true;
      advancedInput.checked = true;
    }
    if (currentState) renderTiers(currentState.tiers);
  }
  desktopMql.addEventListener("change", syncAdvancedToViewport);
  syncAdvancedToViewport();

  // The big toggle controls Night mode: ON = night (only exempt running),
  // OFF = awake. POSTs /api/night/sleep or /api/night/wake accordingly.
  toggleInput.addEventListener("change", function () {
    if (toggle.classList.contains("disabled")) {
      toggleInput.checked = !toggleInput.checked;
      return;
    }
    const action = toggleInput.checked ? "sleep" : "wake";
    toggle.classList.add("disabled", "transitioning");

    fetch("/api/night/" + action, { method: "POST" })
      .then(function (resp) {
        if (!resp.ok) {
          return resp.json().then(function (data) {
            throw new Error(data.error || "Failed");
          });
        }
        pollInterval = POLL_FAST;
        schedulePoll();
      })
      .catch(function (err) {
        console.error(err);
        toggleInput.checked = !toggleInput.checked;
        toggle.classList.remove("disabled", "transitioning");
        showError("Failed to toggle night mode: " + err.message);
      });
  });

  function fetchStatus() {
    fetch("/api/status")
      .then(function (resp) {
        if (!resp.ok) throw new Error("HTTP " + resp.status);
        return resp.json();
      })
      .then(function (data) {
        currentState = data;
        removeError();
        render(data);
        updatePollRate(data);
        schedulePoll();
      })
      .catch(function (err) {
        console.error("status poll failed:", err);
        showError("Connection lost");
        schedulePoll();
      });
  }

  function updatePollRate(data) {
    if (data.transitioning) {
      pollInterval = POLL_FAST;
    } else {
      pollInterval = POLL_NORMAL;
    }
  }

  function schedulePoll() {
    if (pollTimer) clearTimeout(pollTimer);
    pollTimer = setTimeout(fetchStatus, pollInterval);
  }

  function render(data) {
    // Badge
    badge.textContent = data.state;
    badge.className = "badge " + data.state;

    const isAwake = data.state === "awake";
    const isNight = data.state === "night";

    // Toggle: ON = night, OFF = awake. Other states leave whatever the user
    // last set, except we force a clear position for the two pure states.
    if (data.state === "awake") toggleInput.checked = false;
    else if (data.state === "night") toggleInput.checked = true;

    if (data.transitioning) {
      toggle.classList.add("disabled", "transitioning");
    } else {
      toggle.classList.remove("disabled", "transitioning");
    }

    renderSnoozeBanner(data);

    // Labels: highlight the side matching current state
    labelAwake.classList.toggle("active", isAwake);
    labelNight.classList.toggle("active", isNight);

    // Tiers
    renderTiers(data.tiers);

    // Last updated
    var now = new Date();
    lastUpdated.textContent = "Updated " + now.toLocaleTimeString();
  }

  // Render a single instance row. `tierNum` is the tier number for
  // transition-progress dot styling; pass null for always-on rows that
  // never participate in a tiered transition.
  function renderInstanceRow(inst, tierNum, forceAdvanced) {
    var dotClass = inst.status;
    if (inst.status !== "running" && inst.status !== "stopped") {
      dotClass = "transitioning";
    }
    if (tierNum != null && currentState && currentState.transitioning && currentState.current_tier > 0) {
      var dir = currentState.direction;
      var curTier = currentState.current_tier;
      if (dir === "waking") {
        if (tierNum > curTier) dotClass = "pending";
        else if (tierNum === curTier && inst.status !== "running") dotClass = "processing";
      } else if (dir === "sleeping") {
        if (tierNum < curTier) dotClass = "pending";
        else if (tierNum === curTier && inst.status !== "stopped") dotClass = "stopping";
      }
    }
    if (inst.stuck) dotClass = "stuck";

    var html = '<div class="instance' + (inst.protected ? " protected" : "") + (inst.stuck ? " stuck" : "") + '">';
    html += '<span class="status-dot ' + dotClass + '"></span>';
    html += '<span class="instance-name">' + escapeHtml(inst.name) + "</span>";
    if (inst.protected) {
      html += '<span class="instance-lock" title="Protected — never auto-stopped">&#128274;</span>';
    }
    if (inst.stuck) {
      html += '<span class="instance-warn" title="Did not reach target during last transition">&#9888;</span>';
    }
    if ((forceAdvanced || advancedMode) && !inst.protected) {
      var isRunning = inst.status === "running";
      var isStopped = inst.status === "stopped";
      var toggleDisabled = (currentState && currentState.transitioning) || (!isRunning && !isStopped);
      var toggleChecked = isRunning;
      html += '<label class="instance-toggle' + (toggleDisabled ? " disabled" : "") + '">';
      html += '<input type="checkbox"' + (toggleChecked ? " checked" : "") + (toggleDisabled ? " disabled" : "");
      html += ' data-vmid="' + inst.vmid + '">';
      html += '<span class="instance-slider"></span>';
      html += "</label>";
    }
    html += "</div>";
    return html;
  }

  function renderTiers(tiers) {
    // Preserve existing expanded state; default tier 1 to expanded on first render
    if (Object.keys(expanded).length === 0) {
      tiers.forEach(function (tier, i) {
        expanded[tier.tier] = i === 0;
      });
    }

    // Collect protected instances across every tier. Primary-host
    // protected instances go into the "Always on" card; fallback-host
    // instances (Source != "") go into a separate "Fallback" card so
    // it's visually clear those live on a different Proxmox.
    var alwaysOn = [];
    var fallbackByHost = {};
    tiers.forEach(function (tier) {
      tier.instances.forEach(function (inst) {
        if (!inst.protected) return;
        if (inst.source) {
          if (!fallbackByHost[inst.source]) fallbackByHost[inst.source] = [];
          fallbackByHost[inst.source].push(inst);
        } else {
          alwaysOn.push(inst);
        }
      });
    });

    var html = "";

    if (alwaysOn.length > 0) {
      html += '<div class="always-on-card">';
      html += '<div class="always-on-header">';
      html += '<span class="always-on-title">Always on</span>';
      html += "</div>";
      html += '<div class="always-on-instances">';
      alwaysOn.forEach(function (inst) {
        html += renderInstanceRow(inst, null);
      });
      html += "</div></div>";
    }

    Object.keys(fallbackByHost).sort().forEach(function (host) {
      html += '<div class="always-on-card fallback-card">';
      html += '<div class="always-on-header">';
      html += '<span class="always-on-title">Fallback</span>';
      html += '<span class="fallback-host">' + escapeHtml(host) + '</span>';
      html += "</div>";
      html += '<div class="always-on-instances">';
      fallbackByHost[host].forEach(function (inst) {
        html += renderInstanceRow(inst, null);
      });
      html += "</div></div>";
    });

    tiers.forEach(function (tier) {
      // Skip protected instances — they're already in Always-on / Fallback cards.
      var instances = tier.instances.filter(function (inst) { return !inst.protected; });
      if (instances.length === 0) return;

      var isExpanded = expanded[tier.tier];

      var running = 0, stopped = 0;
      instances.forEach(function (inst) {
        if (inst.status === "running") running++;
        else if (inst.status === "stopped") stopped++;
      });
      var tierChecked = running > 0 && running >= stopped;
      var tierDisabled = !!(currentState && currentState.transitioning);
      var tierMixed = running > 0 && stopped > 0;

      html += '<div class="tier' + (isExpanded ? " expanded" : "") + '" data-tier="' + tier.tier + '">';
      html += '<div class="tier-header" onclick="window.toggleTier(' + tier.tier + ')">';
      html += '<span class="tier-title"><span class="tier-num">Tier ' + tier.tier + "</span> " + escapeHtml(tier.name);
      if (tierMixed) html += ' <span class="tier-badge">mixed</span>';
      html += "</span>";
      html += '<span class="tier-header-right">';
      if (advancedMode) {
        html += '<label class="tier-toggle' + (tierDisabled ? " disabled" : "") + '" onclick="event.stopPropagation()">';
        html += '<input type="checkbox"' + (tierChecked ? " checked" : "") + (tierDisabled ? " disabled" : "");
        html += ' data-tier-num="' + tier.tier + '">';
        html += '<span class="tier-slider"></span>';
        html += "</label>";
      }
      html += '<span class="tier-chevron">&#9654;</span>';
      html += "</span>";
      html += "</div>";
      html += '<div class="tier-instances">';
      instances.forEach(function (inst) {
        html += renderInstanceRow(inst, tier.tier);
      });
      html += "</div></div>";
    });

    tiersEl.innerHTML = html;

    // Right pane (desktop): per-tier groups of non-protected instances with
    // toggles. Protected instances stay in the always-on / fallback cards on
    // the left so each VM appears exactly once. Hidden on mobile via CSS.
    var paneHtml = "";
    tiers.forEach(function (tier) {
      var paneInstances = tier.instances.filter(function (inst) { return !inst.protected; });
      if (paneInstances.length === 0) return;
      paneHtml += '<div class="instance-group" data-tier="' + tier.tier + '">';
      paneHtml += '<div class="instance-group-header">';
      paneHtml += '<span class="instance-group-num">Tier ' + tier.tier + '</span> ';
      paneHtml += escapeHtml(tier.name);
      paneHtml += "</div>";
      paneHtml += '<div class="instance-group-rows">';
      paneInstances.forEach(function (inst) {
        paneHtml += renderInstanceRow(inst, tier.tier, true);
      });
      paneHtml += "</div></div>";
    });
    instancesPaneEl.innerHTML = paneHtml;
  }

  window.toggleTier = function (tier) {
    expanded[tier] = !expanded[tier];
    if (currentState) renderTiers(currentState.tiers);
  };

  function escapeHtml(text) {
    var div = document.createElement("div");
    div.textContent = text;
    return div.innerHTML;
  }

  function showError(msg) {
    removeError();
    var el = document.createElement("div");
    el.className = "error-banner";
    el.id = "error-banner";
    el.textContent = msg;
    var container = document.querySelector(".container");
    container.insertBefore(el, container.children[1]);
  }

  function removeError() {
    var el = document.getElementById("error-banner");
    if (el) el.remove();
  }

  // Instance + tier toggle handler (event delegation). Bound on both the
  // left tier pane and the right instance-controls pane.
  function handlePaneChange(e) {
    var input = e.target;
    if (!input.dataset) return;

    if (input.dataset.vmid) {
      handleInstanceToggle(input);
      return;
    }
    if (input.dataset.tierNum) {
      handleTierToggle(input);
      return;
    }
  }
  tiersEl.addEventListener("change", handlePaneChange);
  instancesPaneEl.addEventListener("change", handlePaneChange);

  function handleInstanceToggle(input) {
    var vmid = input.dataset.vmid;
    var action = input.checked ? "start" : "stop";
    var label = input.closest(".instance-toggle");
    if (label) label.classList.add("disabled");
    input.disabled = true;

    fetch("/api/instance/" + vmid + "/" + action, { method: "POST" })
      .then(function (resp) {
        if (!resp.ok) {
          return resp.json().then(function (data) {
            throw new Error(data.error || "Failed");
          });
        }
        pollInterval = POLL_FAST;
        schedulePoll();
      })
      .catch(function (err) {
        console.error(err);
        input.checked = !input.checked;
        if (label) label.classList.remove("disabled");
        input.disabled = false;
        showError("Failed to " + action + " instance");
      });
  }

  function handleTierToggle(input) {
    var tier = input.dataset.tierNum;
    var action = input.checked ? "wake" : "sleep";
    var label = input.closest(".tier-toggle");
    if (label) label.classList.add("disabled");
    input.disabled = true;

    fetch("/api/tier/" + tier + "/" + action, { method: "POST" })
      .then(function (resp) {
        if (!resp.ok) {
          return resp.json().then(function (data) {
            throw new Error(data.error || "Failed");
          });
        }
        pollInterval = POLL_FAST;
        schedulePoll();
      })
      .catch(function (err) {
        console.error(err);
        input.checked = !input.checked;
        if (label) label.classList.remove("disabled");
        input.disabled = false;
        showError("Failed to " + action + " tier " + tier);
      });
  }

  // --- Snooze banner ---

  const SNOOZE_ENTRY = "night-sleep";
  const IMMINENT_WINDOW_MIN = 60;

  function fmtTime(iso) {
    const d = new Date(iso);
    return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  }

  function minutesUntil(iso) {
    return (new Date(iso) - new Date()) / 60000;
  }

  function renderSnoozeBanner(data) {
    // Hide banner if night mode unconfigured, already night/asleep, or
    // currently transitioning.
    if (!data.night_mode_enabled || data.state === "night" ||
        data.state === "asleep" || data.transitioning) {
      snoozeCard.hidden = true;
      return;
    }

    const snoozes = data.snoozes || {};
    const active = snoozes[SNOOZE_ENTRY];
    const nextFires = data.next_fires || {};
    const nextSleep = nextFires[SNOOZE_ENTRY];

    if (active) {
      snoozeCard.hidden = false;
      const deferred = active.deferred_fire_at ? new Date(active.deferred_fire_at) : null;
      const isPostpone = deferred && deferred > new Date();

      let cancelLabel = "Resume schedule";
      if (isPostpone) {
        snoozeLabel.textContent = "Sleep deferred to " + fmtTime(active.deferred_fire_at);
        // Mirror the server's cancel logic: when today's cron is still
        // ahead, cancel clears the snooze and the regular cron fires
        // later; when today's cron has already passed, cancel fires
        // sleep immediately. Preview the outcome in the label.
        const nextCron = nextSleep ? new Date(nextSleep) : null;
        if (nextCron && nextCron > deferred) {
          cancelLabel = "Sleep now";
        } else if (nextCron) {
          cancelLabel = "Sleep at " + fmtTime(nextSleep);
        } else {
          cancelLabel = "Sleep now";
        }
      } else {
        snoozeLabel.textContent = "Skipping tonight";
      }

      // Only show +30 min in postpone mode. In skip-tonight mode, the
      // only sensible action is to undo the skip — adding +30 there
      // would silently convert a skip into a postpone, which doesn't
      // match what the label promises.
      const extendBtn = isPostpone
        ? '<button type="button" class="push-btn" data-mode="30">+30 min</button>'
        : '';
      snoozeActions.innerHTML =
        extendBtn +
        '<button type="button" class="push-btn push-btn-secondary" id="snooze-cancel-btn">' + cancelLabel + '</button>';
      document.getElementById("snooze-cancel-btn").addEventListener("click", cancelSnooze);
      snoozeActions.querySelectorAll("button[data-mode]").forEach((btn) => {
        btn.addEventListener("click", () => snoozeAction(btn.dataset.mode));
      });
      return;
    }

    if (!nextSleep || minutesUntil(nextSleep) > IMMINENT_WINDOW_MIN || minutesUntil(nextSleep) < 0) {
      snoozeCard.hidden = true;
      return;
    }

    snoozeCard.hidden = false;
    snoozeLabel.textContent = "Sleeping at " + fmtTime(nextSleep);
    snoozeActions.innerHTML =
      '<button type="button" class="push-btn" data-mode="30">+30 min</button>' +
      '<button type="button" class="push-btn push-btn-secondary" data-mode="skip">Skip tonight</button>';
    snoozeActions.querySelectorAll("button").forEach((btn) => {
      btn.addEventListener("click", () => snoozeAction(btn.dataset.mode));
    });
  }

  async function snoozeAction(mode) {
    snoozeActions.querySelectorAll("button").forEach((b) => (b.disabled = true));
    let body;
    if (mode === "skip") {
      body = { name: SNOOZE_ENTRY, mode: "skip_tonight" };
    } else {
      body = { name: SNOOZE_ENTRY, mode: "postpone", delay_minutes: parseInt(mode, 10) };
    }
    try {
      const resp = await fetch("/api/snooze", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });
      if (!resp.ok) {
        const t = await resp.text();
        throw new Error(t || "HTTP " + resp.status);
      }
      pollInterval = POLL_FAST;
      schedulePoll();
    } catch (err) {
      console.error(err);
      showError("Snooze failed: " + err.message);
      snoozeActions.querySelectorAll("button").forEach((b) => (b.disabled = false));
    }
  }

  async function cancelSnooze() {
    const btn = document.getElementById("snooze-cancel-btn");
    if (btn) btn.disabled = true;
    try {
      const resp = await fetch("/api/snooze?name=" + encodeURIComponent(SNOOZE_ENTRY), {
        method: "DELETE",
      });
      if (!resp.ok) throw new Error("HTTP " + resp.status);
      pollInterval = POLL_FAST;
      schedulePoll();
    } catch (err) {
      console.error(err);
      showError("Cancel failed: " + err.message);
      if (btn) btn.disabled = false;
    }
  }

  // --- PWA: register service worker + push subscription flow ---

  function urlBase64ToUint8Array(base64) {
    const padding = "=".repeat((4 - base64.length % 4) % 4);
    const safe = (base64 + padding).replace(/-/g, "+").replace(/_/g, "/");
    const raw = atob(safe);
    const out = new Uint8Array(raw.length);
    for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
    return out;
  }

  let swRegistration = null;

  async function initPWA() {
    if (!("serviceWorker" in navigator)) {
      pushLabel.textContent = "Notifications unavailable (no service worker)";
      pushEnableBtn.disabled = true;
      pushCardEl.hidden = false;
      return;
    }
    if (!("PushManager" in window)) {
      pushLabel.textContent = "Notifications unavailable on this browser";
      pushEnableBtn.disabled = true;
      pushCardEl.hidden = false;
      return;
    }
    try {
      swRegistration = await navigator.serviceWorker.register("/sw.js");
    } catch (err) {
      console.error("sw register failed:", err);
      pushLabel.textContent = "Service worker failed: " + err.message;
      pushEnableBtn.disabled = true;
      pushCardEl.hidden = false;
      return;
    }
    pushCardEl.hidden = false;
    await refreshPushUI();
  }

  async function refreshPushUI() {
    const existing = await swRegistration.pushManager.getSubscription();
    if (existing) {
      pushLabel.textContent = "Notifications enabled on this device";
      pushEnableBtn.textContent = "Disable";
      pushEnableBtn.dataset.action = "disable";
      pushTestRow.hidden = false;
    } else {
      const perm = (typeof Notification !== "undefined") ? Notification.permission : "default";
      pushLabel.textContent = perm === "denied"
        ? "Notifications blocked — enable in Settings"
        : "Enable push notifications";
      pushEnableBtn.textContent = "Enable";
      pushEnableBtn.dataset.action = "enable";
      pushEnableBtn.disabled = (perm === "denied");
      pushTestRow.hidden = true;
    }
  }

  pushEnableBtn.addEventListener("click", async () => {
    if (pushEnableBtn.dataset.action === "disable") {
      pushEnableBtn.disabled = true;
      try {
        const sub = await swRegistration.pushManager.getSubscription();
        if (sub) {
          await fetch("/api/push/unsubscribe", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ endpoint: sub.endpoint }),
          });
          await sub.unsubscribe();
        }
      } catch (err) {
        console.error(err);
        showError("Could not unsubscribe: " + err.message);
      }
      pushEnableBtn.disabled = false;
      await refreshPushUI();
      return;
    }

    pushEnableBtn.disabled = true;
    try {
      const perm = await Notification.requestPermission();
      if (perm !== "granted") {
        await refreshPushUI();
        return;
      }
      const keyResp = await fetch("/api/push/vapid-key");
      if (!keyResp.ok) throw new Error("vapid-key HTTP " + keyResp.status);
      const { publicKey } = await keyResp.json();
      const sub = await swRegistration.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: urlBase64ToUint8Array(publicKey),
      });
      const subResp = await fetch("/api/push/subscribe", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(sub.toJSON()),
      });
      if (!subResp.ok) throw new Error("subscribe HTTP " + subResp.status);
    } catch (err) {
      console.error(err);
      showError("Could not enable notifications: " + err.message);
    }
    pushEnableBtn.disabled = false;
    await refreshPushUI();
  });

  pushTestBtn.addEventListener("click", async () => {
    pushTestBtn.disabled = true;
    try {
      const resp = await fetch("/api/push/test", { method: "POST" });
      if (!resp.ok) throw new Error("HTTP " + resp.status);
    } catch (err) {
      console.error(err);
      showError("Test push failed: " + err.message);
    }
    pushTestBtn.disabled = false;
  });

  // --- Schedule editor ---

  // Detect simple daily cron `M H * * *` and return "HH:MM", else null.
  function cronToTime(cronStr) {
    const parts = cronStr.trim().split(/\s+/);
    if (parts.length !== 5) return null;
    const [m, h, dom, mon, dow] = parts;
    if (dom !== "*" || mon !== "*" || dow !== "*") return null;
    if (!/^\d+$/.test(m) || !/^\d+$/.test(h)) return null;
    return String(h).padStart(2, "0") + ":" + String(m).padStart(2, "0");
  }
  function timeToCron(timeStr) {
    const m = /^(\d{1,2}):(\d{2})$/.exec(timeStr);
    if (!m) return null;
    return `${parseInt(m[2], 10)} ${parseInt(m[1], 10)} * * *`;
  }

  async function loadSchedule() {
    try {
      const resp = await fetch("/api/schedule");
      if (!resp.ok) throw new Error("HTTP " + resp.status);
      scheduleEntries = await resp.json();
      renderSchedule();
    } catch (err) {
      console.error(err);
      scheduleBody.textContent = "Could not load schedule: " + err.message;
    }
  }

  function renderSchedule() {
    let html = "";
    scheduleEntries.forEach((entry, idx) => {
      const t = cronToTime(entry.cron);
      const isDaily = t !== null;
      html += `<div class="schedule-row${isDaily ? "" : " advanced"}">`;
      html += `<span class="schedule-row-label">${escapeHtml(entry.name || "(unnamed)")}</span>`;
      if (isDaily) {
        html += `<input type="time" class="schedule-row-time" data-idx="${idx}" value="${t}">`;
      } else {
        html += `<input type="text" class="schedule-row-time" data-idx="${idx}" value="${escapeHtml(entry.cron)}">`;
      }
      html += `</div>`;
    });
    html += `<div class="schedule-save">`;
    html += `<span class="schedule-status" id="schedule-status"></span>`;
    html += `<button type="button" class="push-btn" id="schedule-save-btn">Save</button>`;
    html += `</div>`;
    scheduleBody.innerHTML = html;

    scheduleBody.querySelectorAll(".schedule-row-time").forEach((el) => {
      el.addEventListener("change", (e) => {
        const idx = parseInt(e.target.dataset.idx, 10);
        const val = e.target.value;
        const isDaily = e.target.type === "time";
        if (isDaily) {
          const cron = timeToCron(val);
          if (cron) scheduleEntries[idx].cron = cron;
        } else {
          scheduleEntries[idx].cron = val;
        }
      });
    });
    const saveBtn = document.getElementById("schedule-save-btn");
    saveBtn.addEventListener("click", async () => {
      const statusEl = document.getElementById("schedule-status");
      saveBtn.disabled = true;
      statusEl.textContent = "Saving…";
      try {
        const resp = await fetch("/api/schedule", {
          method: "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(scheduleEntries),
        });
        if (!resp.ok) {
          const t = await resp.text();
          throw new Error(t || ("HTTP " + resp.status));
        }
        scheduleEntries = await resp.json();
        renderSchedule();
        const s = document.getElementById("schedule-status");
        if (s) s.textContent = "Saved.";
      } catch (err) {
        console.error(err);
        statusEl.textContent = "Save failed: " + err.message;
      } finally {
        saveBtn.disabled = false;
      }
    });
  }

  initPWA();

  // Hero video lifecycle. iOS PWA in standalone mode sometimes pauses
  // on the first frame even with `muted playsinline autoplay`; nudge it
  // explicitly when the video is loaded, and again on the first touch as
  // a fallback if iOS refused the autoplay.
  (function () {
    var v = document.querySelector(".hero-video");
    if (!v) return;
    var tryPlay = function () { v.play().catch(function () {}); };
    if (v.readyState >= 2) {
      tryPlay();
    } else {
      v.addEventListener("loadeddata", tryPlay, { once: true });
      v.addEventListener("canplay", tryPlay, { once: true });
    }
    // First user gesture unblocks playback if iOS denied autoplay.
    var onGesture = function () {
      tryPlay();
      document.removeEventListener("touchstart", onGesture);
      document.removeEventListener("click", onGesture);
    };
    document.addEventListener("touchstart", onGesture, { passive: true });
    document.addEventListener("click", onGesture);

    // Pause when the PWA is backgrounded to save CPU; resume on return.
    document.addEventListener("visibilitychange", function () {
      if (document.hidden) v.pause();
      else tryPlay();
    });
  })();

  // Start polling
  fetchStatus();
})();
