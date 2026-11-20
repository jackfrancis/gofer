// Worklist refresh watcher. The page is rendered with the refresh version read before
// its items (data-done: every refresh up to it had ended) and whether one looked in
// flight (data-active). This script polls /items/refresh-status with that version —
// every data-poll-secs while a refresh runs, every data-idle-poll-secs otherwise —
// and reloads the page once the server says a refresh has ended since, so new work
// appears without a click: from a scheduled pass that started and ended between two
// polls, or from one that ended while the next was already running. It shows "refreshing from GitHub…" while one runs. It never reloads
// while the user is mid-choice: a menu (<details>, such as the project filter or a
// model picker) is open, or a form control has focus. A failed poll keeps polling.
// Without script, a <noscript> meta refresh reloads during a refresh shown at render.
(function () {
  "use strict";
  var el = document.currentScript;
  if (!el || !window.fetch) {
    return;
  }
  var done = el.getAttribute("data-done") || "0";
  var active = el.getAttribute("data-active") === "1";
  var poll = parseInt(el.getAttribute("data-poll-secs"), 10) || 3;
  var idle = parseInt(el.getAttribute("data-idle-poll-secs"), 10) || 30;

  var url = "/items/refresh-status?done=" + encodeURIComponent(done);

  function busy() {
    if (document.querySelector("details[open]")) {
      return true;
    }
    var a = document.activeElement;
    return !!(a && a.form && a.matches("input, select, textarea"));
  }
  function showActive() {
    var s = document.querySelector("[data-refreshing]");
    if (s) {
      s.hidden = !active;
    }
  }
  function reloadWhenFree() {
    if (busy()) {
      setTimeout(reloadWhenFree, poll * 1000);
      return;
    }
    window.location.reload();
  }
  function schedule() {
    setTimeout(check, (active ? poll : idle) * 1000);
  }
  function check() {
    fetch(url, { credentials: "same-origin", cache: "no-store" })
      .then(function (r) { return r.json(); })
      .then(function (s) {
        if (s.reload) {
          reloadWhenFree();
          return;
        }
        active = !!s.active;
        showActive();
        schedule();
      })
      .catch(schedule);
  }
  schedule();
})();
