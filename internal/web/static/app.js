// hangarr: minimal client script (CSP script-src 'self', no eval).
// Live password checklist: purely informative, the server stays the only
// source of truth and the password is never sent while typing.
(function () {
  "use strict";

  var rules = {
    length: function (pw) {
      var n = Array.from(pw.normalize("NFKC")).length;
      return n >= 12 && n <= 128;
    },
    lower: function (pw) { return /\p{Ll}/u.test(pw); },
    upper: function (pw) { return /\p{Lu}/u.test(pw); },
    digit: function (pw) { return /\p{Nd}/u.test(pw); },
    special: function (pw) { return /[^\p{L}\p{N}]/u.test(pw); },
    username: function (pw, user) {
      if (!pw) { return false; }
      user = (user || "").normalize("NFKC").toLowerCase();
      return user === "" || pw.normalize("NFKC").toLowerCase().indexOf(user) === -1;
    },
    match: function (pw, user, confirm) { return pw !== "" && pw === confirm; }
  };

  function update(form) {
    var pw = form.querySelector("[name=new_password]");
    var confirm = form.querySelector("[name=confirm_password]");
    if (!pw || !confirm) { return; }
    var user = form.getAttribute("data-username");
    form.querySelectorAll("[data-rule]").forEach(function (li) {
      var check = rules[li.getAttribute("data-rule")];
      li.classList.toggle("ok", !!check && check(pw.value, user, confirm.value));
    });
  }

  document.addEventListener("input", function (e) {
    var form = e.target.closest && e.target.closest("form[data-password-rules]");
    if (form) { update(form); }
  });

  function updateAll() {
    document.querySelectorAll("form[data-password-rules]").forEach(update);
  }
  document.addEventListener("DOMContentLoaded", function () {
    updateAll();
    // Library filters start collapsed on small screens.
    if (window.matchMedia("(max-width: 800px)").matches) {
      document.querySelectorAll("details.filters-panel").forEach(function (d) { d.open = false; });
    }
  });
  document.addEventListener("htmx:afterSwap", updateAll);
})();

// Modals (detail, delete confirmation), toasts and post-deletion updates.
(function () {
  "use strict";

  function openDialog(id) {
    var d = document.getElementById(id);
    if (d && !d.open) { d.showModal(); }
  }

  document.addEventListener("htmx:afterSwap", function (e) {
    var id = e.detail.target && e.detail.target.id;
    if (id === "modal-body") { openDialog("modal"); }
    if (id === "confirm-body") { openDialog("confirm"); }
  });

  document.addEventListener("click", function (e) {
    var closer = e.target.closest && e.target.closest("[data-close]");
    if (closer) {
      var dialog = closer.closest("dialog");
      if (dialog) { e.preventDefault(); dialog.close(); }
      return;
    }
    if (e.target.tagName === "DIALOG") { e.target.close(); } // backdrop click
  });

  function toast(message, level) {
    var box = document.getElementById("toasts");
    if (!box) { return; }
    var t = document.createElement("div");
    t.className = "toast toast-" + (level || "info");
    t.setAttribute("role", "status");
    t.textContent = message;
    box.appendChild(t);
    setTimeout(function () { t.remove(); }, 6000);
  }

  document.addEventListener("hangarr:toast", function (e) { toast(e.detail.message, e.detail.level); });

  // Protection changed: reload the detail (modal or page) to show the new state.
  document.addEventListener("hangarr:changed", function (e) {
    var modal = document.getElementById("modal");
    if (modal && modal.open) { htmx.ajax("GET", e.detail.detailUrl, { target: "#modal-body" }); } else { location.reload(); }
  });

  document.addEventListener("hangarr:deleted", function (e) {
    var d = e.detail;
    var confirm = document.getElementById("confirm");
    if (confirm && confirm.open) { confirm.close(); }
    if (d.dryRun) { return; }
    var modal = document.getElementById("modal");
    if (d.season !== null && d.season !== undefined) {
      if (modal && modal.open) { htmx.ajax("GET", d.detailUrl, { target: "#modal-body" }); } else { location.reload(); }
      return;
    }
    var card = document.getElementById("media-" + d.mediaId);
    if (card) { card.remove(); }
    if (modal && modal.open) { modal.close(); } else if (document.querySelector("[data-detail]")) { location.href = d.libraryUrl; }
  });
})();


// Batch selection: checkboxes (name=ids, form=batch-form) on cards and rows,
// sticky bar with count and size; cards removed after a batch deletion.
(function () {
  "use strict";

  function boxes() { return document.querySelectorAll("input[name=ids][form=batch-form]"); }

  // Same units and decimal separator as the server (i18n.Bytes).
  function formatBytes(n) {
    var fr = document.documentElement.lang === "fr";
    var units = fr ? ["o", "Ko", "Mo", "Go", "To", "Po"] : ["B", "kB", "MB", "GB", "TB", "PB"], i = 0;
    while (n >= 1000 && i < units.length - 1) { n /= 1000; i++; }
    if (i === 0) { return n + " " + units[0]; }
    var v = n.toFixed(n >= 100 ? 0 : 1);
    return (fr ? v.replace(".", ",") : v) + " " + units[i];
  }

  function update() {
    var bar = document.getElementById("batch-form");
    if (!bar) { return; }
    var count = 0, size = 0;
    boxes().forEach(function (b) {
      if (b.checked) { count++; size += parseInt(b.getAttribute("data-size") || "0", 10); }
    });
    bar.hidden = count === 0;
    var summary = bar.querySelector("[data-batch-summary]");
    if (summary) {
      var fr = document.documentElement.lang === "fr";
      var pattern = summary.getAttribute((count === 1 || (fr && count === 0)) ? "data-one" : "data-many");
      summary.textContent = pattern.replace("{n}", count) + " · " + formatBytes(size);
    }
  }

  document.addEventListener("change", function (e) {
    if (e.target.matches && e.target.matches("input[name=ids][form=batch-form]")) { update(); }
  });
  document.addEventListener("click", function (e) {
    if (e.target.closest && e.target.closest("[data-batch-clear]")) {
      boxes().forEach(function (b) { b.checked = false; });
      update();
    }
  });
  // New cards from infinite scroll or filters keep the bar in sync.
  document.addEventListener("htmx:afterSwap", update);
  document.addEventListener("DOMContentLoaded", update);

  var reloadOnClose = false;
  document.addEventListener("hangarr:batch-done", function (e) {
    (e.detail.deleted || []).forEach(function (id) {
      var el = document.getElementById("media-" + id);
      if (el) { el.remove(); }
    });
    boxes().forEach(function (b) { b.checked = false; });
    update();
    reloadOnClose = !!e.detail.reload;
  });
  document.addEventListener("close", function (e) {
    if (reloadOnClose && e.target.id === "confirm") { location.reload(); }
  }, true);
})();

// Theme button: switches between light and dark (the default follows the
// system) and remembers the choice in a cookie that the server reads to
// render <html data-theme> without a flash.
(function () {
  "use strict";

  document.addEventListener("click", function (e) {
    if (!e.target.closest || !e.target.closest("[data-theme-toggle]")) { return; }
    var root = document.documentElement;
    var current = root.getAttribute("data-theme") ||
      (window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
    var next = current === "dark" ? "light" : "dark";
    root.setAttribute("data-theme", next);
    document.cookie = "hangarr_theme=" + next + "; Path=/; Max-Age=31536000; SameSite=Lax" +
      (location.protocol === "https:" ? "; Secure" : "");
  });
})();

// Sections loaded later (hx-trigger="load", data-load-error): when the request
// fails (server error, network, timeout), HTMX swaps nothing and the spinner
// would stay forever; show the message and a link that reloads the page.
(function () {
  "use strict";

  function failed(e) {
    var elt = e.detail && e.detail.elt;
    if (!elt || !elt.hasAttribute || !elt.hasAttribute("data-load-error")) { return; }
    var p = document.createElement("p");
    p.className = "error-text";
    p.setAttribute("role", "alert");
    p.textContent = elt.getAttribute("data-load-error") + " ";
    var retry = document.createElement("a");
    retry.href = location.href;
    retry.textContent = elt.getAttribute("data-load-retry");
    p.appendChild(retry);
    elt.replaceChildren(p);
  }
  document.addEventListener("htmx:responseError", failed);
  document.addEventListener("htmx:sendError", failed);
  document.addEventListener("htmx:timeout", failed);
})();

// Sidebar: collapse to icons on wide screens (remembered in a cookie read by
// the server, no flash), slide-in panel on small screens. Without JavaScript
// the menu is simply shown above the page on small screens (.no-js).
(function () {
  "use strict";

  document.documentElement.classList.remove("no-js");

  function shell() { return document.querySelector(".shell"); }

  var small = window.matchMedia("(max-width: 800px)");

  // While the panel is open, the rest of the page is inert: Tab stays in the
  // menu and screen readers ignore what is behind. Closing gives the focus
  // back to the ☰ button.
  function setOpen(open) {
    var s = shell();
    if (!s) { return; }
    var wasOpen = s.classList.contains("is-open");
    s.classList.toggle("is-open", open);
    var backdrop = s.querySelector(".sidebar-backdrop");
    if (backdrop) { backdrop.hidden = !open; }
    var main = s.querySelector(".shell-main");
    if (main) { main.inert = open; }
    var button = document.querySelector("[data-sidebar-open]");
    if (button) { button.setAttribute("aria-expanded", open ? "true" : "false"); }
    if (open) {
      var first = s.querySelector(".sidebar a, .sidebar-close");
      if (first) { first.focus(); }
    } else if (wasOpen && button && small.matches) {
      button.focus();
    }
  }

  // Back to a wide screen: the panel is no longer an overlay.
  small.addEventListener("change", function (e) { if (!e.matches) { setOpen(false); } });

  document.addEventListener("click", function (e) {
    if (!e.target.closest) { return; }
    if (e.target.closest("[data-sidebar-open]")) { setOpen(true); return; }
    if (e.target.closest("[data-sidebar-close]")) { setOpen(false); return; }
    if (e.target.closest("[data-sidebar-collapse]")) {
      var s = shell();
      if (!s) { return; }
      var collapsed = !s.classList.contains("is-collapsed");
      s.classList.toggle("is-collapsed", collapsed);
      document.cookie = "hangarr_sidebar=" + (collapsed ? "collapsed" : "open") +
        "; Path=/; Max-Age=31536000; SameSite=Lax" + (location.protocol === "https:" ? "; Secure" : "");
    }
  });
  document.addEventListener("keydown", function (e) {
    var s = shell();
    if (e.key === "Escape" && s && s.classList.contains("is-open")) {
      setOpen(false);
    }
  });
})();

// Sync ticker (scrolling text of the Y2K style): the status is swapped every
// 2 s while a sync runs, which would restart the animation; a negative delay
// keeps it going from where it was.
(function () {
  "use strict";

  var start = 0;
  function keepScrolling() {
    var text = document.querySelector(".sync-ticker > span");
    if (!text) { start = 0; return; }
    if (!start) { start = Date.now(); }
    text.style.animationDelay = (-(Date.now() - start) / 1000) + "s";
  }
  document.addEventListener("htmx:afterSwap", keepScrolling);
  document.addEventListener("DOMContentLoaded", keepScrolling);
})();

// Steppers: the − / + buttons (data-step-by, aria-controls) change their
// number field within its min / max; at a bound the button is disabled.
(function () {
  "use strict";

  function sync(input) {
    document.querySelectorAll('[data-step-by][aria-controls="' + CSS.escape(input.id) + '"]').forEach(function (b) {
      var by = parseInt(b.getAttribute("data-step-by"), 10), v = Number(input.value);
      b.disabled = (by < 0 && input.min !== "" && v <= Number(input.min)) ||
        (by > 0 && input.max !== "" && v >= Number(input.max));
    });
  }
  function syncAll() {
    document.querySelectorAll("[data-step-by]").forEach(function (b) {
      var input = document.getElementById(b.getAttribute("aria-controls"));
      if (input) { sync(input); }
    });
  }

  document.addEventListener("click", function (e) {
    var b = e.target.closest && e.target.closest("[data-step-by]");
    if (!b) { return; }
    var input = document.getElementById(b.getAttribute("aria-controls"));
    if (!input) { return; }
    var min = input.min === "" ? -Infinity : Number(input.min);
    var max = input.max === "" ? Infinity : Number(input.max);
    var v = Number(input.value) || 0;
    input.value = String(Math.min(max, Math.max(min, v + parseInt(b.getAttribute("data-step-by"), 10))));
    input.dispatchEvent(new Event("input", { bubbles: true }));
    input.dispatchEvent(new Event("change", { bubbles: true }));
  });
  document.addEventListener("input", function (e) {
    if (e.target.id && document.querySelector('[data-step-by][aria-controls="' + CSS.escape(e.target.id) + '"]')) { sync(e.target); }
  });
  document.addEventListener("DOMContentLoaded", syncAll);
  document.addEventListener("htmx:afterSwap", syncAll);
})();

// Copy buttons (data-copy): the text goes to the clipboard and the button says
// so for 2 s. Without a secure context (plain HTTP) the clipboard API is missing:
// a hidden textarea and execCommand do the copy.
(function () {
  "use strict";

  function fallbackCopy(text) {
    var previous = document.activeElement;
    var buffer = document.createElement("textarea");
    buffer.className = "copy-buffer";
    buffer.readOnly = true;
    buffer.value = text;
    document.body.appendChild(buffer);
    try {
      buffer.select();
      return document.execCommand("copy");
    } catch (err) {
      return false;
    } finally {
      buffer.remove();
      // select() moved the focus to the buffer: give it back to the button.
      if (previous) { previous.focus(); }
    }
  }

  function copy(text) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text).then(function () { return true; }, function () { return false; });
    }
    return Promise.resolve(fallbackCopy(text));
  }

  var statusTimer;

  // The button's aria-label hides its text from screen readers: the same message
  // goes to the page's live region (#copy-status), if the page has one.
  function announce(message) {
    var status = document.getElementById("copy-status");
    if (!status) { return; }
    clearTimeout(statusTimer);
    status.textContent = message;
    statusTimer = setTimeout(function () { status.textContent = ""; }, 2000);
  }

  function feedback(btn, ok) {
    var message = btn.getAttribute(ok ? "data-copied" : "data-copy-failed");
    // The label is kept once, so a second click within 2 s does not keep the feedback.
    if (!btn.hasAttribute("data-label")) { btn.setAttribute("data-label", btn.textContent); }
    clearTimeout(btn.copyTimer);
    btn.textContent = message;
    announce(message);
    btn.copyTimer = setTimeout(function () {
      btn.textContent = btn.getAttribute("data-label");
    }, 2000);
  }

  document.addEventListener("click", function (e) {
    var btn = e.target.closest && e.target.closest("[data-copy]");
    if (!btn) { return; }
    copy(btn.getAttribute("data-copy")).then(function (ok) { feedback(btn, ok); });
  });
})();
