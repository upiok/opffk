/* opffk web UI — vanilla JS, no dependencies, no eval, no innerHTML with data. */
"use strict";

var CSRF = "";
var configLoaded = null;
var logsTimer = null;

// ---- tiny helpers ---------------------------------------------------------

function $(id) { return document.getElementById(id); }

function show(el) { el.classList.remove("hidden"); }
function hide(el) { el.classList.add("hidden"); }

function setText(id, text) { $(id).textContent = text; }

function api(path, opts) {
  opts = opts || {};
  opts.headers = opts.headers || {};
  if (CSRF) opts.headers["X-Opffk-CSRF"] = CSRF;
  if (opts.body && typeof opts.body !== "string") {
    opts.body = JSON.stringify(opts.body);
    opts.headers["Content-Type"] = "application/json";
  }
  return fetch(path, opts).then(function (r) {
    return r.json().then(function (data) {
      if (!r.ok) {
        var msg = (data && data.error) ? data.error : ("HTTP " + r.status);
        var err = new Error(msg);
        err.status = r.status;
        throw err;
      }
      return data;
    });
  });
}

function fmtBytes(n) {
  if (!n) return "0 B";
  var k = 1024, units = ["B", "KB", "MB", "GB"], i = 0;
  while (n >= k && i < units.length - 1) { n /= k; i++; }
  return (i === 0 ? n : n.toFixed(i > 1 ? 2 : 1)) + " " + units[i];
}

function fmtUptime(sec) {
  if (!sec) return "—";
  var d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600),
      m = Math.floor((sec % 3600) / 60), s = Math.floor(sec % 60);
  var out = "";
  if (d) out += d + "d ";
  if (d || h) out += h + "h ";
  if (d || h || m) out += m + "m ";
  out += s + "s";
  return out;
}

// ---- login ----------------------------------------------------------------

function bindLogin() {
  $("login-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    hide($("login-error"));
    $("login-submit").disabled = true;
    fetch("/login", {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: "password=" + encodeURIComponent($("login-password").value),
      credentials: "same-origin"
    }).then(function (r) { return r.json().then(function (d) { return { ok: r.ok, data: d }; }); })
      .then(function (res) {
        $("login-submit").disabled = false;
        if (!res.ok) {
          setText("login-error", res.data && res.data.error ? res.data.error : "login failed");
          show($("login-error"));
          return;
        }
        CSRF = res.data.csrf;
        enterApp();
      })
      .catch(function () {
        $("login-submit").disabled = false;
        setText("login-error", "login failed (network error)");
        show($("login-error"));
      });
  });
}

// ---- state rendering ---------------------------------------------------------

function badge(kind, text) {
  var b = $("hdr-badge");
  b.className = "badge " + kind;
  b.textContent = text;
}

function renderStatus(st) {
  var grid = $("status-grid");
  grid.textContent = "";
  var rows = [
    ["Core process", st.running ? "running (pid " + st.pid + ")" : (st.desired ? "stopped (auto-restart pending)" : "stopped")],
    ["Uptime", fmtUptime(st.uptime_sec)],
    ["Traffic in / out", fmtBytes(st.bytes_in) + " / " + fmtBytes(st.bytes_out)],
    ["Auto-restarts", String(st.restarts || 0)]
  ];
  rows.forEach(function (pair) {
    var dt = document.createElement("div"); dt.className = "k"; dt.textContent = pair[0];
    var dd = document.createElement("div"); dd.className = "v"; dd.textContent = pair[1];
    grid.appendChild(dt); grid.appendChild(dd);
  });
  var note = "";
  if (st.last_exit) note = "Last exit: " + st.last_exit;
  if (st.ipc_error && st.running) note += (note ? " — " : "") + st.ipc_error;
  setText("status-note", note);

  if (st.running && st.connected) badge("ok", "connected");
  else if (st.running) badge("warn", "running");
  else badge("bad", "stopped");

  $("btn-stop").disabled = !st.running;
  $("btn-restart").disabled = !st.running;
}

function updatePriorityVisibility() {
  // Priority only matters with two or more transports: hide the input
  // (and its noise) while there is exactly one.
  var rows = document.querySelectorAll("#transport-list .transport");
  var show = rows.length >= 2;
  rows.forEach(function (row) { row._fields.pri.style.display = show ? "" : "none"; });
}

function transportRow(t) {
  var div = document.createElement("div");
  div.className = "transport";

  var sel = document.createElement("select");
  ["direct", "yandex", "vyandex", "mailru", "cupsonline", "boards", "avito", "telemost", "oneme"].forEach(function (ty) {
    var o = document.createElement("option"); o.value = ty; o.textContent = ty;
    sel.appendChild(o);
  });
  sel.value = t.type;

  var pri = document.createElement("input");
  pri.type = "number"; pri.min = "1"; pri.max = "100"; pri.title = "priority (higher = tried first)";
  pri.value = t.priority;

  var val = document.createElement("input");
  val.type = "text";
  val.placeholder = t.type === "direct" ? "0.0.0.0:7891 (listen)" : "URL / token";
  val.value = t.url || t.listen || t.token ||
    (t.type === "direct" ? "0.0.0.0:7891" : ""); // sensible default for new rows

  var uid = document.createElement("input");
  uid.type = "text"; uid.placeholder = "oneme uid"; uid.className = "uid";
  uid.value = t.uid || "";

  var del = document.createElement("button");
  del.type = "button"; del.className = "ghost small-btn"; del.textContent = "✕";
  del.addEventListener("click", function () { div.remove(); updatePriorityVisibility(); });

  function syncFields() {
    var ty = sel.value;
    val.placeholder =
    ty === "direct" ? "0.0.0.0:7891 (listen)" :
    ty === "oneme" ? "MAX token" :
    ty === "cupsonline" ? "optional — rooms are created at runtime" :
    "https://…";
    val.style.display = (ty === "oneme") ? "" : "";
    uid.style.display = (ty === "oneme") ? "" : "none";
  }
  sel.addEventListener("change", syncFields);
  syncFields();

  div.appendChild(sel); div.appendChild(pri); div.appendChild(val); div.appendChild(uid); div.appendChild(del);
  div._fields = { sel: sel, pri: pri, val: val, uid: uid };
  return div;
}

function renderConfig(cfg) {
  $("cfg-mode").value = cfg.openflux.mode || "l4";
  if (cfg.openflux.mode === "l3") { /* keep visible; validation gates it */ }
  $("cfg-codec").value = cfg.openflux.codec || "batched";
  $("cfg-debug").value = String(cfg.openflux.debug == null ? 2 : cfg.openflux.debug);
  $("cfg-context").value = cfg.openflux.session_context || "";
  $("cfg-egress").value = cfg.openflux.egress_proxy || "";
  $("l3-note").style.display = ($("cfg-mode").value === "l3") ? "" : "none";
  var list = $("transport-list");
  list.textContent = "";
  (cfg.openflux.transports || []).forEach(function (t) {
    list.appendChild(transportRow(t));
  });
  updatePriorityVisibility();
  var sel = $("cfg-mode");
  var l3opt = sel.querySelector('option[value="l3"]');
  if (l3opt) l3opt.disabled = !cfg.allow_l3;
}

function readConfigFromForm() {
  var transports = [];
  document.querySelectorAll("#transport-list .transport").forEach(function (row) {
    var f = row._fields;
    var t = { type: f.sel.value, priority: parseInt(f.pri.value, 10) || 50 };
    var v = f.val.value.trim();
    if (t.type === "direct") t.listen = v;
    else if (t.type === "oneme") { t.token = v; t.uid = f.uid.value.trim(); }
    else t.url = v;
    transports.push(t);
  });
  return {
    mode: $("cfg-mode").value,
    codec: $("cfg-codec").value,
    debug: parseInt($("cfg-debug").value, 10) || 0,
    session_context: $("cfg-context").value.trim(),
    egress_proxy: $("cfg-egress").value.trim(),
    transports: transports
  };
}

function renderState(state) {
  CSRF = state.csrf;
  var transports = (state.config && state.config.openflux && state.config.openflux.transports) || [];
  setText("btn-add-server", transports.length ? "Server settings" : "+ Add server");
  setText("hdr-version", state.backend);
  setText("p-conf", state.paths.conf);
  setText("upd-installed", (state.opffk_version || "unknown") + "");
  setText("p-core", state.paths.core);
  setText("logout-csrf", state.csrf);
  setText("ftr-note", state.backend + " — platform " + state.platform.uname_m +
    (state.platform.test_mode ? " (TEST MODE, root " + state.platform.root + ")" : "") +
    " — upstream core: github.com/p1neappleXpress/OpenFlux");
  renderStatus(state.service);
  renderServerCard(state);
  if (!configLoaded) {
    renderConfig(state.config);
    configLoaded = true;
  }
  renderLogs(state.logs);
}

var carrierNames = {
  direct: "Direct", yandex: "Yandex Docs", vyandex: "Yandex Volga",
  mailru: "Mail.ru Docs", cupsonline: "cups.online", boards: "Boards",
  avito: "Avito", telemost: "Telemost", oneme: "MAX"
};
function carrierLabel(ty) { return carrierNames[ty] || ty; }

function renderServerCard(state) {
  var card = $("card-server");
  var transports = (state.config && state.config.openflux && state.config.openflux.transports) || [];
  if (!transports.length) { card.classList.add("hidden"); return; }
  card.classList.remove("hidden");

  var st = state.service;
  var dot = $("server-dot");
  dot.className = "server-dot";

  // title: the carrier traffic currently flows through; when the node is
  // idle, the highest-priority configured transport
  var title = "opffk exit";
  if (st.running && st.active) {
    title = carrierLabel(st.active);
  } else if (transports.length) {
    var best = transports[0];
    transports.forEach(function (t) { if (t.priority > best.priority) best = t; });
    title = carrierLabel(best.type);
  }
  setText("server-name", title);

  var sub = "—";
  if (st.running && st.connected) {
    dot.classList.add("dot-ok");
    sub = "connected";
  } else if (st.running) {
    dot.classList.add("dot-warn");
    sub = "connecting…";
  } else {
    dot.classList.add("dot-bad");
    sub = "stopped";
  }
  setText("server-sub", sub);

  // expandable transport list
  var list = $("server-transports");
  list.textContent = "";
  transports.forEach(function (t) {
    var row = document.createElement("div");
    row.className = "st-row";
    var name = document.createElement("span");
    name.className = "st-name";
    name.textContent = carrierLabel(t.type);
    var det = document.createElement("span");
    det.className = "st-det muted";
    if (t.type === "direct") det.textContent = t.listen || "";
    else if (t.type === "oneme") det.textContent = "priority " + t.priority;
    else det.textContent = t.url || "";
    var cupsBtn = null;
    if (t.type === "cupsonline") {
      cupsBtn = document.createElement("button");
      cupsBtn.type = "button";
      cupsBtn.className = "ghost small-btn";
      cupsBtn.textContent = "Create rooms";
      cupsBtn.title = "create a fresh cups.online room set (takes up to a minute)";
      cupsBtn.addEventListener("click", function () {
        cupsBtn.disabled = true;
        cupsBtn.textContent = "creating…";
        post("/api/cups/rooms").then(function () {
          configLoaded = false;
          refreshState();
        }).catch(function (err) {
          cupsBtn.disabled = false;
          cupsBtn.textContent = "Create rooms";
          window.alert("create rooms failed: " + err.message);
        });
      });
    }

    var del = document.createElement("button");
    del.type = "button";
    del.className = "ghost small-btn";
    del.textContent = "✕";
    del.title = "remove this transport and restart";
    del.addEventListener("click", function () {
      armButton(del, function () {
        var cfg = JSON.parse(JSON.stringify(state.config));
        cfg.openflux.transports = (cfg.openflux.transports || []).filter(function (x) { return x.type !== t.type; });
        post("/api/config", { settings: cfg.openflux, restart: true }).then(function (res) {
          if (res.errors && res.errors.length) { window.alert(res.errors.join("\n")); return; }
          configLoaded = false; // the card re-reads config from state
          refreshState();
        });
      });
    });
    row.appendChild(name); row.appendChild(det);
    if (cupsBtn) row.appendChild(cupsBtn);
    row.appendChild(del);
    list.appendChild(row);
  });
  if (transports.length) {
    var hint = document.createElement("div");
    hint.className = "muted small";
    hint.textContent = "opffk runs one node; transports above are its carriers (higher priority is tried first). Add more via “Server settings”.";
    list.appendChild(hint);

    var delNode = document.createElement("button");
    delNode.type = "button";
    delNode.className = "danger-btn";
    delNode.textContent = "Delete node";
    delNode.addEventListener("click", function () {
      armButton(delNode, function () {
        var cfg = JSON.parse(JSON.stringify(state.config));
        cfg.openflux.transports = [];
        post("/api/config", { settings: cfg.openflux, restart: false }).then(function (res) {
          if (res.errors && res.errors.length) { window.alert(res.errors.join("\n")); return; }
          configLoaded = false;
          refreshState(); // card disappears, header returns to "+ Add server"
        });
      });
    });
    list.appendChild(delNode);
  }
}

function renderLogs(lines) {
  var view = $("logs-view");
  view.textContent = lines && lines.length ? lines.join("\n") : "(no log lines yet)";
  view.scrollTop = view.scrollHeight;
}

// ---- app lifecycle ---------------------------------------------------------

function enterApp() {
  hide($("view-login"));
  show($("view-app"));
  refreshState();
}

// two-step arming: the first click arms the button (it changes label and
// goes red), the second within 5 s executes. No window.confirm — Firefox
// silently swallows dialogs once "block additional dialogs" was checked.
function armButton(btn, exec) {
  if (btn.dataset.armed === "1") {
    btn.dataset.armed = "";
    btn.classList.remove("armed");
    exec();
    return;
  }
  btn.dataset.armed = "1";
  btn.classList.add("armed");
  var prev = btn.textContent;
  btn.textContent = "Really delete?";
  setTimeout(function () {
    if (btn.dataset.armed === "1") {
      btn.dataset.armed = "";
      btn.classList.remove("armed");
      btn.textContent = prev;
    }
  }, 5000);
}

var statusTimer = null;

function startStatusPolling() {
  if (statusTimer) clearInterval(statusTimer);
  statusTimer = setInterval(function () {
    if ($("view-app").classList.contains("hidden")) return;
    if (!$("status-auto") || !$("status-auto").checked) return;
    refreshState();
  }, 5000);
}

function refreshState() {
  return api("/api/state").then(function (state) {
    show($("view-app"));
    hide($("view-login"));
    renderState(state);
  }).catch(function (err) {
    if (err.status === 401) {
      show($("view-login"));
      hide($("view-app"));
    }
  });
}

// ---- event wiring ---------------------------------------------------------

function post(path, body) { return api(path, { method: "POST", body: body || {} }); }

// ---- floating windows -------------------------------------------------------

function openModal(id) {
  hide($("settings-menu"));
  var m = $(id);
  if (!m) return;
  m.classList.remove("hidden");
}
function closeModal(id) {
  var m = $(id);
  if (m) m.classList.add("hidden");
}
function bindModalChrome() {
  document.querySelectorAll(".modal-close").forEach(function (b) {
    b.addEventListener("click", function () { closeModal(b.getAttribute("data-close")); });
  });
  // click on the backdrop (the wrap itself) closes
  document.querySelectorAll(".modal-wrap").forEach(function (w) {
    w.addEventListener("mousedown", function (ev) {
      if (ev.target === w) w.classList.add("hidden");
    });
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key !== "Escape") return;
    document.querySelectorAll(".modal-wrap").forEach(function (w) { w.classList.add("hidden"); });
    hide($("settings-menu"));
  });
  $("btn-add-server").addEventListener("click", function () {
    hide($("config-errors"));
    openModal("modal-server");
  });
  $("btn-settings").addEventListener("click", function (ev) {
    ev.stopPropagation();
    $("settings-menu").classList.toggle("hidden");
  });
  // click anywhere else closes the dropdown
  document.addEventListener("click", function (ev) {
    if (!ev.target.closest || !ev.target.closest(".menu-wrap")) hide($("settings-menu"));
  });
  $("dd-update").addEventListener("click", function () {
    hide($("settings-menu"));
    openModal("modal-update");
  });
  $("dd-password").addEventListener("click", function () {
    hide($("settings-menu"));
    openModal("modal-password");
  });
  $("dd-uninstall").addEventListener("click", function () {
    hide($("settings-menu"));
    openModal("modal-uninstall");
  });
}

function bindApp() {
  $("cfg-mode").addEventListener("change", function () {
    $("l3-note").style.display = ($("cfg-mode").value === "l3") ? "" : "none";
  });
  $("cfg-egress").addEventListener("input", function () {
    // visual hint only; real validation happens server-side on Apply
  });
  $("server-head").addEventListener("click", function () {
    var list = $("server-transports");
    var open = !list.classList.contains("hidden");
    list.classList.toggle("hidden");
    $("server-chevron").style.transform = open ? "" : "rotate(180deg)";
  });

  $("btn-start").addEventListener("click", function () {
    post("/api/service/start").then(refreshState);
  });
  $("btn-stop").addEventListener("click", function () {
    post("/api/service/stop").then(refreshState);
  });
  $("btn-restart").addEventListener("click", function () {
    post("/api/service/restart").then(refreshState);
  });

  $("btn-config-save").addEventListener("click", function () { applyConfig(false); });
  $("btn-config-save-restart").addEventListener("click", function () { applyConfig(true); });

  $("btn-t-add").addEventListener("click", function () {
    $("transport-list").appendChild(transportRow({ type: "direct", priority: 50 }));
    updatePriorityVisibility();
  });

  $("btn-link").addEventListener("click", function () {
    hide($("link-error"));
    var form = new URLSearchParams();
    form.set("share_host", $("link-host").value.trim());
    post("/api/link?" + form.toString()).then(function (res) {
      setText("link-text", res.link || "");
      show($("link-result"));
    }).catch(function (err) {
      setText("link-error", err.message); show($("link-error"));
    });
  });
  $("btn-link-copy").addEventListener("click", function () {
    $("link-text").select();
    try { document.execCommand("copy"); } catch (e) { /* clipboard API only in secure contexts */ }
  });
  $("btn-key-rotate").addEventListener("click", function () {
    armButton($("btn-key-rotate"), function () {
      post("/api/key/rotate").then(function (res) {
        setText("update-result", res.message || "key rotated");
      });
    });
  });

  $("btn-self-check").addEventListener("click", function () {
    setText("self-result", "checking…");
    post("/api/update", { action: "check_self" }).then(function (res) {
      setText("upd-installed", res.installed || "unknown");
      setText("self-result", (res.message || JSON.stringify(res)));
    }).catch(function (err) { setText("self-result", "error: " + err.message); });
  });
  $("btn-self-apply").addEventListener("click", function () {
    if (!window.confirm("Update the whole opffk (backend + core + init script)? The backend restarts itself and you will need to log in again.")) return;
    setText("self-result", "updating… (download, verify, swap, restart)");
    post("/api/update", { action: "apply_full" }).then(function (res) {
      setText("self-result", res.message || "updated");
      // the backend re-execs: probe until it answers again, then reload
      var tries = 0;
      var t = setInterval(function () {
        tries++;
        fetch("/login").then(function (r) {
          if (r.ok && tries > 2) { clearInterval(t); window.location.reload(); }
          if (tries > 30) clearInterval(t);
        }).catch(function () { /* still restarting */ });
      }, 1500);
    }).catch(function (err) { setText("self-result", "error: " + err.message); });
  });

  $("btn-update-check").addEventListener("click", function () {
    setText("update-result", "checking…");
    post("/api/update", { action: "check" }).then(function (res) {
      setText("update-result", res.message || JSON.stringify(res));
    }).catch(function (err) { setText("update-result", "error: " + err.message); });
  });
  $("btn-update-apply").addEventListener("click", function () {
    if (!window.confirm("Download and activate the candidate core update? Automatic rollback if it fails health checks.")) return;
    setText("update-result", "updating… (download, verify, restart, health check)");
    post("/api/update", { action: "apply" }).then(function (res) {
      setText("update-result", res.message || JSON.stringify(res));
      refreshState();
    }).catch(function (err) { setText("update-result", "error: " + err.message); });
  });
  $("btn-update-rollback").addEventListener("click", function () {
    if (!window.confirm("Roll back to the previous core binary?")) return;
    post("/api/update", { action: "rollback" }).then(function (res) {
      setText("update-result", res.message || "rolled back");
      refreshState();
    }).catch(function (err) { setText("update-result", "error: " + err.message); });
  });

  $("btn-logs-refresh").addEventListener("click", refreshLogs);
  $("logs-auto").addEventListener("change", function () {
    if (logsTimer) { clearInterval(logsTimer); logsTimer = null; }
    if ($("logs-auto").checked) logsTimer = setInterval(refreshLogs, 5000);
  });

  $("btn-password").addEventListener("click", function () {
    setText("password-result", "");
    post("/api/password", { current: $("pw-current").value, "new": $("pw-new").value })
      .then(function (res) {
        setText("password-result", res.message || "changed");
        closeModal("modal-password");
        setTimeout(function () { window.location.reload(); }, 1500);
      })
      .catch(function (err) { setText("password-result", "error: " + err.message); });
  });

  $("btn-uninstall").addEventListener("click", function () {
    var phrase = $("uninstall-confirm").value;
    if (phrase !== "UNINSTALL") {
      setText("uninstall-result", "Type UNINSTALL exactly to confirm.");
      return;
    }
    if (!window.confirm("Really uninstall opffk and stop the exit node?")) return;
    post("/api/uninstall", { confirm: phrase }).then(function (res) {
      setText("uninstall-result", res.message || "done");
    }).catch(function (err) { setText("uninstall-result", "error: " + err.message); });
  });

  $("logout-form").addEventListener("submit", function () {
    // The logout form posts csrf_token as a field — same token.
  });
}

function applyConfig(restart) {
  hide($("config-errors"));
  var payload = { settings: readConfigFromForm(), restart: restart };
  post("/api/config", payload).then(function (res) {
    if (res.errors && res.errors.length) {
      var box = $("config-errors");
      box.textContent = res.errors.join("\n");
      show(box);
      return;
    }
    if (!res.ok && res.restart_error) {
      var box2 = $("config-errors");
      box2.textContent = "saved, but restart failed: " + res.restart_error;
      show(box2);
      return;
    }
    // success: close the server window and refresh
    closeModal("modal-server");
    refreshState();
  }).catch(function (err) {
    var box = $("config-errors");
    box.textContent = err.message;
    show(box);
  });
}

function refreshLogs() {
  api("/api/logs?n=200").then(function (res) { renderLogs(res.logs); })
    .catch(function () { /* session expired; state refresh will handle it */ });
}

// ---- boot ------------------------------------------------------------------

document.addEventListener("DOMContentLoaded", function () {
  bindLogin();
  bindApp();
  bindModalChrome();
  refreshState();
  startStatusPolling();
});
