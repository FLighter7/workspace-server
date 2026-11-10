"use strict";

// The page's script. It builds every piece of the DOM with createElement and
// text nodes: app names, device names and refused ids come from elsewhere and
// are never parsed as HTML.
(() => {
  const S = JSON.parse(document.getElementById("strings").textContent);
  const lang = document.documentElement.lang || "en";
  const ID_PATTERN = /^[a-z0-9-]{1,64}$/;
  const POLL_MS = 2000;

  const $ = (id) => document.getElementById(id);

  // ---- Building the DOM ----------------------------------------------------

  // h creates an element, assigns props (className, textContent, type, ...)
  // and appends children; a string child becomes a text node.
  function h(tag, props, ...children) {
    const node = document.createElement(tag);
    if (props) Object.assign(node, props);
    for (const child of children) {
      if (child != null) node.append(child);
    }
    return node;
  }

  // iso shows untrusted text isolated, so its direction cannot reorder the
  // words around it.
  const iso = (text) => h("bdi", null, String(text));

  // fill returns the string with each {key} replaced by values[key]: a node as
  // is, anything else as isolated text.
  function fill(template, values) {
    const out = document.createDocumentFragment();
    template.split(/\{(\w+)\}/).forEach((part, i) => {
      if (i % 2 === 0) {
        if (part) out.append(part);
      } else {
        const v = values[part];
        out.append(v instanceof Node ? v : iso(v));
      }
    });
    return out;
  }

  // fact is one row of a <dl class="facts">.
  const fact = (label, ...value) => h("div", null, h("dt", null, label), h("dd", null, ...value));

  // say shows a message (text or nodes) in a status line; "" clears it.
  function say(node, content, isError) {
    if (content === "") node.replaceChildren();
    else node.replaceChildren(content);
    node.classList.toggle("error", Boolean(isError));
  }

  // keepFocus re-renders a container and gives focus back to the element
  // with the same data-key, so polling does not take it away.
  function keepFocus(container, render) {
    const active = document.activeElement;
    const key = container.contains(active) && active.dataset ? active.dataset.key : undefined;
    render();
    if (!key) return;
    for (const node of container.querySelectorAll("[data-key]")) {
      if (node.dataset.key === key) {
        node.focus();
        return;
      }
    }
  }

  // ---- Formatting ----------------------------------------------------------

  const numberFormat = new Intl.NumberFormat(lang);
  const timeFormat = new Intl.DateTimeFormat(lang, { dateStyle: "medium", timeStyle: "short" });
  const formatTime = (s) => timeFormat.format(new Date(s));

  function formatBytes(n) {
    const units = ["byte", "kilobyte", "megabyte", "gigabyte"];
    let i = 0;
    while (n >= 1000 && i < units.length - 1) {
      n /= 1000;
      i++;
    }
    return new Intl.NumberFormat(lang, {
      style: "unit", unit: units[i], unitDisplay: i === 0 ? "long" : "short", maximumFractionDigits: i === 0 ? 0 : 1,
    }).format(n);
  }

  // ---- The API -------------------------------------------------------------

  class ApiError extends Error {
    constructor(code, message) {
      super(message);
      this.code = code;
    }
  }

  async function api(method, path, body) {
    const init = { method, headers: { "X-Workspace-Server": "1" }, cache: "no-store" };
    if (body !== undefined) {
      init.headers["Content-Type"] = "application/json";
      init.body = JSON.stringify(body);
    }
    let res;
    try {
      res = await fetch(path, init);
    } catch {
      throw new ApiError("network", "");
    }
    if (res.status === 201 || res.status === 204) return null;
    let data = null;
    try {
      data = await res.json();
    } catch {
      // not JSON
    }
    if (!res.ok) {
      throw new ApiError((data && data.error) || "internal", (data && data.message) || res.statusText);
    }
    return data;
  }

  const errorTexts = {
    invalid: S.errInvalid,
    exists: S.errExists,
    confirmMismatch: S.errConfirmMismatch,
    notFound: S.errNotFound,
    forbidden: S.errForbidden,
    network: S.errNetwork,
    dataExists: S.errDataExists,
    changed: S.errDataChanged,
    busy: S.errBusy,
  };

  // errorText is the page's text for an error, followed by the server's
  // detail, which says which value was refused.
  function errorText(e) {
    const text = (e instanceof ApiError && errorTexts[e.code]) || S.errInternal;
    return e.message ? `${text} (${e.message})` : text;
  }

  // ---- The confirmation dialog ---------------------------------------------

  // ask shows a modal question and resolves to true when the user confirms.
  function ask(title, text, okLabel) {
    const dialog = $("confirm");
    $("confirm-title").replaceChildren(title);
    $("confirm-text").replaceChildren(text);
    $("confirm-ok").textContent = okLabel;
    return new Promise((resolve) => {
      $("confirm-ok").onclick = () => {
        resolve(true);
        dialog.close();
      };
      $("confirm-cancel").onclick = () => dialog.close();
      dialog.onclose = () => resolve(false);
      dialog.showModal();
      $("confirm-cancel").focus();
    });
  }

  // ---- Sections and polling ------------------------------------------------

  let pollRun = 0;
  let pollTimer = 0;

  // poll calls fn now and every POLL_MS until another section is shown. It
  // skips the calls while the page is hidden.
  function poll(fn) {
    const run = ++pollRun;
    const tick = async () => {
      if (run !== pollRun) return;
      if (document.visibilityState === "visible") await fn();
      if (run === pollRun) pollTimer = setTimeout(tick, POLL_MS);
    };
    tick();
  }

  function stopPolling() {
    pollRun++;
    clearTimeout(pollTimer);
  }

  const sections = {
    server: () => loadStatus(),
    apps: () => loadApps(),
    devices: () => openDevices(),
    pair: () => openPair(),
  };

  function show(name) {
    if (!Object.hasOwn(sections, name)) name = "server";
    stopPolling();
    for (const key of Object.keys(sections)) {
      $(key).hidden = key !== name;
      const link = document.querySelector(`nav a[href="#${key}"]`);
      if (key === name) link.setAttribute("aria-current", "page");
      else link.removeAttribute("aria-current");
    }
    sections[name]();
  }

  window.addEventListener("hashchange", () => show(location.hash.slice(1)));

  // ---- Server --------------------------------------------------------------

  let status = null;

  async function loadStatus() {
    try {
      status = await api("GET", "/api/status");
    } catch (e) {
      say($("server-msg"), errorText(e), true);
      return;
    }
    const addresses = status.addresses.length
      ? h("ul", { className: "plain" }, ...status.addresses.map((a) => h("li", null, h("code", null, a))))
      : S.noAddresses;
    $("server-facts").replaceChildren(
      fact(S.serverId, h("code", null, status.serverId)),
      status.listening
        ? fact(S.addresses, addresses)
        : fact(S.addresses, addresses, h("br"), h("span", { className: "state off", textContent: S.notListening })),
      fact(S.mdnsName, h("code", null, status.mdnsName), h("br"),
        h("span", { className: status.mdnsRunning ? "state on" : "state off",
          textContent: status.mdnsRunning ? S.mdnsUp : S.mdnsDown })),
    );
    $("server-name").value = status.name;
    $("udp-port").value = status.udpPort;
    $("http-port").value = status.httpPort;
    if (document.activeElement !== $("data-dir")) $("data-dir").value = status.dataDir;
    if (status.lastError) say($("server-msg"), fill(S.restartError, { error: status.lastError }), true);
  }

  $("server-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    if (!status) return;
    const msg = $("server-msg");
    const change = {};
    const name = $("server-name").value.trim();
    const udp = $("udp-port").valueAsNumber;
    const http = $("http-port").valueAsNumber;
    if (name !== status.name) change.name = name;
    if (udp !== status.udpPort) change.udpPort = udp;
    if (http !== status.httpPort) change.httpPort = http;
    if (Object.keys(change).length === 0) {
      say(msg, S.saved);
      return;
    }
    if ("udpPort" in change && !(await ask(S.confirmUdpTitle, S.confirmUdpText, S.confirmUdpButton))) {
      return;
    }
    try {
      await api("PUT", "/api/config", change);
    } catch (e) {
      say(msg, errorText(e), true);
      return;
    }
    // A code on screen carries the old port and name; the next visit to
    // Pair issues a new one.
    if ("udpPort" in change || "name" in change) dropCode();
    if ("httpPort" in change) {
      // The server now answers on the new port only; the browser follows.
      const url = `http://${location.hostname}:${change.httpPort}/#server`;
      say(msg, fill(S.movedTo, { url: h("a", { href: url, textContent: url }) }));
      setTimeout(() => location.assign(url), 1500);
      return;
    }
    say(msg, S.saved);
    loadStatus();
  });

  // ---- Restarts and stops --------------------------------------------------

  const RESTART_POLL_MS = 500;
  const RESTART_WAIT_MS = 30000;

  // waitRestart polls the status until done accepts it, then reloads the
  // page; after RESTART_WAIT_MS it says the server did not come back.
  async function waitRestart(msg, done) {
    say(msg, S.restarting);
    const until = Date.now() + RESTART_WAIT_MS;
    while (Date.now() < until) {
      await new Promise((resolve) => setTimeout(resolve, RESTART_POLL_MS));
      try {
        if (done(await api("GET", "/api/status"))) {
          location.reload();
          return;
        }
      } catch {
        // stopped for the restart; try again
      }
    }
    say(msg, S.restartFailed, true);
  }

  // showStopped replaces the page with the stopped panel.
  function showStopped(text) {
    stopPolling();
    for (const key of Object.keys(sections)) $(key).hidden = true;
    document.querySelector("nav").hidden = true;
    $("stopped-text").textContent = text;
    $("stopped").hidden = false;
    $("stopped-h").focus();
  }

  // ---- The wipe dialog -----------------------------------------------------

  // appFacts lists apps' data as facts: the size and versions, and a download
  // link when withDownload and the app has a document.
  function appFacts(list, withDownload) {
    return list.map((a) => fact(iso(a.name || a.id),
      a.size || a.modified ? formatBytes(a.size) : S.noDocument,
      " · ", `${S.versions}: ${numberFormat.format(a.versions)}`,
      withDownload && a.modified
        ? h("span", null, h("br"), h("a", { href: `/api/apps/${encodeURIComponent(a.id)}/document`, download: "", textContent: S.download }))
        : null));
  }

  // confirmWipe shows the wipe dialog with facts and runs action on OK. The
  // dialog stays open while action runs and shows its error; it closes when
  // action succeeds.
  function confirmWipe({ title, text, facts, okLabel, danger, action }) {
    const dialog = $("wipe");
    const ok = $("wipe-ok");
    $("wipe-title").textContent = title;
    $("wipe-text").textContent = text;
    $("wipe-facts").replaceChildren(...facts);
    ok.textContent = okLabel;
    ok.className = danger ? "danger" : "primary";
    ok.disabled = false;
    say($("wipe-msg"), "");
    $("wipe-cancel").onclick = () => dialog.close();
    ok.onclick = async () => {
      ok.disabled = true;
      try {
        await action();
      } catch (e) {
        say($("wipe-msg"), errorText(e), true);
        ok.disabled = false;
        return;
      }
      dialog.close();
    };
    dialog.showModal();
    $("wipe-cancel").focus();
  }

  // ---- The data folder -----------------------------------------------------

  $("data-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    if (!status) return;
    const msg = $("data-msg");
    const path = $("data-dir").value.trim();
    let check;
    try {
      check = await api("GET", `/api/data-dir?path=${encodeURIComponent(path)}`);
    } catch (e) {
      say(msg, errorText(e), true);
      return;
    }
    const target = check.path;
    const apply = async () => {
      const done = await api("PUT", "/api/config", { dataDir: target, verdict: check.verdict });
      if (done === null) {
        say(msg, S.saved); // the current folder
        return;
      }
      waitRestart(msg, (st) => st.dataDir === target || st.lastError);
    };
    const run = async () => {
      try {
        await apply();
      } catch (e) {
        say(msg, e.code === "invalid" ? `${S.errDataInvalid} (${e.message})` : errorText(e), true);
      }
    };
    const folder = fact(S.dataDirFact, h("code", null, target));
    switch (check.verdict) {
      case "same":
        say(msg, S.saved);
        return;
      case "conflict":
        say(msg, S.errDataExists, true);
        return;
      case "unavailable":
        say(msg, check.message ? `${S.errDataInvalid} (${check.message})` : S.errDataInvalid, true);
        return;
      case "adopt":
        confirmWipe({ title: S.adoptTitle, text: S.adoptText, facts: [folder, ...appFacts(check.target, false)],
          okLabel: S.adoptOk, danger: false, action: apply });
        return;
      case "move":
      case "create":
        if (check.current.length > 0) {
          confirmWipe({ title: S.moveTitle, text: S.moveText, facts: [folder, ...appFacts(check.current, false)],
            okLabel: S.moveOk, danger: false, action: apply });
          return;
        }
        break;
    }
    run(); // switch, or create with nothing to move: no question
  });

  // ---- Managing the server -------------------------------------------------

  $("btn-stop").addEventListener("click", async () => {
    if (!(await ask(S.stopTitle, S.stopText, S.stopOk))) return;
    try {
      await api("POST", "/api/server/stop", { removeAutostart: true });
    } catch (e) {
      say($("manage-msg"), errorText(e), true);
      return;
    }
    showStopped(S.stoppedText);
  });

  // wipeFacts gathers what a reset or a deletion removes.
  async function wipeFacts(withFolders) {
    const [st, list, devices] = await Promise.all([
      api("GET", "/api/status"), api("GET", "/api/apps"), api("GET", "/api/devices")]);
    const facts = [...appFacts(list, true), fact(S.devicesFact, numberFormat.format(devices.length))];
    if (withFolders) {
      facts.push(fact(S.serverDirFact, h("code", null, st.serverDir)));
      if (st.dataDir !== st.serverDir) facts.push(fact(S.dataDirFact, h("code", null, st.dataDir)));
      if (st.mainServer && st.program) facts.push(fact(S.programFact, h("code", null, st.program)));
    }
    return { st, facts };
  }

  $("btn-reset").addEventListener("click", async () => {
    let info;
    try {
      info = await wipeFacts(false);
    } catch (e) {
      say($("manage-msg"), errorText(e), true);
      return;
    }
    confirmWipe({ title: S.resetTitle, text: S.resetText, facts: info.facts, okLabel: S.resetOk, danger: true,
      action: async () => {
        await api("DELETE", "/api/data");
        dropCode();
        waitRestart($("manage-msg"), (st) => st.serverId !== info.st.serverId || st.lastError);
      } });
  });

  $("btn-delete-server").addEventListener("click", async () => {
    let info;
    try {
      info = await wipeFacts(true);
    } catch (e) {
      say($("manage-msg"), errorText(e), true);
      return;
    }
    confirmWipe({ title: S.deleteServerTitle, text: S.deleteServerText, facts: info.facts,
      okLabel: S.deleteServerOk, danger: true,
      action: async () => {
        await api("DELETE", "/api/server");
        showStopped(S.stoppedDeletedText);
      } });
  });

  // ---- Apps ----------------------------------------------------------------

  let appList = [];

  async function loadApps() {
    try {
      appList = await api("GET", "/api/apps");
    } catch (e) {
      say($("apps-msg"), errorText(e), true);
      return;
    }
    $("apps-empty").hidden = appList.length > 0;
    $("apps-list").replaceChildren(...appList.map(appCard));
    checkNewID();
  }

  function appCard(app) {
    return h("li", { className: "card" },
      h("div", { className: "card-head" },
        h("h2", { className: "card-title" }, iso(app.name), " ", h("code", { className: "muted" }, app.id)),
        h("button", { type: "button", className: "danger", textContent: S.delete, onclick: () => openDelete(app) })),
      h("dl", { className: "facts" },
        fact(S.size, app.modified ? formatBytes(app.size) : S.noDocument),
        fact(S.modified, app.modified ? formatTime(app.modified) : S.noDocument),
        fact(S.versions, numberFormat.format(app.versions)),
        fact(S.tabDevices, numberFormat.format(app.devices))));
  }

  // checkNewID checks the id being typed against the pattern and the ids in
  // use; the server checks again.
  function checkNewID() {
    const input = $("add-id");
    const id = input.value;
    let problem = "";
    if (id && !ID_PATTERN.test(id)) problem = S.idInvalid;
    else if (appList.some((a) => a.id === id)) problem = S.errExists;
    input.setCustomValidity(problem);
  }

  $("add-id").addEventListener("input", checkNewID);

  $("add-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const form = event.currentTarget;
    const app = { id: $("add-id").value };
    const name = $("add-name").value.trim();
    if (name) app.name = name;
    app.history = $("add-history").valueAsNumber;
    app.maxBytes = $("add-max").valueAsNumber;
    try {
      await api("POST", "/api/apps", app);
    } catch (e) {
      say($("add-msg"), errorText(e), true);
      return;
    }
    form.reset();
    say($("add-msg"), S.added);
    loadApps();
  });

  // ---- Deleting an app -----------------------------------------------------

  async function openDelete(app) {
    let devices;
    try {
      devices = (await api("GET", "/api/devices")).filter((d) => d.app === app.id);
    } catch (e) {
      say($("apps-msg"), errorText(e), true);
      return;
    }
    const dialog = $("delete");
    const input = $("delete-confirm");
    const ok = $("delete-ok");

    $("delete-title").replaceChildren(fill(S.deleteTitle, { name: app.name }));
    const names = devices.length
      ? h("ul", { className: "plain" }, ...devices.map((d) => h("li", null, iso(d.name))))
      : S.none;
    $("delete-facts").replaceChildren(
      fact(S.size, app.modified ? formatBytes(app.size) : S.noDocument),
      fact(S.versions, numberFormat.format(app.versions)),
      fact(S.deleteDevices, names));
    $("delete-download").replaceChildren(app.modified
      ? h("a", { href: `/api/apps/${encodeURIComponent(app.id)}/document`, download: "", textContent: S.download })
      : S.noDocument);
    $("delete-label").replaceChildren(fill(S.typeId, { id: h("code", null, app.id) }));
    say($("delete-msg"), "");
    input.value = "";
    ok.disabled = true;

    input.oninput = () => {
      ok.disabled = input.value !== app.id;
    };
    $("delete-cancel").onclick = () => dialog.close();
    $("delete-form").onsubmit = async (event) => {
      event.preventDefault();
      if (input.value !== app.id) return;
      ok.disabled = true;
      try {
        await api("DELETE", `/api/apps/${encodeURIComponent(app.id)}`, { confirm: input.value });
      } catch (e) {
        say($("delete-msg"), errorText(e), true);
        ok.disabled = false;
        return;
      }
      dialog.close();
      say($("apps-msg"), S.deleted);
      loadApps();
    };
    dialog.showModal();
    input.focus();
  }

  // ---- Devices -------------------------------------------------------------

  let shownDevices = "";

  function openDevices() {
    shownDevices = "";
    poll(loadDevices);
  }

  async function loadDevices() {
    const msg = $("devices-msg");
    if (!shownDevices) {
      try {
        appList = await api("GET", "/api/apps"); // for the groups' names
      } catch {
        // the groups then show ids instead of names
      }
    }
    let list;
    try {
      list = await api("GET", "/api/devices");
    } catch (e) {
      say(msg, errorText(e), true);
      return;
    }
    if (msg.classList.contains("error")) say(msg, "");
    const key = JSON.stringify(list);
    if (key === shownDevices) return;
    shownDevices = key;

    const groups = new Map();
    for (const d of list) {
      if (!groups.has(d.app)) groups.set(d.app, []);
      groups.get(d.app).push(d);
    }
    const container = $("devices-list");
    keepFocus(container, () => {
      $("devices-empty").hidden = list.length > 0;
      container.replaceChildren(...[...groups].map(([id, devices]) => {
        const app = appList.find((a) => a.id === id);
        return h("section", { className: "group" },
          h("h2", null, iso(app ? app.name : id), " ", h("code", { className: "muted" }, id)),
          h("ul", { className: "cards" }, ...devices.map(deviceCard)));
      }));
    });
  }

  function deviceCard(d) {
    const remove = h("button", { type: "button", textContent: S.remove, onclick: () => removeDevice(d) });
    remove.dataset.key = "remove:" + d.id;
    return h("li", { className: "card" },
      h("div", { className: "card-head" },
        h("h3", { className: "card-title" }, iso(d.name)),
        h("span", { className: d.connected ? "state on" : "state off",
          textContent: d.connected ? S.connectedNow : S.notConnected })),
      h("dl", { className: "facts" },
        fact(S.pairedAt, formatTime(d.pairedAt)),
        fact(S.lastSeen, formatTime(d.lastSeen))),
      h("div", { className: "actions start" }, remove));
  }

  async function removeDevice(d) {
    if (!(await ask(fill(S.removeTitle, { name: d.name }), S.removeText, S.remove))) return;
    const msg = $("devices-msg");
    try {
      await api("DELETE", `/api/devices/${encodeURIComponent(d.id)}`);
    } catch (e) {
      say(msg, errorText(e), true);
      return;
    }
    say(msg, S.removed);
    shownDevices = "";
    loadDevices();
  }

  // ---- Pairing -------------------------------------------------------------

  let expiresAt = 0;
  let countdown = 0;
  let issuing = false;

  // openPair shows a pairing code on entering the section: it issues one
  // unless a code on screen is still valid, so coming back to the section
  // does not revoke it. An expired code is replaced only on entering again or
  // by the New code button, never by itself.
  function openPair() {
    if (expiresAt <= Date.now()) issueCode();
    poll(loadRefusals);
  }

  // issueCode asks for a new secret, which revokes the previous one, and
  // shows its code.
  async function issueCode() {
    if (issuing) return;
    issuing = true;
    const msg = $("pair-msg");
    let pairing;
    try {
      pairing = await api("POST", "/api/pairing");
    } catch (e) {
      say(msg, errorText(e), true);
      return;
    } finally {
      issuing = false;
    }
    say(msg, "");
    $("pair-qr").src = pairing.qr;
    $("pair-text").value = pairing.code;
    expiresAt = Date.parse(pairing.expiresAt);
    $("pair-code").hidden = false;
    $("pair-expired").hidden = true;
    clearInterval(countdown);
    countdown = setInterval(tick, 1000);
    tick();
  }

  $("pair-create").addEventListener("click", issueCode);

  // tick counts the secret down and hides the code when it expires.
  function tick() {
    const left = Math.ceil((expiresAt - Date.now()) / 1000);
    if (left <= 0) {
      hideCode();
      $("pair-expired").hidden = false;
      return;
    }
    const time = `${Math.floor(left / 60)}:${String(left % 60).padStart(2, "0")}`;
    $("pair-timer").replaceChildren(fill(S.expiresIn, { time }));
  }

  // hideCode takes the code off the screen and stops its countdown.
  function hideCode() {
    clearInterval(countdown);
    $("pair-code").hidden = true;
    $("pair-qr").removeAttribute("src");
    $("pair-text").value = "";
  }

  // dropCode forgets the code on screen, so that entering the section again
  // issues a new one.
  function dropCode() {
    expiresAt = 0;
    hideCode();
  }

  $("pair-copy").addEventListener("click", async () => {
    const text = $("pair-text");
    const msg = $("pair-msg");
    try {
      await navigator.clipboard.writeText(text.value);
      say(msg, S.copied);
    } catch {
      text.select();
      say(msg, S.copyFailed);
    }
  });

  let shownRefusals = "";

  async function loadRefusals() {
    let list;
    try {
      list = await api("GET", "/api/pairing/refusals");
    } catch {
      return; // the pairing itself still works; the next poll retries
    }
    const key = JSON.stringify(list);
    if (key === shownRefusals) return;
    shownRefusals = key;
    const container = $("refusals-list");
    keepFocus(container, () => {
      $("refusals").hidden = list.length === 0;
      container.replaceChildren(...list.map(refusalCard));
    });
  }

  function refusalCard(r) {
    let add = null;
    // Only an id the server could serve gets the offer.
    if (ID_PATTERN.test(r.app)) {
      add = h("button", { type: "button", className: "primary", textContent: S.addIt, onclick: () => addRefused(r.app) });
      add.dataset.key = "add:" + r.app;
    }
    return h("li", { className: "card" },
      h("p", null, fill(S.refused, { app: h("code", null, iso(r.app)) })),
      h("dl", { className: "facts" },
        fact(S.device, r.device ? iso(r.device) : "—"),
        fact(S.when, formatTime(r.at))),
      add && h("div", { className: "actions start" }, add));
  }

  async function addRefused(id) {
    const msg = $("pair-msg");
    try {
      await api("POST", "/api/apps", { id });
    } catch (e) {
      say(msg, errorText(e), true);
      return;
    }
    say(msg, S.addedRetry);
    shownRefusals = "";
    loadRefusals();
  }

  show(location.hash.slice(1));
})();
