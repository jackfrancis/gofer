// Copy buttons on the Discuss thread's assistant replies. Each button ships hidden
// with the reply's Markdown source in data-copy; this script reveals and wires it.
// Without this script the buttons stay hidden and the thread is unchanged.
(function () {
  "use strict";

  var FEEDBACK_MS = 1500;

  // Last-resort copy for contexts without the async Clipboard API (plain-HTTP
  // origins other than localhost): a transient off-screen textarea + execCommand.
  function legacyCopy(text) {
    var ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.style.position = "fixed";
    ta.style.top = "-1000px";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    var ok = false;
    try { ok = document.execCommand("copy"); } catch (e) { ok = false; }
    document.body.removeChild(ta);
    return ok ? Promise.resolve() : Promise.reject(new Error("copy failed"));
  }

  // Copy an assistant reply. text/plain carries the reply's Markdown source (what
  // pastes into a GitHub comment box); where supported, text/html carries the
  // rendered, already-sanitized bubble so rich editors keep the formatting. Each
  // path that the browser rejects (unsupported type, denied permission, embedded
  // webview) falls through to the next, ending at the textarea copy.
  function copyReply(markdown, bodyEl) {
    var clip = navigator.clipboard;
    if (!clip || !window.isSecureContext) {
      return legacyCopy(markdown);
    }
    var attempt;
    if (bodyEl && typeof ClipboardItem !== "undefined" && clip.write) {
      var item = new ClipboardItem({
        "text/plain": new Blob([markdown], { type: "text/plain" }),
        "text/html": new Blob([bodyEl.innerHTML], { type: "text/html" })
      });
      attempt = clip.write([item]).catch(function () { return clip.writeText(markdown); });
    } else {
      attempt = clip.writeText(markdown);
    }
    return attempt.catch(function () { return legacyCopy(markdown); });
  }

  function wireCopyButtons() {
    var buttons = document.querySelectorAll(".zz-copy[data-copy]");
    Array.prototype.forEach.call(buttons, function (btn) {
      var label = btn.textContent;
      var timer = 0;
      btn.addEventListener("click", function () {
        var msg = btn.closest(".zz-msg");
        var body = msg ? msg.querySelector(".zz-msgbody") : null;
        copyReply(btn.getAttribute("data-copy"), body).then(
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
