// arrstats: minimal client script (CSP script-src 'self', no eval).
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

  document.addEventListener("arrstats:toast", function (e) { toast(e.detail.message, e.detail.level); });

  // Protection changed: reload the detail (modal or page) to show the new state.
  document.addEventListener("arrstats:changed", function (e) {
    var modal = document.getElementById("modal");
    if (modal && modal.open) { htmx.ajax("GET", e.detail.detailUrl, { target: "#modal-body" }); } else { location.reload(); }
  });

  document.addEventListener("arrstats:deleted", function (e) {
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

