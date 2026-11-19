// Project filter checklist: keeps "Select all" and the per-project boxes in step.
// Checking "Select all" checks every project, clearing it clears them; checking or
// clearing a project sets "Select all" to whether every project is now checked.
// Without this script the form still works: the server treats a checked
// "Select all" as showing every project.
(function () {
  "use strict";
  document.addEventListener("DOMContentLoaded", function () {
    var all = document.querySelector("[data-filter-all]");
    if (!all) {
      return;
    }
    var boxes = Array.prototype.slice.call(document.querySelectorAll("[data-filter-project]"));
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
