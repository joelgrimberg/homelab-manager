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
  const toggle = document.getElementById("toggle");
  const toggleInput = document.getElementById("toggle-input");
  const spinner = document.getElementById("spinner");
  const tiersEl = document.getElementById("tiers");
  const lastUpdated = document.getElementById("last-updated");
  const labelSleep = document.getElementById("toggle-label-sleep");
  const labelWake = document.getElementById("toggle-label-wake");
  const advancedInput = document.getElementById("advanced-input");

  advancedInput.addEventListener("change", function () {
    advancedMode = advancedInput.checked;
    if (currentState) renderTiers(currentState.tiers);
  });

  toggleInput.addEventListener("change", function () {
    if (toggle.classList.contains("disabled")) {
      toggleInput.checked = !toggleInput.checked;
      return;
    }

    const action = toggleInput.checked ? "wake" : "sleep";
    toggle.classList.add("disabled", "transitioning");

    fetch("/api/" + action, { method: "POST" })
      .then(function (resp) {
        if (!resp.ok) throw new Error("Failed to " + action);
        pollInterval = POLL_FAST;
        schedulePoll();
      })
      .catch(function (err) {
        console.error(err);
        toggleInput.checked = !toggleInput.checked;
        toggle.classList.remove("disabled", "transitioning");
        showError("Failed to " + action + " homelab");
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

    // Toggle
    const isAwake = data.state === "awake";
    const isSleeping = data.state === "asleep";
    toggleInput.checked = isAwake || (data.transitioning && data.state === "transitioning" && toggleInput.checked);

    if (data.transitioning) {
      toggle.classList.add("disabled", "transitioning");
    } else {
      toggle.classList.remove("disabled", "transitioning");
    }

    // If mixed but not transitioning, allow toggling
    if (data.state === "mixed") {
      toggle.classList.remove("disabled");
      // Determine toggle position based on majority
      var running = 0, total = 0;
      data.tiers.forEach(function (tier) {
        tier.instances.forEach(function (inst) {
          total++;
          if (inst.status === "running") running++;
        });
      });
      toggleInput.checked = running > total / 2;
    }

    // Labels
    labelSleep.classList.toggle("active", isSleeping);
    labelWake.classList.toggle("active", isAwake);

    // Tiers
    renderTiers(data.tiers);

    // Last updated
    var now = new Date();
    lastUpdated.textContent = "Updated " + now.toLocaleTimeString();
  }

  function renderTiers(tiers) {
    // Preserve existing expanded state; default tier 1 to expanded on first render
    if (Object.keys(expanded).length === 0) {
      tiers.forEach(function (tier, i) {
        expanded[tier.tier] = i === 0;
      });
    }

    var html = "";
    tiers.forEach(function (tier) {
      var isExpanded = expanded[tier.tier];

      // Roll up instance statuses → tier state
      var running = 0, stopped = 0;
      tier.instances.forEach(function (inst) {
        if (inst.status === "running") running++;
        else if (inst.status === "stopped") stopped++;
      });
      var tierChecked = running > 0 && running >= stopped; // majority-running
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

      tier.instances.forEach(function (inst) {
        var dotClass = inst.status;
        if (inst.status !== "running" && inst.status !== "stopped") {
          dotClass = "transitioning";
        }

        // During a transition, override dot classes based on tier progress
        if (currentState && currentState.transitioning && currentState.current_tier > 0) {
          var dir = currentState.direction;
          var curTier = currentState.current_tier;
          if (dir === "waking") {
            // Tiers after current: pending (haven't started yet)
            if (tier.tier > curTier) {
              dotClass = "pending";
            }
            // Current tier: show processing if not yet running
            else if (tier.tier === curTier && inst.status !== "running") {
              dotClass = "processing";
            }
          } else if (dir === "sleeping") {
            // Tiers before current: pending (haven't been stopped yet)
            if (tier.tier < curTier) {
              dotClass = "pending";
            }
            // Current tier: show stopping if not yet stopped
            else if (tier.tier === curTier && inst.status !== "stopped") {
              dotClass = "stopping";
            }
          }
        }

        html += '<div class="instance">';
        html += '<span class="status-dot ' + dotClass + '"></span>';
        html += '<span class="instance-name">' + escapeHtml(inst.name) + "</span>";
        html += '<span class="instance-meta">' + inst.type + "</span>";

        if (advancedMode) {
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
      });

      html += "</div></div>";
    });

    tiersEl.innerHTML = html;
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

  // Instance + tier toggle handler (event delegation)
  tiersEl.addEventListener("change", function (e) {
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
  });

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

  // Start polling
  fetchStatus();
})();
