// GitHub avatars: the signed-in user's in the header, and each work item's author.
// The page ships only their URLs (data-avatar on the user's name and on each
// author's login) and this script adds the images once the page has finished loading.
// An <img> in the markup would hold up the load event until the avatar arrived, and
// a meta refresh (the processing and pending-reply views poll with one) waits for
// that event, so a slow avatar host would delay the page noticing finished work.
// Without this script the page shows the names alone.
(function () {
  "use strict";

  function addTo(el) {
    var img = document.createElement("img");
    img.className = "zz-avatar";
    img.alt = "";
    img.width = 24;
    img.height = 24;
    img.referrerPolicy = "no-referrer";
    img.decoding = "async";
    img.loading = "lazy";
    // A failed load leaves the name alone, not a broken-image glyph.
    img.addEventListener("error", function () {
      img.remove();
    });
    img.src = el.getAttribute("data-avatar");
    el.insertBefore(img, el.firstChild);
  }

  function add() {
    var els = document.querySelectorAll(".zz-user[data-avatar], .zz-author[data-avatar]");
    for (var i = 0; i < els.length; i++) {
      addTo(els[i]);
    }
  }

  if (document.readyState === "complete") {
    add();
  } else {
    window.addEventListener("load", add);
  }
})();
