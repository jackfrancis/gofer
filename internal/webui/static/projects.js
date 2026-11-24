// Filter checklists (projects, labels): reveals each menu's "Select all" and keeps it in
// step with that menu's own boxes. Checking "Select all" checks every option in its
// menu, clearing it clears them; checking or clearing an option sets "Select all" to
// whether every option in the menu is now checked. Without this script "Select all"
// stays hidden and disabled, and the option boxes alone decide the selection (all
// checked = everything).
(function () {
  "use strict";
  document.addEventListener("DOMContentLoaded", function () {
    Array.prototype.forEach.call(document.querySelectorAll("[data-filter-all]"), function (all) {
      var form = all.closest("form");
      if (!form) {
        return;
      }
      var boxes = Array.prototype.slice.call(form.querySelectorAll("[data-filter-project], [data-filter-label]"));
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
  });
})();
