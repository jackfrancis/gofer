// Bulk comment box (ADR 0009). Progressive enhancement over a plain form: without
// this script the checkboxes and the textarea still post, and the server previews.
// With it, the box shows how many items are checked, and typing "@" suggests logins
// that can be assigned in the checked items' repositories (GET /items/mentions),
// ranked by how many of them can assign each. The count is always against every
// selected repository: one that cannot be assigned in all of them says so, since a
// /assign for it would fail in some, and repositories gofer could not check are
// shown as unknown, never as assignable.
(function () {
  "use strict";
  var form = document.getElementById("zz-bulk-comment");
  if (!form) {
    return;
  }
  var box = form.querySelector("textarea[data-mentions]");
  var count = document.querySelector("[data-selected-count]");
  var maxShown = 8;

  function checked() {
    return Array.prototype.slice.call(document.querySelectorAll("[data-comment-item]:checked"));
  }
  function updateCount() {
    if (!count) {
      return;
    }
    var n = checked().length;
    count.textContent = n ? "(" + n + " selected)" : "(none selected)";
  }
  document.addEventListener("change", function (e) {
    if (e.target && e.target.hasAttribute && e.target.hasAttribute("data-comment-item")) {
      updateCount();
    }
  });
  updateCount();

  if (!box || !window.fetch) {
    return;
  }

  // --- @ suggestions -------------------------------------------------------------
  var list = document.createElement("ul");
  list.className = "zz-mentions";
  list.id = "zz-mentions";
  list.setAttribute("role", "listbox");
  list.hidden = true;
  box.parentNode.appendChild(list);
  box.setAttribute("aria-autocomplete", "list");
  box.setAttribute("aria-controls", list.id);
  box.setAttribute("aria-expanded", "false");

  var cache = {};     // repo-set key -> Promise of the server's answer
  var shown = [];     // the suggestions in the list
  var active = -1;    // the highlighted one
  var token = null;   // {start, end, prefix} of the @word at the caret
  var seq = 0;        // drops answers to stale lookups

  function repos() {
    var seen = {};
    var out = [];
    checked().forEach(function (c) {
      var r = c.getAttribute("data-repo");
      if (r && !seen[r.toLowerCase()]) {
        seen[r.toLowerCase()] = true;
        out.push(r);
      }
    });
    return out.sort();
  }
  function lookup(rs) {
    var key = rs.join(",");
    if (!cache[key]) {
      var q = rs.map(function (r) { return "repo=" + encodeURIComponent(r); }).join("&");
      cache[key] = fetch("/items/mentions?" + q, { credentials: "same-origin" })
        .then(function (r) {
          if (!r.ok) {
            throw new Error("status " + r.status);
          }
          return r.json();
        })
        .catch(function (err) {
          delete cache[key]; // try again next time
          throw err;
        });
    }
    return cache[key];
  }
  // The @word ending at the caret: "@" at the start or after whitespace or "(",
  // then GitHub login characters.
  function currentToken() {
    if (box.selectionStart !== box.selectionEnd) {
      return null;
    }
    var end = box.selectionStart;
    var before = box.value.slice(0, end);
    var m = /(^|[\s(])@([A-Za-z0-9-]{0,39})$/.exec(before);
    if (!m) {
      return null;
    }
    return { start: end - m[2].length - 1, end: end, prefix: m[2].toLowerCase() };
  }
  function close() {
    list.hidden = true;
    list.textContent = "";
    shown = [];
    active = -1;
    box.setAttribute("aria-expanded", "false");
    box.removeAttribute("aria-activedescendant");
  }
  function note(text) {
    list.textContent = "";
    var li = document.createElement("li");
    li.className = "zz-mention-empty";
    li.textContent = text;
    list.appendChild(li);
    list.hidden = false;
    box.setAttribute("aria-expanded", "true");
  }
  function highlight(i) {
    var items = list.querySelectorAll("[role=option]");
    for (var k = 0; k < items.length; k++) {
      items[k].setAttribute("aria-selected", k === i ? "true" : "false");
    }
    active = i;
    if (items[i]) {
      box.setAttribute("aria-activedescendant", items[i].id);
      items[i].scrollIntoView({ block: "nearest" });
    }
  }
  function render(answer, total) {
    list.textContent = "";
    var unchecked = total - (answer.checked || 0);
    if (!shown.length) {
      note((token.prefix ? "No assignable user starts with “" + token.prefix + "”" : "No assignable users found") +
        (unchecked > 0 ? " (" + unchecked + " of " + total + " repositories not checked)" : ""));
      return;
    }
    shown.forEach(function (u, i) {
      var li = document.createElement("li");
      li.id = "zz-mention-" + i;
      li.setAttribute("role", "option");
      var name = document.createElement("span");
      name.textContent = "@" + u.login;
      li.appendChild(name);
      if (u.repos < total) {
        var n = document.createElement("span");
        n.className = "zz-mention-note";
        n.textContent = "assignable in " + u.repos + " of " + total +
          (u.unknown ? "; " + u.unknown + " not checked" : "");
        li.appendChild(n);
      }
      li.addEventListener("mousedown", function (e) {
        e.preventDefault(); // keep focus (and the caret) in the box
        choose(i);
      });
      list.appendChild(li);
    });
    list.hidden = false;
    box.setAttribute("aria-expanded", "true");
    highlight(0);
  }
  function suggest() {
    token = currentToken();
    if (!token) {
      close();
      return;
    }
    var rs = repos();
    if (!rs.length) {
      note("Check an item to see who can be assigned in its repository");
      return;
    }
    var mine = ++seq;
    var t = token;
    lookup(rs).then(function (answer) {
      if (mine !== seq) {
        return;
      }
      var users = answer.users || [];
      var starts = [];
      var contains = [];
      users.forEach(function (u) {
        var l = u.login.toLowerCase();
        if (l.indexOf(t.prefix) === 0) {
          starts.push(u);
        } else if (t.prefix && l.indexOf(t.prefix) > 0) {
          contains.push(u);
        }
      });
      shown = starts.concat(contains).slice(0, maxShown);
      render(answer, answer.selected || rs.length);
    }, function () {
      if (mine === seq) {
        note("Suggestions are unavailable right now");
      }
    });
  }
  function choose(i) {
    var u = shown[i];
    if (!u || !token) {
      return;
    }
    var v = box.value;
    var insert = "@" + u.login + " ";
    box.value = v.slice(0, token.start) + insert + v.slice(token.end);
    var caret = token.start + insert.length;
    box.setSelectionRange(caret, caret);
    close();
    box.focus();
  }

  box.addEventListener("input", suggest);
  box.addEventListener("click", suggest);
  box.addEventListener("blur", function () { setTimeout(close, 100); });
  box.addEventListener("keydown", function (e) {
    if (list.hidden || !shown.length) {
      if (e.key === "Escape" && !list.hidden) {
        close();
      }
      return;
    }
    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        highlight((active + 1) % shown.length);
        break;
      case "ArrowUp":
        e.preventDefault();
        highlight((active - 1 + shown.length) % shown.length);
        break;
      case "Enter":
      case "Tab":
        if (active >= 0) {
          e.preventDefault();
          choose(active);
        }
        break;
      case "Escape":
        e.preventDefault();
        close();
        break;
    }
  });
  // A changed selection changes whom to suggest.
  document.addEventListener("change", function (e) {
    if (e.target && e.target.hasAttribute && e.target.hasAttribute("data-comment-item") && !list.hidden) {
      suggest();
    }
  });
})();
