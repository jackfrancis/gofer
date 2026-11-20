// Worklist reload while a GitHub refresh runs: the page reloads itself every few
// seconds (data-reload-secs on this script's tag) so new work appears without a
// click, and stops once the server renders the page without this script. It waits
// while the user is mid-choice, so a reload never discards a half-made selection: a
// menu (<details>, such as the project filter or a model picker) is open, or a form
// control has focus. Without script, a <noscript> meta refresh reloads instead.
(function () {
  "use strict";
  var el = document.currentScript;
  var secs = parseInt(el && el.getAttribute("data-reload-secs"), 10);
  if (!(secs > 0)) {
    return;
  }
  function busy() {
    if (document.querySelector("details[open]")) {
      return true;
    }
    var a = document.activeElement;
    return !!(a && a.form && a.matches("input, select, textarea"));
  }
  function tick() {
    if (busy()) {
      setTimeout(tick, secs * 1000);
      return;
    }
    window.location.reload();
  }
  setTimeout(tick, secs * 1000);
})();
