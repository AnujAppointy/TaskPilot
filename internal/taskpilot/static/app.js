const apiState = {
  token: loadSetting("taskpilot.token", "dev-token"),
  apiKey: loadSetting("taskpilot.apiKey", ""),
  legacyEnabled: loadSetting("taskpilot.legacyEnabled", "") === "true",
  actor: loadSetting("taskpilot.actor", ""),
  actorSecret: loadSetting("taskpilot.actorSecret", ""),
  principal: null,
  tab: "board",
  selected: null,
  tasks: [],
  actors: [],
  projects: [],
  repositories: [],
  workspaces: [],
  selectedProject: loadSetting("taskpilot.project", ""),
  filters: {
    search: "",
    owner: "",
    status: "",
    repo: "",
    priority: "",
    blocked: "",
    stale: "",
  },
  users: [],
  apiKeys: [],
  handoffs: [],
  conflicts: [],
  staleClaims: [],
  events: [],
  detail: null,
  error: "",
  authChecked: false,
  streamActive: false,
  streamController: null,
  streamRetry: null,
  refreshTimer: null,
  refreshing: false,
  refreshQueued: false,
  lastEventID: 0,
  pendingRender: false,
  memoryError: "",
  handoffModal: null,
};

const statuses = ["ready", "claimed", "in_progress", "blocked", "handoff_ready", "in_review", "completed"];
const writeRoles = ["admin", "maintainer", "developer", "agent"];

function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k === "class") el.className = v;
    else if (k === "checked") el.checked = !!v;
    else if (k === "selected") el.selected = !!v;
    else if (k === "value") el.value = v;
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
    if (value) localStorage.setItem(key, value);
    else localStorage.removeItem(key);
  } catch {
    // The current tab can still use in-memory state if storage is blocked.
  }
}

function isFormEditing() {
  const el = document.activeElement;
  if (!el) return false;
  return ["INPUT", "TEXTAREA", "SELECT"].includes(el.tagName);
}

function renderWhenSafe() {
  if (isFormEditing()) {
    apiState.pendingRender = true;
    return;
  }
  apiState.pendingRender = false;
  render();
}

function clearActorSettings() {
  apiState.actor = "";
  apiState.actorSecret = "";
  saveSetting("taskpilot.actor", "");
  saveSetting("taskpilot.actorSecret", "");
}

function setLegacyEnabled(value) {
  apiState.legacyEnabled = value;
  saveSetting("taskpilot.legacyEnabled", value ? "true" : "");
}

function isAdmin() {
  return apiState.principal && apiState.principal.role === "admin";
}

function canWrite() {
  return apiState.principal && writeRoles.includes(apiState.principal.role);
}

function authHeaders(includeActor = true) {
  const headers = { "Content-Type": "application/json" };
  if (apiState.apiKey) {
    headers.Authorization = `ApiKey ${apiState.apiKey}`;
  } else if (apiState.legacyEnabled && apiState.token) {
    headers.Authorization = `Bearer ${apiState.token}`;
    if (includeActor && apiState.actor) headers["X-Actor-ID"] = apiState.actor;
    if (includeActor && apiState.actorSecret) headers["X-Actor-Secret"] = apiState.actorSecret;
  }
  return headers;
}

async function api(path, options = {}) {
  return apiRequest(path, options, true);
}

async function apiNoActor(path, options = {}) {
  return apiRequest(path, options, false);
}

async function apiRequest(path, options = {}, includeActor = true) {
  const res = await fetch(path, {
    credentials: "same-origin",
    ...options,
    headers: { ...authHeaders(includeActor), ...(options.headers || {}) },
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const err = new Error(data.message || data.error || res.statusText);
    err.status = res.status;
    err.data = data;
    throw err;
  }
  return data;
}

async function loadMe() {
  try {
    apiState.principal = await api("/api/me");
    apiState.authChecked = true;
    return true;
  } catch (err) {
    if (!apiState.apiKey && apiState.legacyEnabled && apiState.token && err.status === 401) {
      try {
        await ensureLegacyActor();
        apiState.principal = await api("/api/me");
        apiState.authChecked = true;
        return true;
      } catch {
        clearActorSettings();
      }
    }
    apiState.principal = null;
    apiState.authChecked = true;
    return false;
  }
}

async function refresh() {
  if (apiState.refreshing) {
    apiState.refreshQueued = true;
    return;
  }
  apiState.refreshing = true;
  try {
    await refreshNow();
  } finally {
    apiState.refreshing = false;
    if (apiState.refreshQueued) {
      apiState.refreshQueued = false;
      scheduleRefresh(100);
    }
  }
}

async function refreshNow() {
  try {
    apiState.error = "";
    const authed = await loadMe();
    if (!authed) {
      renderWhenSafe();
      return;
    }
    if (apiState.principal.kind === "legacy_actor") await ensureLegacyActor();
    const calls = [
      apiState.selectedProject ? api(`/api/tasks?project_id=${encodeURIComponent(apiState.selectedProject)}`) : api("/api/tasks"),
      api("/api/actors"),
      api("/api/projects"),
      apiState.selectedProject ? api(`/api/repositories?project_id=${encodeURIComponent(apiState.selectedProject)}`) : api("/api/repositories"),
      apiState.selectedProject ? api(`/api/workspaces?project_id=${encodeURIComponent(apiState.selectedProject)}`) : api("/api/workspaces"),
      api("/api/handoffs"),
      api("/api/conflicts?status=open"),
      api("/api/conflicts/stale-claims"),
      api("/api/events"),
    ];
    if (isAdmin()) {
      calls.push(api("/api/users"));
      calls.push(api("/api/api-keys"));
    }
    const [tasks, actors, projects, repositories, workspaces, handoffs, conflicts, staleClaims, events, users = [], apiKeys = []] = await Promise.all(calls);
    apiState.tasks = Array.isArray(tasks) ? tasks : [];
    apiState.actors = Array.isArray(actors) ? actors : [];
    apiState.projects = Array.isArray(projects) ? projects : [];
    apiState.repositories = Array.isArray(repositories) ? repositories : [];
    apiState.workspaces = Array.isArray(workspaces) ? workspaces : [];
    if (!apiState.selectedProject && apiState.projects.length) {
      apiState.selectedProject = "project_default";
      saveSetting("taskpilot.project", apiState.selectedProject);
    }
    apiState.handoffs = Array.isArray(handoffs) ? handoffs : [];
    apiState.conflicts = Array.isArray(conflicts) ? conflicts : [];
    apiState.staleClaims = Array.isArray(staleClaims) ? staleClaims : [];
    apiState.events = Array.isArray(events) ? events : [];
    apiState.lastEventID = apiState.events.reduce((max, e) => Math.max(max, e.id || 0), apiState.lastEventID || 0);
    apiState.users = Array.isArray(users) ? users : [];
    apiState.apiKeys = Array.isArray(apiKeys) ? apiKeys : [];
    if (apiState.selected) apiState.detail = await api(`/api/tasks/${apiState.selected}`);
    ensureEventStream();
  } catch (err) {
    apiState.error = err.message;
    stopEventStream();
  }
  renderWhenSafe();
}

function scheduleRefresh(delay = 200) {
  if (apiState.refreshTimer) return;
  apiState.refreshTimer = setTimeout(async () => {
    apiState.refreshTimer = null;
    await refresh();
  }, delay);
}

function stopEventStream() {
  if (apiState.streamController) {
    apiState.streamController.abort();
  }
  apiState.streamController = null;
  apiState.streamActive = false;
  if (apiState.streamRetry) {
    clearTimeout(apiState.streamRetry);
    apiState.streamRetry = null;
  }
}

function ensureEventStream() {
  if (!apiState.principal || apiState.streamActive) return;
  apiState.streamActive = true;
  const controller = new AbortController();
  apiState.streamController = controller;
  const since = apiState.lastEventID || 0;
  fetch(`/api/events/stream?since=${encodeURIComponent(since)}`, {
    credentials: "same-origin",
    headers: authHeaders(true),
    signal: controller.signal,
  }).then(async res => {
    if (!res.ok || !res.body) throw new Error(`event stream failed: ${res.status}`);
    await readEventStream(res.body);
  }).catch(() => {
    if (controller.signal.aborted) return;
  }).finally(() => {
    if (apiState.streamController === controller) {
      apiState.streamController = null;
      apiState.streamActive = false;
      if (apiState.principal) {
        apiState.streamRetry = setTimeout(() => {
          apiState.streamRetry = null;
          ensureEventStream();
        }, 5000);
      }
    }
  });
}

async function readEventStream(body) {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    const parts = buffer.split("\n\n");
    buffer = parts.pop() || "";
    for (const part of parts) {
      handleStreamFrame(part);
    }
  }
}

function handleStreamFrame(frame) {
  const lines = frame.split("\n");
  let id = 0;
  const dataLines = [];
  for (const line of lines) {
    if (line.startsWith(":")) continue;
    if (line.startsWith("id:")) id = Number(line.slice(3).trim()) || 0;
    if (line.startsWith("data:")) dataLines.push(line.slice(5).trimStart());
  }
  if (!dataLines.length) return;
  if (id > apiState.lastEventID) apiState.lastEventID = id;
  try {
    const event = JSON.parse(dataLines.join("\n"));
    if (["context.appended", "task.heartbeat"].includes(event.event_type)) return;
  } catch {
    // Refresh on malformed frames; the next full load will reconcile state.
  }
  scheduleRefresh(150);
}

async function ensureLegacyActor() {
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
  if (actor) return actor.name;
  const user = apiState.users.find(u => u.id === id);
  if (user) return user.name || user.email;
  return id || "Unowned";
}

function projectName(id) {
  const project = apiState.projects.find(p => p.id === id);
  return (project && project.name) || id || "Default";
}

function repoName(id) {
  const repo = apiState.repositories.find(r => r.id === id);
  return (repo && repo.name) || id || "None";
}

function workspaceName(id) {
  const workspace = apiState.workspaces.find(w => w.id === id);
  return (workspace && workspace.name) || id || "None";
}

function projectFilter() {
  const select = h("select", {}, [
    h("option", { value: "", selected: apiState.selectedProject === "" }, "All projects"),
    ...apiState.projects.map(p => h("option", { value: p.id, selected: apiState.selectedProject === p.id }, p.name)),
  ]);
  return h("div", { class: "toolbar" },
    h("label", {}, "Project"),
    select,
    h("button", { onclick: async () => {
      apiState.selectedProject = select.value;
      saveSetting("taskpilot.project", select.value);
      await refresh();
    }}, "Apply")
  );
}

function taskFilters() {
  const f = apiState.filters;
  const search = h("input", { placeholder: "Search title, goal, context, decisions", value: f.search });
  const owner = h("select", {}, [h("option", { value: "" }, "Any owner")].concat(apiState.actors.map(a => h("option", { value: a.id, selected: f.owner === a.id }, a.name))));
  const status = h("select", {}, [h("option", { value: "" }, "Any status")].concat(statuses.map(s => h("option", { value: s, selected: f.status === s }, s))));
  const repo = h("select", {}, [h("option", { value: "" }, "Any repo")].concat(apiState.repositories.map(r => h("option", { value: r.id, selected: f.repo === r.id }, r.name))));
  const priority = h("select", {}, [h("option", { value: "" }, "Any priority")].concat(["low","normal","high","urgent"].map(p => h("option", { value: p, selected: f.priority === p }, p))));
  const blocked = h("select", {}, [
    h("option", { value: "", selected: f.blocked === "" }, "Any blocked state"),
    h("option", { value: "blocked", selected: f.blocked === "blocked" }, "Blocked only"),
    h("option", { value: "not_blocked", selected: f.blocked === "not_blocked" }, "Not blocked"),
  ]);
  const stale = h("select", {}, [
    h("option", { value: "", selected: f.stale === "" }, "Any stale state"),
    h("option", { value: "stale", selected: f.stale === "stale" }, "Stale only"),
    h("option", { value: "fresh", selected: f.stale === "fresh" }, "Fresh only"),
  ]);
  const apply = async () => {
    apiState.filters = { search: search.value, owner: owner.value, status: status.value, repo: repo.value, priority: priority.value, blocked: blocked.value, stale: stale.value };
    render();
  };
  for (const el of [search, owner, status, repo, priority, blocked, stale]) {
    el.addEventListener("change", apply);
  }
  search.addEventListener("input", () => {
    apiState.filters.search = search.value;
  });
  return h("div", { class: "toolbar filters" },
    h("label", {}, "Filters"),
    search, owner, status, repo, priority, blocked, stale,
    h("button", { onclick: () => render() }, "Apply"),
    h("button", { onclick: () => { apiState.filters = { search: "", owner: "", status: "", repo: "", priority: "", blocked: "", stale: "" }; render(); } }, "Clear")
  );
}

function filteredTasks() {
  const f = apiState.filters;
  const q = (f.search || "").trim().toLowerCase();
  return apiState.tasks.filter(t => {
    if (f.owner && t.owner_id !== f.owner) return false;
    if (f.status && t.status !== f.status) return false;
    if (f.repo && t.repo_id !== f.repo) return false;
    if (f.priority && t.priority !== f.priority) return false;
    const isBlocked = t.status === "blocked" || (Array.isArray(t.blockers) && t.blockers.length > 0) || (t.open_dependency_count || 0) > 0;
    if (f.blocked === "blocked" && !isBlocked) return false;
    if (f.blocked === "not_blocked" && isBlocked) return false;
    const isStale = t.claim_expires_at && new Date(t.claim_expires_at) < new Date();
    if (f.stale === "stale" && !isStale) return false;
    if (f.stale === "fresh" && isStale) return false;
    if (q) {
      const haystack = [t.title, t.goal, t.search_text, ...(t.blockers || [])].join(" ").toLowerCase();
      if (!haystack.includes(q)) return false;
    }
    return true;
  });
}

function stats() {
  const tasks = filteredTasks();
  return h("div", { class: "stats" },
    stat("Active", tasks.filter(t => !["completed", "cancelled"].includes(t.status)).length),
    stat("Blocked", tasks.filter(t => t.status === "blocked" || (t.blockers || []).length).length),
    stat("Conflicts", apiState.conflicts.length || tasks.reduce((n, t) => n + (t.potential_conflict_count || 0), 0)),
    stat("Handoffs", apiState.handoffs.filter(h => h.status === "prepared").length),
    stat("Completed", tasks.filter(t => t.status === "completed").length),
  );
}

function stat(label, value) {
  return h("div", { class: "stat" }, h("strong", {}, value), h("span", {}, label));
}

function board() {
  const tasks = filteredTasks();
  return h("div", {},
    projectFilter(),
    taskFilters(),
    stats(),
    h("div", { class: "board" }, statuses.map(status =>
      h("div", { class: "column" },
        h("h3", {}, status.split("_").join(" ")),
        tasks.filter(t => t.status === status).map(taskCard)
      )
    ))
  );
}

function taskCard(t) {
  return h("div", { class: "card", onclick: () => selectTask(t.id) },
    h("div", { class: "card-title" }, t.title),
    h("div", { class: "meta" },
      h("span", {}, `Owner: ${actorName(t.owner_id)}`),
      h("span", {}, `Project: ${projectName(t.project_id)}`),
      t.repo_id ? h("span", {}, `Repo: ${repoName(t.repo_id)}`) : null,
      h("span", {}, `Priority: ${t.priority}`),
      h("span", {}, `Updated: ${new Date(t.updated_at).toLocaleString()}`),
      h("span", { class: "pill" }, `${t.active_lock_count || 0} locks`),
      (t.subtask_count || 0) > 0 ? h("span", { class: "pill" }, `${t.subtask_count} subtasks`) : null,
      (t.open_dependency_count || 0) > 0 ? h("span", { class: "pill amber" }, `${t.open_dependency_count} blockers`) : null,
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
  if (!canWrite()) return null;
  const title = h("input", { placeholder: "Title" });
  const goal = h("textarea", { placeholder: "Goal" });
  const scope = h("input", { placeholder: "Scope, comma separated" });
  const project = h("select", {}, apiState.projects.map(p => h("option", { value: p.id, selected: (apiState.selectedProject || "project_default") === p.id }, p.name)));
  const repo = h("select", {}, [h("option", { value: "" }, "No repo")].concat(apiState.repositories.map(r => h("option", { value: r.id }, r.name))));
  const workspace = h("select", {}, [h("option", { value: "" }, "No workspace")].concat(apiState.workspaces.map(w => h("option", { value: w.id }, w.name))));
  const parent = h("select", {}, [h("option", { value: "" }, "No parent task")].concat(apiState.tasks.map(t => h("option", { value: t.id }, `${t.title} · ${t.status}`))));
  const type = h("select", {}, ["planning","research","implementation","review","debugging","documentation","other"].map(v => h("option", { value: v }, v)));
  const priority = h("select", {}, ["normal","low","high","urgent"].map(v => h("option", { value: v }, v)));
  return h("div", { class: "panel" },
    h("h2", {}, "Create Task"),
    h("div", { class: "form" },
      title, goal,
      project,
      h("div", { class: "row" }, repo, workspace),
      h("div", { class: "row" }, type, priority),
      parent,
      scope,
      h("button", { class: "primary", onclick: async () => {
        await api("/api/tasks", { method: "POST", body: JSON.stringify({
          project_id: project.value, repo_id: repo.value, workspace_id: workspace.value, parent_task_id: parent.value,
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
  if (!apiState.detail) {
    return h("div", { class: "grid2" },
      h("div", {}, createTaskForm()),
      h("div", { class: "panel" },
        h("h2", {}, "Task Detail"),
        h("p", { class: "meta" }, "Select a task from the board or create a new task here.")
      )
    );
  }
  const { task } = apiState.detail;
  const decisions = Array.isArray(apiState.detail.decisions) ? apiState.detail.decisions : [];
  const comments = Array.isArray(apiState.detail.comments) ? apiState.detail.comments : [];
  const artifacts = Array.isArray(apiState.detail.artifacts) ? apiState.detail.artifacts : [];
  const gitRefs = Array.isArray(apiState.detail.git_refs) ? apiState.detail.git_refs : [];
  const locks = Array.isArray(apiState.detail.locks) ? apiState.detail.locks : [];
  const handoffs = Array.isArray(apiState.detail.handoffs) ? apiState.detail.handoffs : [];
  const snapshots = Array.isArray(apiState.detail.snapshots) ? apiState.detail.snapshots : [];
  const latestSnapshot = apiState.detail.latest_snapshot;
  const handoffPacket = apiState.detail.handoff_packet;
  const events = visibleTimelineEvents(Array.isArray(apiState.detail.events) ? apiState.detail.events : []);
  const subtasks = Array.isArray(apiState.detail.subtasks) ? apiState.detail.subtasks : [];
  const dependencies = Array.isArray(apiState.detail.dependencies) ? apiState.detail.dependencies : [];
  const dependents = Array.isArray(apiState.detail.dependents) ? apiState.detail.dependents : [];
  const parent = apiState.detail.parent;
  return h("div", { class: "grid2" },
    h("div", {}, createTaskForm(), h("br"), actionsPanel(task)),
    h("div", { class: "panel detail" },
      h("h2", {}, task.title),
      h("div", { class: "meta" },
        h("span", {}, `Goal: ${task.goal}`),
        h("span", {}, `Status: ${task.status}`),
        h("span", {}, `Owner: ${actorName(task.owner_id)}`),
        h("span", {}, `Project: ${projectName(task.project_id)}`),
        h("span", {}, `Repo: ${repoName(task.repo_id)}`),
        h("span", {}, `Workspace: ${workspaceName(task.workspace_id)}`),
        parent ? h("span", { class: "linkish", onclick: () => selectTask(parent.id) }, `Parent: ${parent.title}`) : null,
        h("span", {}, `Scope: ${(task.scope || []).join(", ") || "none"}`)
      ),
      taskMemoryPanel(task, latestSnapshot, handoffPacket, snapshots),
      section("Subtasks", subtasks.map(subtaskItem)),
      section("Blocked By", dependencies.map(dependencyItem)),
      section("Blocking", dependents.map(dependentItem)),
      section("Decisions", decisions.map(decisionItem)),
      section("Comments", comments.map(commentItem)),
      section("Artifacts", artifacts.map(artifactItem)),
      section("Git", gitRefs.map(gitItem)),
      section("Locks", locks.map(lockItem)),
      section("Handoffs", handoffs.map(x => h("div", { class: "item" }, h("strong", {}, x.status), h("p", {}, x.resume_summary), h("p", {}, `Next: ${(x.next_steps || []).join(", ")}`)))),
      section("Timeline", h("div", { class: "timeline" }, events.map(e => h("div", { class: "event" }, `${e.id} · ${e.event_type} · ${new Date(e.created_at).toLocaleString()}`))))
    )
  );
}

function lockItem(l) {
  const stale = l.status === "stale";
  return h("div", { class: "item" },
    h("div", { class: "item-head" },
      h("strong", {}, `${l.scope_type}: ${l.scope}`),
      h("span", { class: `pill ${stale ? "amber" : l.status === "overridden" ? "red" : ""}` }, l.status || "active")
    ),
    h("p", { class: "meta" }, `Owner: ${l.owner_name || actorName(l.owner_id)} · created ${new Date(l.created_at).toLocaleString()}`),
    h("p", { class: "meta" }, `Last activity: ${l.last_heartbeat_at ? new Date(l.last_heartbeat_at).toLocaleString() : "unknown"} · expires ${new Date(l.expires_at).toLocaleString()}`),
    l.message ? h("p", {}, l.message) : null,
    canWrite() && !l.released_at && l.status !== "overridden" ? h("div", { class: "button-row" },
      h("button", { onclick: async () => { await api(`/api/locks/${l.id}/release`, { method: "POST", body: JSON.stringify({ reason: "Released from dashboard." }) }); await refresh(); } }, "Release Lock"),
      h("button", { onclick: async () => {
        const reason = prompt("Reason for overriding this lock?");
        if (!reason) return;
        await api(`/api/locks/${l.id}/override`, { method: "POST", body: JSON.stringify({ reason }) });
        await refresh();
      } }, "Override Lock")
    ) : null
  );
}

function taskMemoryPanel(task, latestSnapshot, handoffPacket, snapshots) {
  const packetTimeline = handoffPacket && handoffPacket.packet && Array.isArray(handoffPacket.packet.handoff_timeline) ? handoffPacket.packet.handoff_timeline : [];
  const packetIsWeak = handoffPacket && Array.isArray(handoffPacket.validation_errors) && handoffPacket.validation_errors.length && !packetTimeline.length;
  const snapshotIsNewer = handoffPacket && latestSnapshot && new Date(latestSnapshot.updated_at || latestSnapshot.created_at) > new Date(handoffPacket.updated_at || handoffPacket.created_at);
  const preferSnapshot = packetIsWeak && snapshotIsNewer;
  const packetMarkdown = !preferSnapshot && handoffPacket && handoffPacket.markdown ? handoffPacket.markdown : "";
  const snapshotMarkdown = latestSnapshot && latestSnapshot.markdown ? latestSnapshot.markdown : "";
  const markdown = packetMarkdown || snapshotMarkdown || "# Task Memory\n\nNo handoff packet or context snapshot has been generated yet.\n";
  const editor = h("textarea", { class: "memory-editor", value: markdown });
  const sourceLabel = handoffPacket && handoffPacket.source ? handoffPacket.source.replaceAll("_", " ") : "";
  const source = packetMarkdown ? `Handoff packet ${handoffPacket.status || "draft"} v${handoffPacket.version || 1}${sourceLabel ? ` · ${sourceLabel}` : ""} · ${new Date(handoffPacket.updated_at).toLocaleString()}` : snapshotMarkdown ? `Latest snapshot · ${new Date(latestSnapshot.updated_at).toLocaleString()}${preferSnapshot ? " · newer than weak draft" : ""}` : "No memory document yet";
  const saveMarkdown = async () => {
    apiState.memoryError = "";
    try {
      if (handoffPacket) {
        await api(`/api/handoff-packets/${handoffPacket.id}`, { method: "PATCH", body: JSON.stringify({ markdown: editor.value }) });
      } else if (latestSnapshot) {
        await api(`/api/snapshots/${latestSnapshot.id}`, { method: "PATCH", body: JSON.stringify({ markdown: editor.value }) });
      }
      await refresh();
    } catch (err) {
      const details = err.data && Array.isArray(err.data.errors) ? err.data.errors.map(e => `${e.section || "Document"}${e.line ? ` line ${e.line}` : ""}: ${e.message}`).join("\n") : err.message;
      apiState.memoryError = details;
      render();
    }
  };
  const snapshotItems = (snapshots || []).slice(-5).reverse().map(s => h("div", { class: "mini-card" },
    h("strong", {}, `${s.snapshot_type} · ${s.status_at_time}`),
    h("p", { class: "meta" }, `${new Date(s.created_at).toLocaleString()} · ${s.source_context_ids ? s.source_context_ids.length : 0} context items`)
  ));
  const validationItems = handoffPacket && Array.isArray(handoffPacket.validation_errors) ? handoffPacket.validation_errors : [];
  const evidenceItems = handoffPacket && Array.isArray(handoffPacket.supporting_evidence) ? handoffPacket.supporting_evidence : [];
  return h("div", { class: "section task-memory" },
    h("div", { class: "item-head" },
      h("div", {}, h("h3", {}, "Task Memory"), h("p", { class: "meta" }, source)),
      canWrite() ? h("div", { class: "button-row" },
        h("button", { onclick: async () => { await api(`/api/tasks/${task.id}/snapshots`, { method: "POST", body: JSON.stringify({ snapshot_type: "manual" }) }); await refresh(); } }, "Generate Snapshot"),
        h("button", { onclick: () => openHandoffModal(task, latestSnapshot, handoffPacket) }, "Prepare handoff for other agent")
      ) : null
    ),
    h("pre", { class: "markdown-doc" }, markdown),
    validationItems.length ? h("div", { class: "error-box" },
      h("strong", {}, "Handoff needs stronger content before publish:"),
      h("ul", {}, validationItems.map(e => h("li", {}, `${e.section || "Document"}: ${e.message}`)))
    ) : null,
    evidenceItems.length ? h("details", {},
      h("summary", {}, "Supporting evidence"),
      h("ul", {}, evidenceItems.map(item => h("li", {}, item)))
    ) : null,
    apiState.memoryError ? h("pre", { class: "error-box" }, apiState.memoryError) : null,
    canWrite() ? h("details", { class: "memory-edit" },
      h("summary", {}, "Edit Markdown"),
      editor,
      h("div", { class: "button-row" },
        handoffPacket || latestSnapshot ? h("button", { class: "primary", onclick: saveMarkdown }, handoffPacket ? "Save Draft" : "Save Snapshot") : null
      )
    ) : null,
    snapshotItems.length ? h("details", {}, h("summary", {}, "Recent snapshots"), h("div", { class: "snapshot-list" }, snapshotItems)) : null
  );
}

function openHandoffModal(task, latestSnapshot, handoffPacket) {
  const packet = handoffPacket && handoffPacket.packet ? handoffPacket.packet : {};
  const snapshot = latestSnapshot && latestSnapshot.summary ? latestSnapshot.summary : {};
  const next = packet.suggested_next_steps || snapshot.next_recommended_actions || [];
  apiState.handoffModal = {
    taskId: task.id,
    title: task.title,
    summary: packet.handoff_message || packet.task_objective || snapshot.implementation_direction || task.goal || "",
    nextText: Array.isArray(next) ? next.join("\n") : "",
    error: "",
  };
  render();
}

function closeHandoffModal() {
  apiState.handoffModal = null;
  render();
}

function parseHandoffNextSteps(text) {
  return (text || "")
    .split(/\n/)
    .map(s => s.trim())
    .filter(Boolean);
}

function handoffModalView() {
  const modal = apiState.handoffModal;
  if (!modal) return null;
  const summary = h("textarea", { value: modal.summary, placeholder: "Ready for next agent" });
  const nextSteps = h("textarea", { value: modal.nextText, placeholder: "Write test\nPatch logic" });
  return h("div", { class: "modal-backdrop", onclick: (event) => { if (event.target.className === "modal-backdrop") closeHandoffModal(); } },
    h("div", { class: "modal-card" },
      h("div", { class: "item-head" },
        h("div", {},
          h("h2", {}, "Prepare Handoff"),
          h("p", { class: "meta" }, `${modal.title} · ${modal.taskId}`)
        ),
        h("button", { onclick: closeHandoffModal }, "Close")
      ),
      h("label", {}, "Summary"),
      summary,
      h("label", {}, "Next steps"),
      nextSteps,
      modal.error ? h("div", { class: "error-box" }, modal.error) : null,
      h("div", { class: "button-row modal-actions" },
        h("button", { onclick: closeHandoffModal }, "Cancel"),
        h("button", { class: "primary", onclick: async () => {
          const body = {
            summary: summary.value.trim(),
            next_steps: parseHandoffNextSteps(nextSteps.value),
          };
          if (!body.summary) {
            apiState.handoffModal.error = "Summary is required before publishing a handoff.";
            render();
            return;
          }
          try {
            await api(`/api/tasks/${modal.taskId}/handoff`, { method: "POST", body: JSON.stringify(body) });
            apiState.handoffModal = null;
            apiState.tab = "handoffs";
            await refresh();
          } catch (err) {
            apiState.handoffModal.error = err.message;
            render();
          }
        } }, "Publish Handoff")
      )
    )
  );
}

function section(title, content) {
  return h("div", { class: "section" }, h("h3", {}, title), Array.isArray(content) && !content.length ? h("p", { class: "meta" }, "Nothing yet.") : content);
}

function visibleTimelineEvents(events) {
  const hidden = new Set(["context.appended", "task.heartbeat"]);
  return events.filter(e => !hidden.has(e.event_type)).slice(-80);
}

function subtaskItem(t) {
  return h("div", { class: "item clickable", onclick: () => selectTask(t.id) },
    h("strong", {}, t.title),
    h("p", { class: "meta" }, `${t.status} · ${t.priority} · ${t.id}`)
  );
}

function dependencyItem(dep) {
  const t = dep.depends_on_task || {};
  const titleAttrs = t.id ? { class: "linkish", onclick: () => selectTask(t.id) } : {};
  return h("div", { class: "item" },
    h("div", { class: "item-head" },
      h("strong", titleAttrs, t.title || dep.depends_on_id),
      canWrite() ? h("button", { class: "danger", onclick: async () => { await api(`/api/dependencies/${dep.id}`, { method: "DELETE" }); await refresh(); } }, "Remove") : null
    ),
    h("p", { class: "meta" }, `${t.status || "unknown"} · dependency ${dep.id}`)
  );
}

function dependentItem(dep) {
  const t = dep.task || {};
  const attrs = t.id ? { class: "item clickable", onclick: () => selectTask(t.id) } : { class: "item" };
  return h("div", attrs,
    h("strong", {}, t.title || dep.task_id),
    h("p", { class: "meta" }, `${t.status || "unknown"} · waits for this task`)
  );
}

function decisionItem(d) {
  return h("div", { class: "item" },
    h("strong", {}, d.decision),
    d.reason ? h("p", {}, `Reason: ${d.reason}`) : null,
    d.impact ? h("p", {}, `Impact: ${d.impact}`) : null,
    Array.isArray(d.alternatives) && d.alternatives.length ? h("p", { class: "meta" }, `Alternatives: ${d.alternatives.join(", ")}`) : null,
    h("small", { class: "meta" }, `${actorName(d.author_id)} · ${new Date(d.created_at).toLocaleString()}`)
  );
}

function commentItem(c) {
  return h("div", { class: "item" },
    h("p", {}, c.body),
    h("small", { class: "meta" }, `${actorName(c.author_id)} · ${new Date(c.created_at).toLocaleString()}`)
  );
}

function artifactItem(a) {
  const link = /^https?:\/\//.test(a.uri) ? h("a", { href: a.uri, target: "_blank", rel: "noreferrer" }, a.uri) : h("span", {}, a.uri);
  return h("div", { class: "item" },
    h("strong", {}, `${a.kind}: ${a.title}`),
    h("p", {}, link),
    a.description ? h("p", { class: "meta" }, a.description) : null,
    h("small", { class: "meta" }, `${actorName(a.author_id)} · ${new Date(a.created_at).toLocaleString()}`)
  );
}

function gitItem(g) {
  return h("div", { class: "item" },
    g.branch ? h("p", {}, `Branch: ${g.branch}`) : null,
    g.commit_sha ? h("p", {}, `Commit: ${g.commit_sha}`) : null,
    g.pr_url ? h("p", {}, h("a", { href: g.pr_url, target: "_blank", rel: "noreferrer" }, g.pr_url)) : null,
    Array.isArray(g.changed_files) && g.changed_files.length ? h("p", { class: "meta" }, `Files: ${g.changed_files.join(", ")}`) : null,
    g.note ? h("p", { class: "meta" }, g.note) : null,
    h("small", { class: "meta" }, `${actorName(g.author_id)} · ${new Date(g.created_at).toLocaleString()}`)
  );
}

function actionsPanel(task) {
  if (!canWrite()) return h("div", { class: "panel" }, h("h2", {}, "Actions"), h("p", { class: "meta" }, "Your role is read-only."));
  const status = h("select", {}, ["blocked", "handoff_ready", "in_review", "cancelled"].map(v => h("option", { value: v, selected: v === task.status }, v)));
  const lockScope = h("input", { placeholder: "Lock scope, e.g. src/auth/*" });
  const decision = h("textarea", { placeholder: "Decision" });
  const decisionReason = h("input", { placeholder: "Reason" });
  const decisionImpact = h("input", { placeholder: "Impact" });
  const decisionAlternatives = h("input", { placeholder: "Alternatives, comma separated" });
  const comment = h("textarea", { placeholder: "Human comment or review note" });
  const artifactKind = h("select", {}, ["pr","log","branch","doc","screenshot","output","other"].map(v => h("option", { value: v }, v)));
  const artifactTitle = h("input", { placeholder: "Artifact title" });
  const artifactURI = h("input", { placeholder: "Artifact URL/path/reference" });
  const artifactDescription = h("textarea", { placeholder: "Artifact description" });
  const gitBranch = h("input", { placeholder: "Branch" });
  const gitCommit = h("input", { placeholder: "Commit SHA" });
  const gitPR = h("input", { placeholder: "PR URL" });
  const gitFiles = h("textarea", { placeholder: "Changed files, comma separated" });
  const gitNote = h("input", { placeholder: "Git note" });
  const subtaskTitle = h("input", { placeholder: "Subtask title" });
  const subtaskGoal = h("textarea", { placeholder: "Subtask goal" });
  const dependencyOptions = apiState.tasks.filter(t => t.id !== task.id && t.project_id === task.project_id);
  const dependency = h("select", {}, [h("option", { value: "" }, "Select blocking task")].concat(dependencyOptions.map(t => h("option", { value: t.id }, `${t.title} · ${t.status}`))));
  return h("div", { class: "panel" },
    h("h2", {}, "Actions"),
    h("div", { class: "form" },
      h("button", { onclick: async () => { await api(`/api/tasks/${task.id}/claim`, { method: "POST", body: "{}" }); await refresh(); } }, "Claim"),
      h("div", { class: "row" }, status, h("button", { onclick: async () => { await api(`/api/tasks/${task.id}`, { method: "PATCH", body: JSON.stringify({ status: status.value }) }); await refresh(); } }, "Apply Manual Status")),
      h("div", { class: "button-row" },
        h("button", { onclick: async () => { await api(`/api/tasks/${task.id}`, { method: "PATCH", body: JSON.stringify({ status: "blocked" }) }); await refresh(); } }, "Mark Blocked"),
        h("button", { onclick: () => openHandoffModal(task, apiState.detail && apiState.detail.latest_snapshot, apiState.detail && apiState.detail.handoff_packet) }, "Prepare Handoff"),
        h("button", { onclick: async () => { await api(`/api/tasks/${task.id}`, { method: "PATCH", body: JSON.stringify({ status: "in_review" }) }); await refresh(); } }, "Send To Review"),
        h("button", { class: "primary", onclick: async () => { await api(`/api/tasks/${task.id}/complete`, { method: "POST", body: JSON.stringify({ summary: "Completed from dashboard." }) }); await refresh(); } }, "Mark Complete")
      ),
      h("h3", {}, "Subtasks"),
      subtaskTitle, subtaskGoal,
      h("button", { onclick: async () => { await api(`/api/tasks/${task.id}/subtasks`, { method: "POST", body: JSON.stringify({ title: subtaskTitle.value, goal: subtaskGoal.value, type: task.type, priority: task.priority }) }); subtaskTitle.value = ""; subtaskGoal.value = ""; await refresh(); } }, "Create Subtask"),
      h("h3", {}, "Dependencies"),
      h("div", { class: "row" }, dependency, h("button", { onclick: async () => { if (!dependency.value) return; await api(`/api/tasks/${task.id}/dependencies`, { method: "POST", body: JSON.stringify({ depends_on_id: dependency.value }) }); dependency.value = ""; await refresh(); } }, "Add Blocker")),
      h("h3", {}, "Decision"),
      decision, decisionReason, decisionImpact, decisionAlternatives,
      h("button", { onclick: async () => {
        await api(`/api/tasks/${task.id}/decisions`, { method: "POST", body: JSON.stringify({
          decision: decision.value,
          reason: decisionReason.value,
          impact: decisionImpact.value,
          alternatives: decisionAlternatives.value.split(",").map(s => s.trim()).filter(Boolean),
        }) });
        decision.value = ""; decisionReason.value = ""; decisionImpact.value = ""; decisionAlternatives.value = "";
        await refresh();
      } }, "Record Decision"),
      h("h3", {}, "Comment"),
      comment,
      h("button", { onclick: async () => { await api(`/api/tasks/${task.id}/comments`, { method: "POST", body: JSON.stringify({ body: comment.value }) }); comment.value = ""; await refresh(); } }, "Add Comment"),
      h("h3", {}, "Artifact Reference"),
      h("div", { class: "row" }, artifactKind, artifactTitle),
      artifactURI, artifactDescription,
      h("button", { onclick: async () => {
        await api(`/api/tasks/${task.id}/artifacts`, { method: "POST", body: JSON.stringify({ kind: artifactKind.value, title: artifactTitle.value, uri: artifactURI.value, description: artifactDescription.value }) });
        artifactTitle.value = ""; artifactURI.value = ""; artifactDescription.value = "";
        await refresh();
      } }, "Add Artifact"),
      h("h3", {}, "Git Metadata"),
      h("div", { class: "row" }, gitBranch, gitCommit),
      gitPR, gitFiles, gitNote,
      h("button", { onclick: async () => {
        await api(`/api/tasks/${task.id}/git`, { method: "POST", body: JSON.stringify({
          branch: gitBranch.value,
          commit_sha: gitCommit.value,
          pr_url: gitPR.value,
          changed_files: gitFiles.value.split(",").map(s => s.trim()).filter(Boolean),
          note: gitNote.value,
        }) });
        gitBranch.value = ""; gitCommit.value = ""; gitPR.value = ""; gitFiles.value = ""; gitNote.value = "";
        await refresh();
      } }, "Attach Git Metadata"),
      h("div", { class: "row" }, lockScope, h("button", { onclick: async () => { try { await api(`/api/tasks/${task.id}/locks`, { method: "POST", body: JSON.stringify({ scope: lockScope.value, scope_type: "file_glob" }) }); lockScope.value = ""; } finally { await refresh(); } } }, "Acquire Lock"))
    )
  );
}

function actorsView() {
  return h("div", { class: "grid2" }, actorForm(), h("div", { class: "panel list" }, h("h2", {}, "People / Agents"), apiState.actors.map(a => h("div", { class: "item" }, h("strong", {}, a.name), h("p", {}, `${a.kind} · ${a.machine_name || "no machine"} · ${a.id}`)))));
}

function actorForm() {
  if (!canWrite()) return h("div", { class: "panel" }, h("h2", {}, "Register Actor"), h("p", { class: "meta" }, "Your role is read-only."));
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
  const seenTasks = new Set();
  const published = apiState.handoffs.filter(x => {
    if (x.status !== "prepared") return false;
    if (seenTasks.has(x.task_id)) return false;
    seenTasks.add(x.task_id);
    return true;
  });
  return h("div", { class: "panel list" },
    h("h2", {}, "Handoffs"),
    published.length ? published.map(handoffItem) : h("p", { class: "meta" }, "No published handoffs.")
  );
}

function handoffItem(x) {
  const task = x.task || apiState.tasks.find(t => t.id === x.task_id) || {};
  const packet = x.packet && x.packet.packet ? x.packet.packet : {};
  const next = x.next_steps && x.next_steps.length ? x.next_steps : (packet.suggested_next_steps || []);
  return h("div", { class: "item" },
    h("div", { class: "item-head" },
      h("strong", {}, `${task.title || x.task_id}`),
      h("span", { class: "pill amber" }, x.packet ? `handoff v${x.packet.version || 1}` : "handoff")
    ),
    h("p", {}, x.resume_summary || packet.task_objective),
    h("p", { class: "meta" }, `Task: ${x.task_id} · owner ${actorName(task.owner_id || x.from_actor_id)} · ${new Date(x.created_at).toLocaleString()}`),
    next.length ? h("ol", { class: "compact-list" }, next.map(step => h("li", {}, step))) : h("p", { class: "meta" }, "No next steps provided."),
    x.packet ? h("details", {},
      h("summary", {}, "More details"),
      packet.files_components_affected && packet.files_components_affected.length ? h("p", { class: "meta" }, `Files: ${packet.files_components_affected.slice(0, 5).join(", ")}`) : null,
      packet.risks && packet.risks.length ? h("p", { class: "meta" }, `Risks: ${packet.risks.slice(0, 3).join("; ")}`) : null
    ) : null,
    h("div", { class: "button-row" },
      task.id ? h("button", { onclick: () => selectTask(task.id) }, "Open Task") : null,
      canWrite() ? h("button", { class: "primary", onclick: async () => { await api(`/api/handoffs/${x.id}/accept`, { method: "POST", body: "{}" }); await refresh(); } }, "Acquire / Accept Handoff") : null
    )
  );
}

function conflictsView() {
  return h("div", { class: "grid2" },
    h("div", { class: "panel list" },
      h("h2", {}, "Open Conflicts"),
      apiState.conflicts.length ? apiState.conflicts.map(conflictItem) : h("p", { class: "meta" }, "No conflicts detected.")
    ),
    h("div", { class: "panel list" },
      h("h2", {}, "Stale Claims"),
      apiState.staleClaims.length ? apiState.staleClaims.map(staleClaimItem) : h("p", { class: "meta" }, "No stale claims.")
    )
  );
}

function staleClaimItem(item) {
  const task = item.task || {};
  const owner = item.owner ? item.owner.name : actorName(task.owner_id);
  return h("div", { class: "item conflict-card" },
    h("div", { class: "item-head" },
      h("strong", {}, task.title || task.id),
      h("span", { class: "pill amber" }, "stale claim")
    ),
    h("p", {}, item.reason || "Claim appears stale."),
    h("p", { class: "meta" }, `Task: ${task.id} · owner ${owner}`),
    h("p", { class: "meta" }, `Claimed: ${item.claim_timestamp ? new Date(item.claim_timestamp).toLocaleString() : "unknown"} · last activity: ${item.last_activity_timestamp ? new Date(item.last_activity_timestamp).toLocaleString() : "unknown"}`),
    h("p", { class: "meta" }, `Threshold: ${item.stale_threshold || "unknown"} · actions: ${(item.suggested_actions || []).join(", ")}`),
    h("div", { class: "button-row" },
      task.id ? h("button", { onclick: () => selectTask(task.id) }, "Open Task") : null,
      canWrite() && task.id ? h("button", { onclick: async () => { await api(`/api/tasks/${task.id}/release`, { method: "POST", body: "{}" }); await refresh(); } }, "Release Claim") : null
    )
  );
}

function conflictItem(c) {
  const task = c.task || apiState.tasks.find(t => t.id === c.task_id) || {};
  const otherTask = c.other_task || apiState.tasks.find(t => t.id === c.other_task_id) || {};
  const reason = conflictReason(c, task, otherTask);
  const resolution = h("select", {}, [
    ["continue_current_owner", "Continue current owner"],
    ["transfer_ownership", "Transfer ownership"],
    ["split_scope", "Split scope"],
    ["pause_secondary_work", "Pause secondary work"],
    ["mark_duplicate", "Mark duplicate"],
    ["escalate_to_human", "Escalate to human"],
  ].map(([value, label]) => h("option", { value }, label)));
  const target = h("select", {}, [h("option", { value: "" }, "Default target actor")].concat(apiState.actors.map(a => h("option", { value: a.id }, `${a.name} · ${a.id}`))));
  const note = h("textarea", { placeholder: "Resolution note required" });
  return h("div", { class: "item conflict-card" },
    h("div", { class: "item-head" },
      h("strong", {}, c.conflict_type.split("_").join(" ")),
      h("span", { class: "pill red" }, "needs decision")
    ),
    h("p", {}, reason),
    h("div", { class: "conflict-tasks" },
      h("div", { class: "mini-card clickable", onclick: () => task.id && selectTask(task.id) },
        h("small", { class: "meta" }, "Task"),
        h("strong", {}, task.title || c.task_id || "Unknown task"),
        h("p", { class: "meta" }, `${c.task_id || ""} · owner ${actorName(task.owner_id || c.current_owner_id)}`)
      ),
      otherTask.id || c.other_task_id ? h("div", { class: "mini-card clickable", onclick: () => otherTask.id && selectTask(otherTask.id) },
        h("small", { class: "meta" }, "Conflicts with"),
        h("strong", {}, otherTask.title || c.other_task_id),
        h("p", { class: "meta" }, `${c.other_task_id || ""} · owner ${actorName(otherTask.owner_id || c.other_actor_id)}`)
      ) : h("div", { class: "mini-card" },
        h("small", { class: "meta" }, "Conflicts with"),
        h("strong", {}, actorName(c.other_actor_id || c.current_owner_id)),
        h("p", { class: "meta" }, "Same task ownership")
      )
    ),
    c.scope ? h("p", { class: "meta" }, `Overlapping scope: ${c.scope_type || "scope"} · ${c.scope}`) : null,
    canWrite() ? h("div", { class: "form" },
      resolution,
      target,
      note,
      h("button", { class: "primary", onclick: async () => {
        await api(`/api/conflicts/${c.id}/resolve`, { method: "POST", body: JSON.stringify({ resolution: resolution.value, target_actor_id: target.value, note: note.value }) });
        await refresh();
      } }, "Resolve Conflict")
    ) : null
  );
}

function conflictReason(c, task, otherTask) {
  if (c.conflict_type === "lock_overlap") {
    return `Two active work claims overlap on ${c.scope || "the same scope"}. Resolve who should continue before both agents edit the same area.`;
  }
  if (c.conflict_type === "ownership") {
    return `Another actor tried to claim a task that already has an active owner. Resolve whether to keep the current owner or transfer work.`;
  }
  if (otherTask && otherTask.id) {
    return `${task.title || "This task"} conflicts with ${otherTask.title}.`;
  }
  return "TaskPilot detected competing work that needs a human decision.";
}

function projectsView() {
  return h("div", { class: "grid2" },
    h("div", {}, createProjectForm(), h("br"), createRepoForm(), h("br"), createWorkspaceForm()),
    h("div", {},
      h("div", { class: "panel list" }, h("h2", {}, "Projects"), apiState.projects.map(p => h("div", { class: "item" }, h("strong", {}, p.name), h("p", { class: "meta" }, `${p.id} · ${p.description || "no description"}`)))),
      h("br"),
      h("div", { class: "panel list" }, h("h2", {}, "Repositories"), apiState.repositories.map(r => h("div", { class: "item" }, h("strong", {}, r.name), h("p", { class: "meta" }, `${r.id} · ${projectName(r.project_id)} · ${r.default_branch} · ${r.path || "no path"}`)))),
      h("br"),
      h("div", { class: "panel list" }, h("h2", {}, "Workspaces"), apiState.workspaces.map(w => h("div", { class: "item" }, h("strong", {}, w.name), h("p", { class: "meta" }, `${w.id} · ${projectName(w.project_id)} · ${actorName(w.actor_id)} · ${w.machine_name || "no machine"}`))))
    )
  );
}

function createProjectForm() {
  if (!canWrite()) return h("div", { class: "panel" }, h("h2", {}, "Create Project"), h("p", { class: "meta" }, "Your role is read-only."));
  const name = h("input", { placeholder: "Project name" });
  const description = h("textarea", { placeholder: "Description" });
  return h("div", { class: "panel form" }, h("h2", {}, "Create Project"), name, description,
    h("button", { class: "primary", onclick: async () => {
      await api("/api/projects", { method: "POST", body: JSON.stringify({ name: name.value, description: description.value }) });
      name.value = ""; description.value = "";
      await refresh();
    }}, "Create Project"));
}

function projectSelect(selected = "") {
  return h("select", {}, apiState.projects.map(p => h("option", { value: p.id, selected: selected === p.id }, p.name)));
}

function createRepoForm() {
  if (!canWrite()) return null;
  const project = projectSelect(apiState.selectedProject || "project_default");
  const name = h("input", { placeholder: "Repository name" });
  const path = h("input", { placeholder: "Local path or remote URL" });
  const branch = h("input", { placeholder: "Default branch", value: "main" });
  return h("div", { class: "panel form" }, h("h2", {}, "Add Repository"), project, name, path, branch,
    h("button", { onclick: async () => {
      await api("/api/repositories", { method: "POST", body: JSON.stringify({ project_id: project.value, name: name.value, path: path.value, default_branch: branch.value }) });
      name.value = ""; path.value = "";
      await refresh();
    }}, "Add Repository"));
}

function createWorkspaceForm() {
  if (!canWrite()) return null;
  const project = projectSelect(apiState.selectedProject || "project_default");
  const actor = h("select", {}, [h("option", { value: "" }, "No actor")].concat(apiState.actors.map(a => h("option", { value: a.id }, `${a.name} · ${a.id}`))));
  const name = h("input", { placeholder: "Workspace name" });
  const machine = h("input", { placeholder: "Machine name" });
  const kind = h("select", {}, ["local","agent","ci","other"].map(v => h("option", { value: v }, v)));
  return h("div", { class: "panel form" }, h("h2", {}, "Add Workspace"), project, actor, name, machine, kind,
    h("button", { onclick: async () => {
      await api("/api/workspaces", { method: "POST", body: JSON.stringify({ project_id: project.value, actor_id: actor.value, name: name.value, machine_name: machine.value, kind: kind.value }) });
      name.value = ""; machine.value = "";
      await refresh();
    }}, "Add Workspace"));
}

function adminView() {
  if (!isAdmin()) return h("div", { class: "panel" }, h("h2", {}, "Admin"), h("p", { class: "meta" }, "Admin role required."));
  return h("div", { class: "grid2" },
    h("div", {}, createUserForm(), h("br"), createKeyForm(), h("br"), changePasswordForm()),
    h("div", {},
      h("div", { class: "panel list" }, h("h2", {}, "Users"), apiState.users.map(userItem)),
      h("br"),
      h("div", { class: "panel list" }, h("h2", {}, "API Keys"), apiState.apiKeys.map(keyItem))
    )
  );
}

function createUserForm() {
  const email = h("input", { placeholder: "Email" });
  const name = h("input", { placeholder: "Name" });
  const password = h("input", { type: "password", placeholder: "Temporary password" });
  const role = h("select", {}, ["developer","maintainer","viewer","admin"].map(v => h("option", { value: v }, v)));
  return h("div", { class: "panel form" }, h("h2", {}, "Invite User"), email, name, password, role,
    h("button", { class: "primary", onclick: async () => {
      await api("/api/users", { method: "POST", body: JSON.stringify({ email: email.value, name: name.value, password: password.value, role: role.value }) });
      email.value = ""; name.value = ""; password.value = "";
      await refresh();
    }}, "Create User"));
}

function userItem(u) {
  const role = h("select", {}, ["admin","maintainer","developer","viewer"].map(v => h("option", { value: v, selected: v === u.role }, v)));
  const active = h("input", { type: "checkbox", checked: u.active });
  const newPassword = h("input", { type: "password", placeholder: "New password" });
  return h("div", { class: "item" },
    h("strong", {}, `${u.name} · ${u.email}`),
    h("p", { class: "meta" }, `${u.id} · last seen ${u.last_seen_at ? new Date(u.last_seen_at).toLocaleString() : "never"}`),
    h("div", { class: "row" }, role, h("label", { class: "check" }, active, " Active")),
    h("div", { class: "row" },
      h("button", { onclick: async () => { await api(`/api/users/${u.id}`, { method: "PATCH", body: JSON.stringify({ role: role.value, active: active.checked }) }); await refresh(); } }, "Save User"),
      h("button", { class: "danger", onclick: async () => { active.checked = false; await api(`/api/users/${u.id}`, { method: "PATCH", body: JSON.stringify({ active: false }) }); await refresh(); } }, "Deactivate")
    ),
    h("div", { class: "row" }, newPassword, h("button", { onclick: async () => { await api(`/api/users/${u.id}/password`, { method: "POST", body: JSON.stringify({ new_password: newPassword.value }) }); newPassword.value = ""; await refresh(); } }, "Reset Password"))
  );
}

function createKeyForm() {
  const name = h("input", { placeholder: "Key name" });
  const actor = h("select", {}, apiState.actors.map(a => h("option", { value: a.id }, `${a.name} · ${a.id}`)));
  const role = h("select", {}, ["agent","developer","maintainer","viewer","admin"].map(v => h("option", { value: v }, v)));
  const scopes = h("input", { placeholder: "Scopes comma separated", value: "task:read,task:write,lock:write,context:write,handoff:write" });
  const output = h("textarea", { readonly: "readonly", placeholder: "New API key appears here once" });
  return h("div", { class: "panel form" }, h("h2", {}, "Create API Key"), name, actor, role, scopes,
    h("button", { class: "primary", onclick: async () => {
      const key = await api("/api/api-keys", { method: "POST", body: JSON.stringify({
        name: name.value, actor_id: actor.value, role: role.value,
        scopes: scopes.value.split(",").map(s => s.trim()).filter(Boolean),
      }) });
      output.value = key.api_key || "";
      await refresh();
    }}, "Create Key"),
    output);
}

function keyItem(k) {
  return h("div", { class: "item" },
    h("strong", {}, `${k.name} · ${k.prefix}`),
    h("p", { class: "meta" }, `${k.id} · actor ${actorName(k.actor_id)} · ${k.role} · ${(k.scopes || []).join(", ")}`),
    k.revoked_at ? h("span", { class: "pill amber" }, `revoked ${new Date(k.revoked_at).toLocaleString()}`) :
      h("button", { class: "danger", onclick: async () => { await api(`/api/api-keys/${k.id}`, { method: "DELETE" }); await refresh(); } }, "Revoke")
  );
}

function changePasswordForm() {
  if (!apiState.principal || apiState.principal.kind !== "user") return null;
  const current = h("input", { type: "password", placeholder: "Current password" });
  const next = h("input", { type: "password", placeholder: "New password" });
  return h("div", { class: "panel form" }, h("h2", {}, "Change My Password"), current, next,
    h("button", { onclick: async () => { await api("/api/me/password", { method: "POST", body: JSON.stringify({ current_password: current.value, new_password: next.value }) }); current.value = ""; next.value = ""; apiState.error = "Password changed. Please log in again."; await logout(false); } }, "Change Password"));
}

function settings() {
  const token = h("input", { value: apiState.token, placeholder: "Team token" });
  const apiKey = h("input", { value: apiState.apiKey, placeholder: "API key" });
  const actorID = h("input", { value: apiState.actor, placeholder: "Actor ID from CLI config" });
  const actorSecret = h("input", { value: apiState.actorSecret, placeholder: "Actor secret from CLI config" });
  const identity = apiState.principal
    ? `${apiState.principal.kind} · ${apiState.principal.role} · ${apiState.principal.actor_id || apiState.principal.user_id || "session"}`
    : "Not signed in";
  return h("div", { class: "panel form" },
    h("h2", {}, "Connection"),
    h("p", { class: "meta" }, `Current identity: ${identity}`),
    h("p", { class: "meta" }, "Use a team token for local testing, an API key for an agent, or paste the CLI actor credentials so dashboard and CLI act as the same identity."),
    token,
    apiKey,
    h("button", { class: "primary", onclick: async () => {
    saveSetting("taskpilot.token", token.value);
    saveSetting("taskpilot.apiKey", apiKey.value);
    apiState.token = token.value;
    apiState.apiKey = apiKey.value;
    setLegacyEnabled(!apiKey.value && !!token.value);
    await refresh();
  }}, "Save Connection"),
    h("hr", {}),
    h("p", { class: "meta" }, "Match dashboard identity to CLI. Use values from `taskpilot config show` and the local config file."),
    actorID,
    actorSecret,
    h("button", { onclick: async () => {
      apiState.error = "";
      stopEventStream();
      apiState.apiKey = "";
      apiState.token = token.value.trim();
      apiState.actor = actorID.value.trim();
      apiState.actorSecret = actorSecret.value.trim();
      saveSetting("taskpilot.apiKey", "");
      saveSetting("taskpilot.token", apiState.token);
      saveSetting("taskpilot.actor", apiState.actor);
      saveSetting("taskpilot.actorSecret", apiState.actorSecret);
      setLegacyEnabled(true);
      try {
        await api("/api/me");
        await refresh();
      } catch (err) {
        apiState.error = `Could not use that actor identity. Check actor id, actor secret, and team token. (${err.message})`;
        render();
      }
    }}, "Use This Actor In Dashboard"),
    h("button", { onclick: async () => {
      clearActorSettings();
      await ensureLegacyActor();
      await refresh();
    }}, "Reset Dashboard Actor")
  );
}

function loginView() {
  const email = h("input", { placeholder: "Email" });
  const password = h("input", { type: "password", placeholder: "Password" });
  const apiKey = h("input", { placeholder: "Agent/API key" });
  const token = h("input", { value: apiState.token, placeholder: "Development team token" });
  const actorID = h("input", { value: apiState.actor, placeholder: "Existing actor ID, optional" });
  const actorSecret = h("input", { value: apiState.actorSecret, placeholder: "Existing actor secret, optional" });
  return h("div", { class: "login" },
    h("div", { class: "panel form login-panel" },
      h("h1", {}, "TaskPilot"),
      h("p", { class: "meta" }, "Sign in as a human, paste an agent API key, or use the development token flow."),
      email, password,
      h("button", { class: "primary", onclick: async () => {
        await apiRequest("/api/auth/login", { method: "POST", body: JSON.stringify({ email: email.value, password: password.value }) }, false);
        apiState.apiKey = "";
        setLegacyEnabled(false);
        saveSetting("taskpilot.apiKey", "");
        await refresh();
      }}, "Log In"),
      h("hr", {}),
      apiKey,
      h("button", { onclick: async () => {
        apiState.apiKey = apiKey.value;
        saveSetting("taskpilot.apiKey", apiKey.value);
        setLegacyEnabled(false);
        await refresh();
      }}, "Use API Key"),
      h("hr", {}),
      h("p", { class: "meta" }, "Docker default token: change-this-team-token-before-use. Local SQLite default token: dev-token."),
      token,
      actorID,
      actorSecret,
      h("button", { onclick: async () => {
        try {
          apiState.error = "";
          stopEventStream();
          apiState.token = token.value.trim();
          apiState.apiKey = "";
          saveSetting("taskpilot.apiKey", "");
          saveSetting("taskpilot.token", apiState.token);
          setLegacyEnabled(true);
          if (actorID.value.trim() || actorSecret.value.trim()) {
            apiState.actor = actorID.value.trim();
            apiState.actorSecret = actorSecret.value.trim();
            saveSetting("taskpilot.actor", apiState.actor);
            saveSetting("taskpilot.actorSecret", apiState.actorSecret);
            await api("/api/me");
          } else {
            await ensureLegacyActor();
          }
          await refresh();
        } catch (err) {
          setLegacyEnabled(false);
          clearActorSettings();
          apiState.principal = null;
          apiState.error = `Development token was rejected. For the Docker setup use change-this-team-token-before-use. (${err.message})`;
          render();
        }
      }}, "Use Development Token")
    )
  );
}

async function logout(callServer = true) {
  stopEventStream();
  if (callServer) {
    try { await api("/api/auth/logout", { method: "POST", body: "{}" }); } catch {}
  }
  apiState.principal = null;
  apiState.apiKey = "";
  setLegacyEnabled(false);
  saveSetting("taskpilot.apiKey", "");
  render();
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
    if (!apiState.principal) {
      root.append(loginView());
      if (apiState.error) root.append(h("div", { class: "toast error" }, apiState.error));
      return;
    }
    const tabs = ["board", "detail", "projects", "conflicts", "actors", "handoffs", "settings"];
    if (isAdmin()) tabs.splice(5, 0, "admin");
    const content = apiState.tab === "board" ? board()
      : apiState.tab === "detail" ? detailView()
      : apiState.tab === "projects" ? projectsView()
      : apiState.tab === "conflicts" ? conflictsView()
      : apiState.tab === "actors" ? actorsView()
      : apiState.tab === "handoffs" ? handoffsView()
      : apiState.tab === "admin" ? adminView()
      : settings();
    root.append(h("div", { class: "shell" },
      h("div", { class: "topbar" },
        h("div", { class: "brand" }, "TaskPilot"),
        h("div", { class: "tabs" }, tabs.map(t => h("button", { class: apiState.tab === t ? "active" : "", onclick: () => { apiState.tab = t; render(); } }, t))),
        h("div", { class: "identity" },
          h("span", {}, `${apiState.principal.kind} · ${apiState.principal.role}`),
          h("button", { onclick: () => logout(true) }, "Log Out")
        )
      ),
      h("main", { class: "main" }, apiState.error ? h("div", { class: "panel error" }, apiState.error) : null, content)
    ));
    if (apiState.handoffModal) root.append(handoffModalView());
  } catch (err) {
    document.body.innerHTML = `<div style="font:14px system-ui;padding:24px"><h1>TaskPilot dashboard error</h1><p>${String(err.message || err)}</p></div>`;
  }
}

render();
refresh();
document.addEventListener("focusout", () => {
  if (!apiState.pendingRender) return;
  setTimeout(() => {
    if (!isFormEditing() && apiState.pendingRender) renderWhenSafe();
  }, 50);
});
setInterval(() => {
  if (apiState.principal && !apiState.streamActive) refresh();
}, 5000);
