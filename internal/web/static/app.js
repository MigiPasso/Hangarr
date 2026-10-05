// cleanarr: minimal client script (CSP script-src 'self', no eval).
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

  document.addEventListener("cleanarr:toast", function (e) { toast(e.detail.message, e.detail.level); });

  // Protection changed: reload the detail (modal or page) to show the new state.
  document.addEventListener("cleanarr:changed", function (e) {
    var modal = document.getElementById("modal");
    if (modal && modal.open) { htmx.ajax("GET", e.detail.detailUrl, { target: "#modal-body" }); } else { location.reload(); }
  });

  document.addEventListener("cleanarr:deleted", function (e) {
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
  document.addEventListener("cleanarr:batch-done", function (e) {
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
    document.cookie = "cleanarr_theme=" + next + "; Path=/; Max-Age=31536000; SameSite=Lax" +
      (location.protocol === "https:" ? "; Secure" : "");
  });
})();
