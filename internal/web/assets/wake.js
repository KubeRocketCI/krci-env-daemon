/* Waiting page of a sleeping KubeRocketCI environment.
 *
 * Runs on the environment's own origin for a visitor who has not signed in:
 *   - It requests this origin only: /.krci-wake/status for the progress, and the root of
 *     the host to learn whether the gateway routes to the workloads yet. The CSP the page
 *     is sent with (connect-src 'self') forbids any other destination.
 *   - Status calls use credentials: 'omit', so the daemon never sees cookies.
 *   - No response data reaches innerHTML. Names must be Deployment names (DNS
 *     subdomains) and are written with textContent.
 */
(function () {
  "use strict";

  var STATUS = "/.krci-wake/status";
  var POLL_MS = 1000;
  var FAIL_LIMIT = 4;
  var MAX_COMPONENTS = 64;
  var NAME = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$/;
  var NAME_MAX = 253;
  // Transition times from wake.css. The green holds below outlast --tint, so the change
  // of colour is always seen in full.
  var T_TINT = parseFloat(getComputedStyle(document.documentElement).getPropertyValue("--tint"));
  var T_FADE = parseFloat(getComputedStyle(document.documentElement).getPropertyValue("--fade"));
  var T_MIN_RED = 500;              // a component that came up quickly still shows red this long
  var T_GREEN = T_TINT + 500;       // how long a component stays green before the next slides in
  var T_GREEN_FAST = T_TINT + 250;  // the same, while more components are already up

  function $(sel) { return document.querySelector(sel); }
  var ring = $(".ring"), readOut = $(".count .r"), totalOut = $(".count .t");
  var slot = $(".slot"), slotName = $(".slot .name"), slotState = $(".slot .state");
  var pips = $(".pips"), headline = $(".headline"), netfail = $(".netfail");

  var DONE = -2;
  var comps = [];    // [{name, ready}] in the daemon's order
  var queue = [];    // indexes not shown in the slot yet
  var cur = -1;      // component in the slot; DONE once every one was shown
  var busy = false;  // a slot transition is running
  var shownAt = 0;
  var fails = 0;
  var leaving = false;

  function isReady(i) { return comps[i].ready; }
  function readyCount() { return comps.filter(function (c) { return c.ready; }).length; }

  function paint() {
    var total = comps.length, ready = readyCount();
    readOut.textContent = String(ready);
    totalOut.textContent = total ? String(total) : "-";
    ring.style.setProperty("--pct", (total ? (ready * 100) / total : 0) + "%");
    while (pips.children.length < total) pips.appendChild(document.createElement("span"));
    while (pips.children.length > total) pips.removeChild(pips.lastChild);
    for (var i = 0; i < total; i++) {
      pips.children[i].className = comps[i].ready ? "on" : i === cur ? "now" : "";
    }
  }

  function setSlot(name, up) {
    slotName.textContent = name;
    slotState.textContent = up ? "ready" : "starting";
    slot.classList.toggle("up", up);
  }

  // The next component for the slot: one that is already up goes first, so the
  // slot keeps moving while a slow one starts; otherwise the daemon's order.
  function pickNext() {
    if (!queue.length) return DONE;
    return queue.splice(Math.max(0, queue.findIndex(isReady)), 1)[0];
  }

  function enter(i) {
    cur = i;
    shownAt = Date.now();
    setSlot(i >= 0 ? comps[i].name : "all components", i < 0);
  }

  // Moves the slot forward when its component is Ready. Called after every
  // status answer and after every transition.
  function step() {
    if (busy || leaving || cur < 0 || !comps[cur].ready) return;
    busy = true;
    var green = queue.filter(isReady).length > 1 ? T_GREEN_FAST : T_GREEN;
    setTimeout(function () {
      slot.classList.add("up");          // the dot glides from red to green
      slotState.textContent = "ready";
      setTimeout(function () {
        if (leaving) return;
        slot.classList.add("fade");
        setTimeout(function () {
          if (leaving) return;
          enter(pickNext());               // swapped while invisible, no colour transition
          void slot.offsetWidth;
          requestAnimationFrame(function () {
            slot.classList.remove("fade");
            busy = false;
            paint();
            if (cur === DONE || readyCount() === comps.length) finish();
            else step();
          });
        }, T_FADE);
      }, green);
    }, Math.max(0, T_MIN_RED - (Date.now() - shownAt)));
  }

  function merge(list) {
    if (!comps.length) {
      comps = list;
      for (var q = 0; q < comps.length; q++) queue.push(q);
      if (comps.length) enter(pickNext());
    } else {
      var byName = Object.create(null);
      for (var i = 0; i < list.length; i++) byName[list[i].name] = list[i].ready;
      for (var j = 0; j < comps.length; j++) {
        if (comps[j].name in byName) comps[j].ready = byName[comps[j].name];
      }
    }
    paint();
    if (readyCount() === comps.length) finish();
    else step();
  }

  function valid(doc) {
    if (!doc || typeof doc !== "object" || !Array.isArray(doc.components)) return null;
    var out = [];
    for (var i = 0; i < doc.components.length && out.length < MAX_COMPONENTS; i++) {
      var c = doc.components[i];
      if (c && typeof c.name === "string" && c.name.length <= NAME_MAX && NAME.test(c.name)) out.push({ name: c.name, ready: c.ready === true });
    }
    return out;
  }

  function offline(on) {
    netfail.hidden = !on;
    document.body.classList.toggle("offline", on);
  }

  function poll() {
    if (leaving) return;
    fetch(STATUS, { cache: "no-store", credentials: "omit", redirect: "error", headers: { Accept: "application/json" } })
      .then(function (r) {
        if (!r.ok) throw new Error("status " + r.status);
        return r.json();
      })
      .then(function (doc) {
        var list = valid(doc);
        if (!list) throw new Error("unexpected answer");
        fails = 0;
        offline(false);
        merge(list);
      })
      .catch(function () {
        if (++fails >= FAIL_LIMIT) offline(true);
      })
      .then(function () {
        if (!leaving) setTimeout(poll, fails ? Math.min(5000, POLL_MS * (fails + 1)) : POLL_MS);
      });
  }

  function finish() {
    if (leaving) return;
    leaving = true;
    slot.classList.remove("fade");
    enter(DONE);
    paint();
    headline.textContent = "Opening the Portal";
    probe(0);
  }

  // The workloads can be Ready a moment before the gateway routes to them, so ask the
  // root of the host first: reloading into this page again would only flash. The page's
  // own URL is not asked, as it can be single-use, such as a sign-in callback.
  function probe(n) {
    fetch("/", { cache: "no-store", credentials: "same-origin", redirect: "manual" })
      .then(function (r) {
        if (r.type === "opaqueredirect" || r.status !== 503 || n >= 30) location.reload();
        else setTimeout(function () { probe(n + 1); }, 1000);
      })
      .catch(function () { setTimeout(function () { probe(n + 1); }, 1000); });
  }

  // Theme toggle for this page only; the Portal's stored preference is not touched.
  var toggle = $(".dock .theme");
  if (toggle) {
    toggle.addEventListener("click", function () {
      var root = document.documentElement;
      var explicit = root.getAttribute("data-theme");
      var dark = explicit ? explicit === "dark" : window.matchMedia("(prefers-color-scheme: dark)").matches;
      root.setAttribute("data-theme", dark ? "light" : "dark");
    });
  }

  poll();
})();
