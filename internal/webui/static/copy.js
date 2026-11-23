// Copy buttons on the Discuss thread's assistant replies. Each button ships hidden
// with the reply's Markdown source in data-copy; this script reveals and wires it.
// Without this script the buttons stay hidden and the thread is unchanged.
(function () {
  "use strict";

  var FEEDBACK_MS = 1500;
  // A failure stays up longer: the user has to notice it and copy another way.
  var FAILURE_MS = 4000;

  // Last-resort copy for contexts where the async Clipboard API is missing or
  // rejected (plain-HTTP origins other than localhost, denied permission, embedded
  // webviews): a transient off-screen textarea + execCommand. Selecting the textarea
  // moves focus to it, and removing it would drop focus to <body>, so the element
  // that had focus (normally the Copy button) gets it back either way.
  function legacyCopy(text) {
    var prev = document.activeElement;
    var ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.style.position = "fixed";
    ta.style.top = "-1000px";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    var ok = false;
    try {
      ta.select();
      ok = document.execCommand("copy");
    } catch (e) {
      ok = false;
    } finally {
      document.body.removeChild(ta);
      if (prev && typeof prev.focus === "function") {
        prev.focus({ preventScroll: true });
      }
    }
    return ok ? Promise.resolve() : Promise.reject(new Error("copy failed"));
  }

  // Copy an assistant reply's Markdown source as text/plain only. Never add a
  // text/html flavor of the rendered reply: GitHub's comment box converts pasted
  // HTML links back to Markdown, so a reply's [label](url) would paste as
  // [[label](url)](url).
  function copyReply(markdown) {
    var clip = navigator.clipboard;
    if (!clip || !clip.writeText || !window.isSecureContext) {
      return legacyCopy(markdown);
    }
    return clip.writeText(markdown).catch(function () { return legacyCopy(markdown); });
  }

  // One shared, visually hidden live region announces each outcome, since an
  // icon swap alone says nothing to a screen reader.
  var status = null;
  function announce(text) {
    if (!status) {
      status = document.createElement("div");
      status.className = "zz-sr";
      status.setAttribute("role", "status");
      document.body.appendChild(status);
    }
    status.textContent = "";
    setTimeout(function () { status.textContent = text; }, 50);
  }

  // The button is icon-only: copy.js shows the outcome by class (is-copied swaps in
  // the check; is-failed swaps in an alert glyph and the visible words "Copy
  // failed") and in the tooltip, then restores both.
  function wireCopyButtons() {
    var buttons = document.querySelectorAll(".zz-copy[data-copy]");
    Array.prototype.forEach.call(buttons, function (btn) {
      var title = btn.getAttribute("title");
      var timer = 0;
      function show(cls, text, ms) {
        clearTimeout(timer);
        btn.classList.remove("is-copied", "is-failed");
        btn.classList.add(cls);
        btn.setAttribute("title", text);
        announce(text);
        timer = setTimeout(function () {
          btn.classList.remove(cls);
          btn.setAttribute("title", title);
        }, ms);
      }
      btn.addEventListener("click", function () {
        copyReply(btn.getAttribute("data-copy")).then(
          function () { show("is-copied", "Copied", FEEDBACK_MS); },
          function () { show("is-failed", "Copy failed", FAILURE_MS); }
        );
      });
      btn.hidden = false;
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", wireCopyButtons);
  } else {
    wireCopyButtons();
  }
})();
