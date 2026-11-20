// Project filter checklist: reveals "Select all" and keeps it in step with the
// per-project boxes. Checking "Select all" checks every project, clearing it clears
// them; checking or clearing a project sets "Select all" to whether every project is
// now checked. Without this script "Select all" stays hidden and disabled, and the
// per-project boxes alone decide the selection (all checked = every project).
(function () {
  "use strict";
  document.addEventListener("DOMContentLoaded", function () {
    var all = document.querySelector("[data-filter-all]");
    if (!all) {
      return;
    }
    var boxes = Array.prototype.slice.call(document.querySelectorAll("[data-filter-project]"));
    all.disabled = false;
    all.closest("label").hidden = false;
    all.addEventListener("change", function () {
      boxes.forEach(function (b) { b.checked = all.checked; });
    });
    boxes.forEach(function (b) {
      b.addEventListener("change", function () {
        all.checked = boxes.every(function (x) { return x.checked; });
      });
    });
  });
})();
