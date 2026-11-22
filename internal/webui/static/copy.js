// Copy buttons on the Discuss thread's assistant replies. Each button ships hidden
// with the reply's Markdown source in data-copy; this script reveals and wires it.
// Without this script the buttons stay hidden and the thread is unchanged.
(function () {
  "use strict";

  var FEEDBACK_MS = 1500;

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

  function wireCopyButtons() {
    var buttons = document.querySelectorAll(".zz-copy[data-copy]");
    Array.prototype.forEach.call(buttons, function (btn) {
      var label = btn.textContent;
      var timer = 0;
      btn.addEventListener("click", function () {
        copyReply(btn.getAttribute("data-copy")).then(
          function () { btn.textContent = "Copied"; },
          function () { btn.textContent = "Copy failed"; }
        ).then(function () {
          clearTimeout(timer);
          timer = setTimeout(function () { btn.textContent = label; }, FEEDBACK_MS);
        });
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
