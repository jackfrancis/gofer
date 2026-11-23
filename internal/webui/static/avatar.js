// The header's GitHub avatar. The page ships only its URL (data-avatar on the
// user's name) and this script adds the image once the page has finished loading.
// An <img> in the markup would hold up the load event until the avatar arrived, and
// a meta refresh (the processing and pending-reply views poll with one) waits for
// that event, so a slow avatar host would delay the page noticing finished work.
// Without this script the header shows the name alone.
(function () {
  "use strict";

  function add() {
    var el = document.querySelector(".zz-user[data-avatar]");
    if (!el) {
      return;
    }
    var img = document.createElement("img");
    img.className = "zz-avatar";
    img.alt = "";
    img.width = 24;
    img.height = 24;
    img.referrerPolicy = "no-referrer";
    img.decoding = "async";
    // A failed load leaves the name alone, not a broken-image glyph.
    img.addEventListener("error", function () {
      img.remove();
    });
    img.src = el.getAttribute("data-avatar");
    el.insertBefore(img, el.firstChild);
  }

  if (document.readyState === "complete") {
    add();
  } else {
    window.addEventListener("load", add);
  }
})();
