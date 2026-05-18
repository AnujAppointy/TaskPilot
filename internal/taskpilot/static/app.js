const apiState = {
  token: loadSetting("taskpilot.token", "dev-token"),
  actor: loadSetting("taskpilot.actor", ""),
  actorSecret: loadSetting("taskpilot.actorSecret", ""),
  tab: "board",
  selected: null,
  tasks: [],
  actors: [],
  handoffs: [],
  events: [],
  detail: null,
  error: "",
};

const statuses = ["ready", "claimed", "in_progress", "blocked", "handoff_ready", "in_review", "completed"];

function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k === "class") el.className = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2).toLowerCase(), v);
    else if (v !== undefined && v !== null) el.setAttribute(k, v);
  }
  for (const child of children.flat()) {
    if (child === undefined || child === null) continue;
    el.append(child.nodeType ? child : document.createTextNode(String(child)));
  }
  return el;
}

function loadSetting(key, fallback) {
  try {
    return localStorage.getItem(key) || fallback;
  } catch {
    return fallback;
  }
}

function saveSetting(key, value) {
  try {
    localStorage.setItem(key, value);
  } catch {
    // The dashboard still works for the current tab if browser storage is blocked.
  }
}

function clearActorSettings() {
  apiState.actor = "";
  apiState.actorSecret = "";
  try {
    localStorage.removeItem("taskpilot.actor");
    localStorage.removeItem("taskpilot.actorSecret");
  } catch {
    // Ignore blocked storage.
  }
}

async function api(path, options = {}) {
  return apiRequest(path, options, true);
}

async function apiNoActor(path, options = {}) {
  return apiRequest(path, options, false);
}

async function apiRequest(path, options = {}, includeActor = true) {
  const headers = {
    "Content-Type": "application/json",
    "Authorization": `Bearer ${apiState.token}`,
    ...(options.headers || {}),
  };
  if (includeActor && apiState.actor) headers["X-Actor-ID"] = apiState.actor;
  if (includeActor && apiState.actorSecret) headers["X-Actor-Secret"] = apiState.actorSecret;
  const res = await fetch(path, {
    ...options,
    headers,
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const err = new Error(data.message || data.error || res.statusText);
    err.status = res.status;
    throw err;
  }
  return data;
}

async function refresh() {
  try {
    apiState.error = "";
    await ensureActor();
    const [tasks, actors, handoffs, events] = await Promise.all([
      api("/api/tasks"),
      api("/api/actors"),
      api("/api/handoffs"),
      api("/api/events"),
    ]);
    apiState.tasks = Array.isArray(tasks) ? tasks : [];
    apiState.actors = Array.isArray(actors) ? actors : [];
    apiState.handoffs = Array.isArray(handoffs) ? handoffs : [];
    apiState.events = Array.isArray(events) ? events : [];
    if (apiState.selected) apiState.detail = await api(`/api/tasks/${apiState.selected}`);
  } catch (err) {
    apiState.error = err.message;
  }
  render();
}

async function ensureActor() {
  if (apiState.actor && apiState.actorSecret) {
    try {
      await api("/api/actors");
      return;
    } catch (err) {
      if (err.status !== 401) throw err;
      clearActorSettings();
    }
  }
  const actor = await apiNoActor("/api/actors/register", {
    method: "POST",
    body: JSON.stringify({ name: "Dashboard User", kind: "human", machine_name: navigator.platform || "browser" }),
  });
  apiState.actor = actor.id;
  apiState.actorSecret = actor.actor_secret;
  saveSetting("taskpilot.actor", actor.id);
  saveSetting("taskpilot.actorSecret", actor.actor_secret);
}

function actorName(id) {
  const actor = apiState.actors.find(a => a.id === id);
  return (actor && actor.name) || id || "Unowned";
}

function stats() {
  return h("div", { class: "stats" },
    stat("Active", apiState.tasks.filter(t => !["completed", "cancelled"].includes(t.status)).length),
    stat("Blocked", apiState.tasks.filter(t => t.status === "blocked" || (t.blockers || []).length).length),
    stat("Conflicts", apiState.tasks.reduce((n, t) => n + (t.potential_conflict_count || 0), 0)),
    stat("Handoffs", apiState.handoffs.filter(h => h.status === "prepared").length),
    stat("Completed", apiState.tasks.filter(t => t.status === "completed").length),
  );
}

function stat(label, value) {
  return h("div", { class: "stat" }, h("strong", {}, value), h("span", {}, label));
}

function board() {
  return h("div", {},
    stats(),
    h("div", { class: "board" }, statuses.map(status =>
      h("div", { class: "column" },
        h("h3", {}, status.split("_").join(" ")),
        apiState.tasks.filter(t => t.status === status).map(taskCard)
      )
    ))
  );
}

function taskCard(t) {
  return h("div", { class: "card", onclick: () => selectTask(t.id) },
    h("div", { class: "card-title" }, t.title),
    h("div", { class: "meta" },
      h("span", {}, `Owner: ${actorName(t.owner_id)}`),
      h("span", {}, `Priority: ${t.priority}`),
      h("span", {}, `Updated: ${new Date(t.updated_at).toLocaleString()}`),
      h("span", { class: "pill" }, `${t.active_lock_count || 0} locks`),
      (t.potential_conflict_count || 0) > 0 ? h("span", { class: "pill red" }, "conflict") : null,
      (t.blockers || []).length ? h("span", { class: "pill amber" }, "blocked") : null,
    )
  );
}

async function selectTask(id) {
  apiState.selected = id;
  apiState.tab = "detail";
  apiState.detail = await api(`/api/tasks/${id}`);
  render();
}

function createTaskForm() {
  const title = h("input", { placeholder: "Title" });
  const goal = h("textarea", { placeholder: "Goal" });
  const scope = h("input", { placeholder: "Scope, comma separated" });
  const type = h("select", {}, ["planning","research","implementation","review","debugging","documentation","other"].map(v => h("option", { value: v }, v)));
  const priority = h("select", {}, ["normal","low","high","urgent"].map(v => h("option", { value: v }, v)));
  return h("div", { class: "panel" },
    h("h2", {}, "Create Task"),
    h("div", { class: "form" },
      title, goal,
      h("div", { class: "row" }, type, priority),
      scope,
      h("button", { class: "primary", onclick: async () => {
        await api("/api/tasks", { method: "POST", body: JSON.stringify({
          title: title.value, goal: goal.value, type: type.value, priority: priority.value,
          scope: scope.value.split(",").map(s => s.trim()).filter(Boolean),
        })});
        title.value = ""; goal.value = ""; scope.value = "";
        await refresh();
      }}, "Create")
    )
  );
}

function detailView() {
  if (!apiState.detail) return h("div", { class: "panel" }, "Select a task.");
  const { task } = apiState.detail;
  const context = Array.isArray(apiState.detail.context) ? apiState.detail.context : [];
  const locks = Array.isArray(apiState.detail.locks) ? apiState.detail.locks : [];
  const handoffs = Array.isArray(apiState.detail.handoffs) ? apiState.detail.handoffs : [];
  const events = Array.isArray(apiState.detail.events) ? apiState.detail.events : [];
  return h("div", { class: "grid2" },
    h("div", {}, createTaskForm(), h("br"), actionsPanel(task)),
    h("div", { class: "panel detail" },
      h("h2", {}, task.title),
      h("div", { class: "meta" },
        h("span", {}, `Goal: ${task.goal}`),
        h("span", {}, `Status: ${task.status}`),
        h("span", {}, `Owner: ${actorName(task.owner_id)}`),
        h("span", {}, `Scope: ${(task.scope || []).join(", ") || "none"}`)
      ),
      section("Context", context.map(c => h("div", { class: "item" }, h("strong", {}, c.kind), h("p", {}, c.content), h("small", {}, actorName(c.author_id))))),
      section("Locks", locks.map(l => h("div", { class: "item" }, `${l.scope_type}: ${l.scope} · owner ${actorName(l.owner_id)} · expires ${new Date(l.expires_at).toLocaleString()}`))),
      section("Handoffs", handoffs.map(x => h("div", { class: "item" }, h("strong", {}, x.status), h("p", {}, x.resume_summary), h("p", {}, `Next: ${(x.next_steps || []).join(", ")}`)))),
      section("Timeline", h("div", { class: "timeline" }, events.map(e => h("div", { class: "event" }, `${e.id} · ${e.event_type} · ${new Date(e.created_at).toLocaleString()}`))))
    )
  );
}

function section(title, content) {
  return h("div", { class: "section" }, h("h3", {}, title), Array.isArray(content) && !content.length ? h("p", { class: "meta" }, "Nothing yet.") : content);
}

function actionsPanel(task) {
  const status = h("select", {}, statuses.concat(["cancelled"]).map(v => h("option", { value: v, selected: v === task.status ? "selected" : null }, v)));
  const noteKind = h("select", {}, ["note","summary","decision","risk","blocker","output_ref"].map(v => h("option", { value: v }, v)));
  const note = h("textarea", { placeholder: "Add sanitized context" });
  const lockScope = h("input", { placeholder: "Lock scope, e.g. src/auth/*" });
  const handoffSummary = h("textarea", { placeholder: "Handoff resume summary" });
  const nextSteps = h("input", { placeholder: "Next steps, comma separated" });
  return h("div", { class: "panel" },
    h("h2", {}, "Actions"),
    h("div", { class: "form" },
      h("button", { onclick: async () => { await api(`/api/tasks/${task.id}/claim`, { method: "POST", body: "{}" }); await refresh(); } }, "Claim"),
      h("div", { class: "row" }, status, h("button", { onclick: async () => { await api(`/api/tasks/${task.id}`, { method: "PATCH", body: JSON.stringify({ status: status.value }) }); await refresh(); } }, "Update Status")),
      h("div", { class: "row" }, noteKind, h("button", { onclick: async () => { await api(`/api/tasks/${task.id}/context`, { method: "POST", body: JSON.stringify({ kind: noteKind.value, content: note.value }) }); note.value = ""; await refresh(); } }, "Add Context")),
      note,
      h("div", { class: "row" }, lockScope, h("button", { onclick: async () => { await api(`/api/tasks/${task.id}/locks`, { method: "POST", body: JSON.stringify({ scope: lockScope.value, scope_type: "file_glob" }) }); lockScope.value = ""; await refresh(); } }, "Acquire Lock")),
      handoffSummary, nextSteps,
      h("button", { onclick: async () => { await api(`/api/tasks/${task.id}/handoff`, { method: "POST", body: JSON.stringify({ summary: handoffSummary.value, next_steps: nextSteps.value.split(",").map(s => s.trim()).filter(Boolean) }) }); await refresh(); } }, "Prepare Handoff"),
      h("button", { class: "primary", onclick: async () => { await api(`/api/tasks/${task.id}/complete`, { method: "POST", body: JSON.stringify({ summary: "Completed from dashboard." }) }); await refresh(); } }, "Complete")
    )
  );
}

function actorsView() {
  return h("div", { class: "grid2" }, actorForm(), h("div", { class: "panel list" }, h("h2", {}, "People / Agents"), apiState.actors.map(a => h("div", { class: "item" }, h("strong", {}, a.name), h("p", {}, `${a.kind} · ${a.machine_name || "no machine"} · ${a.id}`)))));
}

function actorForm() {
  const name = h("input", { placeholder: "Name" });
  const machine = h("input", { placeholder: "Machine name" });
  const kind = h("select", {}, h("option", { value: "agent" }, "agent"), h("option", { value: "human" }, "human"));
  return h("div", { class: "panel form" }, h("h2", {}, "Register Actor"), name, kind, machine, h("button", { class: "primary", onclick: async () => {
    const actor = await apiNoActor("/api/actors/register", { method: "POST", body: JSON.stringify({ name: name.value, kind: kind.value, machine_name: machine.value }) });
    saveSetting("taskpilot.actor", actor.id);
    saveSetting("taskpilot.actorSecret", actor.actor_secret);
    apiState.actor = actor.id;
    apiState.actorSecret = actor.actor_secret;
    await refresh();
  }}, "Register and Use"));
}

function handoffsView() {
  return h("div", { class: "panel list" }, h("h2", {}, "Handoffs"), apiState.handoffs.map(x => h("div", { class: "item" },
    h("strong", {}, `${x.status} · ${x.task_id}`),
    h("p", {}, x.resume_summary),
    h("p", {}, `Next: ${(x.next_steps || []).join(", ")}`),
    x.status === "prepared" ? h("button", { onclick: async () => { await api(`/api/handoffs/${x.id}/accept`, { method: "POST", body: "{}" }); await refresh(); } }, "Accept") : null
  )));
}

function conflictsView() {
  const conflicts = apiState.tasks.filter(t => (t.potential_conflict_count || 0) > 0 || (t.claim_expires_at && new Date(t.claim_expires_at) < new Date()));
  return h("div", { class: "panel list" }, h("h2", {}, "Conflicts and Stale Work"), conflicts.length ? conflicts.map(taskCard) : h("p", { class: "meta" }, "No conflicts detected."));
}

function settings() {
  const token = h("input", { value: apiState.token, placeholder: "Team token" });
  const actor = h("input", { value: apiState.actor, placeholder: "Actor ID" });
  const actorSecret = h("input", { value: apiState.actorSecret, placeholder: "Actor secret" });
  return h("div", { class: "panel form" }, h("h2", {}, "Connection"), token, actor, actorSecret, h("button", { class: "primary", onclick: async () => {
    saveSetting("taskpilot.token", token.value);
    saveSetting("taskpilot.actor", actor.value);
    saveSetting("taskpilot.actorSecret", actorSecret.value);
    apiState.token = token.value;
    apiState.actor = actor.value;
    apiState.actorSecret = actorSecret.value;
    await refresh();
  }}, "Save"));
}

function render() {
  try {
    let root = document.getElementById("root");
    if (!root) {
      document.body.innerHTML = "";
      root = h("div", { id: "root" });
      document.body.append(root);
    }
    root.innerHTML = "";
    const tabs = ["board", "detail", "conflicts", "actors", "handoffs", "settings"];
    const content = apiState.tab === "board" ? board()
      : apiState.tab === "detail" ? detailView()
      : apiState.tab === "conflicts" ? conflictsView()
      : apiState.tab === "actors" ? actorsView()
      : apiState.tab === "handoffs" ? handoffsView()
      : settings();
    root.append(h("div", { class: "shell" },
      h("div", { class: "topbar" },
        h("div", { class: "brand" }, "TaskPilot"),
        h("div", { class: "tabs" }, tabs.map(t => h("button", { class: apiState.tab === t ? "active" : "", onclick: () => { apiState.tab = t; render(); } }, t)))
      ),
      h("main", { class: "main" }, apiState.error ? h("div", { class: "panel error" }, apiState.error) : null, content)
    ));
  } catch (err) {
    document.body.innerHTML = `<div style="font:14px system-ui;padding:24px"><h1>TaskPilot dashboard error</h1><p>${String(err.message || err)}</p></div>`;
  }
}

render();
refresh();
setInterval(refresh, 5000);
