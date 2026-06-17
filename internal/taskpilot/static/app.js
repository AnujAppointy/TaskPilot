const apiState = {
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
  selectedScopeType: loadSetting("taskpilot.scopeType", "project"),
  selectedRepo: loadSetting("taskpilot.repo", ""),
  selectedWorkspace: loadSetting("taskpilot.workspace", ""),
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
  memoryMode: "auto",
  handoffModal: null,
  actorSetupCommands: {},
};

const statuses = ["ready", "claimed", "in_progress", "blocked", "handoff_ready", "in_review", "completed"];
try {
  localStorage.removeItem("taskpilot.theme");
} catch {
  // Ignore storage failures; TaskPilot now uses the light theme only.
}

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

function canWrite() {
  return !!apiState.principal;
}

function currentUserID() {
  return apiState.principal && (apiState.principal.user_id || apiState.principal.id || "");
}

function identityLabel() {
  if (!apiState.principal) return "Not signed in";
  if (apiState.principal.kind === "user") return apiState.principal.name || apiState.principal.email || "TaskPilot user";
  if (apiState.principal.kind === "api_key") return "Agent API key";
  if (apiState.principal.kind === "legacy_actor") return "Development actor";
  return apiState.principal.kind;
}

function actorOwnershipLabel(actor) {
  if (actor.created_by_user_id && actor.created_by_user_id === currentUserID()) return "Yours";
  if (actor.created_by_user_id) return "Team actor";
  return "Legacy actor";
}

function authHeaders(includeActor = true) {
  const headers = { "Content-Type": "application/json" };
  if (includeActor && apiState.actor && apiState.actorSecret) {
    headers["X-Actor-ID"] = apiState.actor;
    headers["X-Actor-Secret"] = apiState.actorSecret;
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
    const calls = [
      api(taskListPath()),
      api("/api/actors"),
      api("/api/projects"),
      api("/api/repositories"),
      api("/api/workspaces"),
      api("/api/handoffs"),
      api("/api/conflicts?status=open"),
      api("/api/conflicts/stale-claims"),
      api("/api/events"),
    ];
    const [tasks, actors, projects, repositories, workspaces, handoffs, conflicts, staleClaims, events] = await Promise.all(calls);
    apiState.tasks = Array.isArray(tasks) ? tasks : [];
    apiState.actors = Array.isArray(actors) ? actors : [];
    apiState.projects = Array.isArray(projects) ? projects : [];
    apiState.repositories = Array.isArray(repositories) ? repositories : [];
    apiState.workspaces = Array.isArray(workspaces) ? workspaces : [];
    if (!apiState.selectedProject && apiState.projects.length) {
      apiState.selectedProject = "project_default";
      saveSetting("taskpilot.project", apiState.selectedProject);
    }
    if (!apiState.selectedRepo && apiState.repositories.length) {
      apiState.selectedRepo = apiState.repositories[0].id;
      saveSetting("taskpilot.repo", apiState.selectedRepo);
    }
    if (!apiState.selectedWorkspace && apiState.workspaces.length) {
      apiState.selectedWorkspace = apiState.workspaces[0].id;
      saveSetting("taskpilot.workspace", apiState.selectedWorkspace);
    }
    apiState.handoffs = Array.isArray(handoffs) ? handoffs : [];
    apiState.conflicts = Array.isArray(conflicts) ? conflicts : [];
    apiState.staleClaims = Array.isArray(staleClaims) ? staleClaims : [];
    apiState.events = Array.isArray(events) ? events : [];
    apiState.lastEventID = apiState.events.reduce((max, e) => Math.max(max, e.id || 0), apiState.lastEventID || 0);
    apiState.users = [];
    if (apiState.selected) apiState.detail = await api(`/api/tasks/${apiState.selected}`);
    ensureEventStream();
  } catch (err) {
    apiState.error = err.message;
    stopEventStream();
  }
  renderWhenSafe();
}

function taskListPath() {
  const scope = apiState.selectedScopeType || "project";
  if (scope === "repo" && apiState.selectedRepo) {
    return `/api/tasks?repo_id=${encodeURIComponent(apiState.selectedRepo)}`;
  }
  if (scope === "workspace" && apiState.selectedWorkspace) {
    return `/api/tasks?workspace_id=${encodeURIComponent(apiState.selectedWorkspace)}`;
  }
  if (apiState.selectedProject) {
    return `/api/tasks?project_id=${encodeURIComponent(apiState.selectedProject)}`;
  }
  return "/api/tasks";
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

function boardScopeFilter() {
  const scope = h("select", {}, [
    h("option", { value: "project", selected: apiState.selectedScopeType === "project" }, "Projects"),
    h("option", { value: "repo", selected: apiState.selectedScopeType === "repo" }, "Repositories"),
    h("option", { value: "workspace", selected: apiState.selectedScopeType === "workspace" }, "Workspaces"),
  ]);
  const project = h("select", {}, [
    h("option", { value: "", selected: apiState.selectedProject === "" }, "All projects"),
    ...apiState.projects.map(p => h("option", { value: p.id, selected: apiState.selectedProject === p.id }, p.name)),
  ]);
  const repo = h("select", {}, apiState.repositories.map(r => h("option", { value: r.id, selected: apiState.selectedRepo === r.id }, r.name)));
  const workspace = h("select", {}, apiState.workspaces.map(w => h("option", { value: w.id, selected: apiState.selectedWorkspace === w.id }, w.name)));
  const activePicker = apiState.selectedScopeType === "repo" ? repo : apiState.selectedScopeType === "workspace" ? workspace : project;
  return h("div", { class: "toolbar" },
    h("label", {}, "Board scope"),
    scope,
    activePicker,
    h("button", { onclick: async () => {
      apiState.selectedScopeType = scope.value;
      apiState.selectedProject = project.value;
      apiState.selectedRepo = repo.value;
      apiState.selectedWorkspace = workspace.value;
      saveSetting("taskpilot.scopeType", apiState.selectedScopeType);
      saveSetting("taskpilot.project", apiState.selectedProject);
      saveSetting("taskpilot.repo", apiState.selectedRepo);
      saveSetting("taskpilot.workspace", apiState.selectedWorkspace);
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

function navIcon(tab) {
  const icons = {
    board: "dashboard",
    detail: "schedule",
    projects: "inventory_2",
    conflicts: "report",
    actors: "groups",
    handoffs: "call_split",
    settings: "settings",
  };
  return icons[tab] || "•";
}

function board() {
  const tasks = filteredTasks();
  return h("div", { class: "board-page" },
    h("div", { class: "page-heading" },
      h("div", {},
        h("p", { class: "eyebrow" }, "Workspace"),
        h("h1", {}, "Main Kanban Board"),
        h("p", { class: "meta" }, "Coordinate humans and agents across claims, locks, handoffs, and task memory.")
      ),
      canWrite() ? h("button", { class: "primary", onclick: () => { apiState.selected = null; apiState.detail = null; apiState.tab = "detail"; render(); } }, "+ New Task") : null
    ),
    boardScopeFilter(),
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
  return h("div", { class: `card task-card ${t.status === "completed" ? "completed" : ""}`, onclick: () => selectTask(t.id) },
    h("div", { class: "card-title" }, t.title),
    h("div", { class: "meta" },
      h("span", {}, `Owner: ${actorName(t.owner_id)}`),
      t.repo_id ? h("span", {}, `Repo: ${repoName(t.repo_id)}`) : null,
      apiState.selectedScopeType !== "repo" ? h("span", {}, `Project: ${projectName(t.project_id)}`) : null,
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
  apiState.memoryMode = "auto";
  apiState.memoryError = "";
  apiState.detail = await api(`/api/tasks/${id}`);
  render();
}

function createTaskForm() {
  if (!canWrite()) return null;
  const title = h("input", { placeholder: "Title" });
  const goal = h("textarea", { placeholder: "Goal" });
  const scope = h("input", { placeholder: "Scope, comma separated" });
  const project = h("select", {}, apiState.projects.map(p => h("option", { value: p.id, selected: (apiState.selectedProject || "project_default") === p.id }, p.name)));
  const repo = h("select", {}, [h("option", { value: "" }, "No repo")].concat(apiState.repositories.map(r => h("option", { value: r.id, selected: apiState.selectedScopeType === "repo" && apiState.selectedRepo === r.id }, r.name))));
  const workspace = h("select", {}, [h("option", { value: "" }, "No workspace")].concat(apiState.workspaces.map(w => h("option", { value: w.id, selected: apiState.selectedScopeType === "workspace" && apiState.selectedWorkspace === w.id }, w.name))));
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
      section("Timeline", h("div", { class: "timeline log-panel" }, events.map(e => h("div", { class: "event log-line" }, `${e.id} · ${e.event_type} · ${new Date(e.created_at).toLocaleString()}`))))
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
  const forcedHandoff = apiState.memoryMode === "handoff";
  const forcedSnapshot = apiState.memoryMode === "snapshot";
  const packetMarkdown = !forcedSnapshot && (forcedHandoff || !preferSnapshot) && handoffPacket && handoffPacket.markdown ? handoffPacket.markdown : "";
  const snapshotMarkdown = latestSnapshot && latestSnapshot.markdown ? latestSnapshot.markdown : "";
  const markdown = packetMarkdown || (!forcedHandoff ? snapshotMarkdown : "") || "# Task Memory\n\nNo handoff packet or context snapshot has been generated yet.\n";
  const activeDoc = packetMarkdown ? "handoff" : snapshotMarkdown && !forcedHandoff ? "snapshot" : "none";
  const editor = h("textarea", { class: "memory-editor", value: markdown });
  const sourceLabel = handoffPacket && handoffPacket.source ? handoffPacket.source.replaceAll("_", " ") : "";
  const source = packetMarkdown ? `Recent handoff packet ${handoffPacket.status || "draft"} v${handoffPacket.version || 1}${sourceLabel ? ` · ${sourceLabel}` : ""} · ${new Date(handoffPacket.updated_at).toLocaleString()}` : snapshotMarkdown && !forcedHandoff ? `Latest snapshot · ${new Date(latestSnapshot.updated_at).toLocaleString()}${preferSnapshot ? " · newer than weak draft" : ""}` : forcedHandoff ? "No recent handoff packet yet. Generate or prepare a handoff first." : "No memory document yet";
  const showRecentHandoff = async () => {
    apiState.memoryError = "";
    apiState.memoryMode = "handoff";
    try {
      const shouldRefreshDraft = !handoffPacket || handoffPacketShouldRefreshFromMemory(handoffPacket, latestSnapshot);
      if (shouldRefreshDraft) {
        await api(`/api/tasks/${task.id}/handoff-packet/generate`, { method: "POST", body: JSON.stringify({ status: "draft" }) });
      }
      await refresh();
    } catch (err) {
      apiState.memoryError = err.message;
      render();
    }
  };
  const saveMarkdown = async () => {
    apiState.memoryError = "";
    try {
      if (activeDoc === "handoff" && handoffPacket) {
        await api(`/api/handoff-packets/${handoffPacket.id}`, { method: "PATCH", body: JSON.stringify({ markdown: editor.value }) });
      } else if (activeDoc === "snapshot" && latestSnapshot) {
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
        h("button", { onclick: async () => { apiState.memoryMode = "snapshot"; await api(`/api/tasks/${task.id}/snapshots`, { method: "POST", body: JSON.stringify({ snapshot_type: "manual" }) }); await refresh(); } }, "Generate Snapshot"),
        h("button", { onclick: showRecentHandoff }, "Show Recent Handoff"),
        apiState.memoryMode !== "auto" ? h("button", { onclick: () => { apiState.memoryMode = "auto"; apiState.memoryError = ""; render(); } }, "Show Best Memory") : null,
        h("button", { onclick: () => openHandoffModal(task, latestSnapshot, handoffPacket) }, "Prepare handoff for other agent"),
        h("button", { class: "danger", onclick: async () => {
          if (!confirm("Delete task memory? This removes context entries, snapshots, and handoff memory, but keeps the task.")) return;
          await api(`/api/tasks/${task.id}/memory`, { method: "DELETE" });
          apiState.memoryMode = "auto";
          await refresh();
        } }, "Delete Memory")
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
        activeDoc !== "none" ? h("button", { class: "primary", onclick: saveMarkdown }, activeDoc === "handoff" ? "Save Handoff Draft" : "Save Snapshot") : null
      )
    ) : null,
    snapshotItems.length ? h("details", {}, h("summary", {}, "Recent snapshots"), h("div", { class: "snapshot-list" }, snapshotItems)) : null
  );
}

function handoffPacketShouldRefreshFromMemory(packet, latestSnapshot) {
  if (!packet) return true;
  if (packet.source && packet.source !== "generated_fallback") return false;
  if (handoffPacketHasUsefulTransferContent(packet)) return false;
  if (!latestSnapshot || !latestSnapshot.markdown) return true;
  const snapshotTime = new Date(latestSnapshot.updated_at || latestSnapshot.created_at || 0).getTime();
  const packetTime = new Date(packet.updated_at || packet.created_at || 0).getTime();
  return snapshotTime >= packetTime || handoffPacketLooksEmpty(packet);
}

function handoffPacketHasUsefulTransferContent(packet) {
  const p = packet && packet.packet ? packet.packet : {};
  return usefulArray(p.completed_work).length > 0 ||
    usefulArray(p.important_decisions).length > 0 ||
    usefulArray(p.files_components_affected).length > 0 ||
    usefulArray(p.handoff_timeline).length > 0 ||
    usefulText(p.current_state) ||
    usefulText(p.handoff_message);
}

function handoffPacketLooksEmpty(packet) {
  const text = `${packet && packet.markdown ? packet.markdown : ""}`.toLowerCase();
  if (!text) return true;
  const noneCount = (text.match(/none recorded/g) || []).length;
  return noneCount >= 6 && !handoffPacketHasUsefulTransferContent(packet);
}

function usefulArray(values) {
  return Array.isArray(values) ? values.filter(v => usefulText(v)) : [];
}

function usefulText(value) {
  value = `${value || ""}`.trim();
  if (!value || value === "Not specified." || value === "None recorded.") return false;
  return !/^(task is .*continue from the latest task memory|continue from the latest task context|not specified)$/i.test(value);
}

function linesFromTextarea(value) {
  return String(value || "").split("\n").map(s => s.trim()).filter(Boolean);
}

function csvFromInput(value) {
  return String(value || "").split(",").map(s => s.trim()).filter(Boolean);
}

function editTaskForm(task) {
  const title = h("input", { value: task.title || "", placeholder: "Task title" });
  const goal = h("textarea", { value: task.goal || "", placeholder: "Task goal" });
  const type = h("select", {}, ["planning","research","implementation","review","debugging","documentation","other"].map(v => h("option", { value: v, selected: v === task.type }, v)));
  const priority = h("select", {}, ["low","normal","high","urgent"].map(v => h("option", { value: v, selected: v === task.priority }, v)));
  const scope = h("textarea", { value: (task.scope || []).join("\n"), placeholder: "Scope, one item per line" });
  const requirements = h("textarea", { value: (task.requirements || []).join("\n"), placeholder: "Requirements, one per line" });
  const criteria = h("textarea", { value: (task.completion_criteria || []).join("\n"), placeholder: "Completion criteria, one per line" });
  const risks = h("textarea", { value: (task.risks || []).join("\n"), placeholder: "Risks, one per line" });
  const blockers = h("textarea", { value: (task.blockers || []).join("\n"), placeholder: "Blockers, one per line" });
  return h("details", { class: "edit-task" },
    h("summary", {}, "Edit Task"),
    h("div", { class: "form" },
      title,
      goal,
      h("div", { class: "row" }, type, priority),
      scope,
      requirements,
      criteria,
      risks,
      blockers,
      h("button", { class: "primary", onclick: async () => {
        await api(`/api/tasks/${task.id}`, { method: "PATCH", body: JSON.stringify({
          title: title.value,
          goal: goal.value,
          type: type.value,
          priority: priority.value,
          scope: linesFromTextarea(scope.value),
          requirements: linesFromTextarea(requirements.value),
          completion_criteria: linesFromTextarea(criteria.value),
          risks: linesFromTextarea(risks.value),
          blockers: linesFromTextarea(blockers.value),
          reason: "Edited from dashboard.",
        }) });
        await refresh();
      } }, "Save Task")
    )
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
  if (!canWrite()) return h("div", { class: "panel" }, h("h2", {}, "Actions"), h("p", { class: "meta" }, "Sign in to make task changes."));
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
      editTaskForm(task),
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
      h("div", { class: "row" }, lockScope, h("button", { onclick: async () => { try { await api(`/api/tasks/${task.id}/locks`, { method: "POST", body: JSON.stringify({ scope: lockScope.value, scope_type: "file_glob" }) }); lockScope.value = ""; } finally { await refresh(); } } }, "Acquire Lock")),
      h("h3", {}, "Danger Zone"),
      h("button", { class: "danger", onclick: async () => {
        if (!confirm(`Delete task "${task.title}" permanently? This removes the task and its related database records.`)) return;
        await api(`/api/tasks/${task.id}`, { method: "DELETE" });
        apiState.selected = null;
        apiState.detail = null;
        apiState.tab = "board";
        await refresh();
      } }, "Delete Task")
    )
  );
}

function actorsView() {
  const mine = apiState.actors.filter(a => a.created_by_user_id && a.created_by_user_id === currentUserID());
  return h("div", { class: "grid2" },
    myActorsPanel(mine),
    h("div", { class: "panel list" },
      h("h2", {}, "All Actors"),
      h("p", { class: "meta" }, "Team-visible human and agent identities currently registered in TaskPilot."),
      apiState.actors.length ? apiState.actors.map(a => h("div", { class: "item" },
        h("div", { class: "item-head" },
          h("strong", {}, a.name),
          h("span", { class: "pill" }, actorOwnershipLabel(a))
        ),
        h("p", {}, `${a.kind} · ${a.machine_name || "no machine"} · ${a.id}`)
      )) : h("p", { class: "meta" }, "No actors yet.")
    )
  );
}

function myActorsPanel(mine) {
  if (!canWrite()) return h("div", { class: "panel" }, h("h2", {}, "My Actors"), h("p", { class: "meta" }, "This account is read-only."));
  const name = h("input", { placeholder: "Name" });
  const machine = h("input", { placeholder: "Machine or tool, optional" });
  const kind = h("select", {}, h("option", { value: "agent" }, "Agent"), h("option", { value: "human" }, "Manual developer"));
  return h("div", { class: "panel form" },
    h("h2", {}, "My Actors"),
    h("p", { class: "meta" }, "Create one actor per agent or work mode. Recommended names: yourname_agentname, for example anuj_codex or priya_gemini."),
    h("div", { class: "row" }, name, kind, machine),
    h("button", { class: "primary", onclick: async () => {
      await api("/api/actors/register", { method: "POST", body: JSON.stringify({ name: name.value, kind: kind.value, machine_name: machine.value }) });
      name.value = "";
      machine.value = "";
      await refresh();
    }}, "Add Actor"),
    h("div", { class: "list" },
      mine.length ? mine.map(myActorItem) : h("p", { class: "meta" }, "Your default actor will appear here after signup. Add more actors for Gemini, Claude, Codex, or manual developer mode.")
    )
  );
}

function myActorItem(actor) {
  const name = h("input", { value: actor.name, placeholder: "Actor name" });
  const machine = h("input", { value: actor.machine_name || "", placeholder: "Machine or tool" });
  const existingSetup = apiState.actorSetupCommands[actor.id] || "";
  const setup = h("textarea", { readonly: "readonly", style: `${existingSetup ? "" : "display:none;"}min-height:110px;`, value: existingSetup });
  const kind = h("select", {},
    h("option", { value: "agent", selected: actor.kind === "agent" }, "Agent"),
    h("option", { value: "human", selected: actor.kind === "human" }, "Manual developer")
  );
  return h("div", { class: "item" },
    h("div", { class: "item-head" },
      h("strong", {}, actor.name),
      h("span", { class: "pill amber" }, actor.kind)
    ),
    h("p", { class: "meta" }, actor.id),
    h("div", { class: "row" }, name, kind, machine),
    h("div", { class: "button-row" },
      h("button", { onclick: async () => {
        await api(`/api/actors/${actor.id}`, { method: "PATCH", body: JSON.stringify({ name: name.value, kind: kind.value, machine_name: machine.value }) });
        await refresh();
      }}, "Save"),
      h("button", { onclick: async () => {
        const fresh = await api(`/api/actors/${actor.id}/secret`, { method: "POST", body: "{}" });
        const server = window.location.origin;
        const email = apiState.principal.email || "";
        apiState.actorSetupCommands[actor.id] = [
          `taskpilot login --server ${server}${email ? ` --email ${email}` : ""}`,
          `taskpilot config set-actor ${fresh.id} ${fresh.actor_secret}`,
          "taskpilot config show",
        ].join("\n");
        setup.value = apiState.actorSetupCommands[actor.id];
        setup.style.display = "block";
      }}, "Generate CLI Setup"),
      h("button", { class: "danger", onclick: async () => {
        if (!confirm(`Delete actor "${actor.name}"?`)) return;
        await api(`/api/actors/${actor.id}`, { method: "DELETE" });
        await refresh();
      }}, "Delete")
    ),
    setup,
    h("p", { class: "meta" }, "The CLI secret is shown only after generation. Generating a new one replaces the previous actor secret.")
  );
}

function cliSetupItem(actor) {
  const existingSetup = apiState.actorSetupCommands[actor.id] || "";
  const setup = h("textarea", { readonly: "readonly", style: `${existingSetup ? "" : "display:none;"}min-height:110px;`, value: existingSetup });
  return h("div", { class: "item" },
    h("div", { class: "item-head" },
      h("strong", {}, actor.name),
      h("span", { class: "pill" }, actor.kind)
    ),
    h("p", { class: "meta" }, `Actor ID: ${actor.id}`),
    h("button", { onclick: async () => {
      const fresh = await api(`/api/actors/${actor.id}/secret`, { method: "POST", body: "{}" });
      const server = window.location.origin;
      const email = apiState.principal.email || "";
      apiState.actorSetupCommands[actor.id] = [
        `taskpilot login --server ${server}${email ? ` --email ${email}` : ""}`,
        `taskpilot config set-actor ${fresh.id} ${fresh.actor_secret}`,
        "taskpilot config show",
      ].join("\n");
      setup.value = apiState.actorSetupCommands[actor.id];
      setup.style.display = "block";
    }}, "Generate CLI Secret"),
    setup
  );
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
  if (!canWrite()) return h("div", { class: "panel" }, h("h2", {}, "Create Project"), h("p", { class: "meta" }, "Sign in to create projects."));
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

function changePasswordForm() {
  if (!apiState.principal || apiState.principal.kind !== "user") return null;
  const current = h("input", { type: "password", placeholder: "Current password" });
  const next = h("input", { type: "password", placeholder: "New password" });
  return h("div", { class: "panel form" }, h("h2", {}, "Change My Password"), current, next,
    h("button", { onclick: async () => { await api("/api/me/password", { method: "POST", body: JSON.stringify({ current_password: current.value, new_password: next.value }) }); current.value = ""; next.value = ""; apiState.error = "Password changed. Please log in again."; await logout(false); } }, "Change Password"));
}

function settings() {
  const mine = apiState.actors.filter(a => a.created_by_user_id && a.created_by_user_id === currentUserID());
  return h("div", { class: "grid2" },
    h("div", { class: "panel form" },
      h("h2", {}, "Account"),
      h("p", { class: "meta" }, `Signed in as ${identityLabel()}. Manage Gemini, Claude, Codex, and manual identities from the Actors page.`)
    ),
    changePasswordForm(),
    h("div", { class: "panel list" },
      h("h2", {}, "My CLI Actor Setup"),
      h("p", { class: "meta" }, "Only your own actors are shown here. Generate a fresh secret, then paste the command into the machine where that agent runs."),
      mine.length ? mine.map(cliSetupItem) : h("p", { class: "meta" }, "No owned actors yet. Add one from the Actors page.")
    )
  );
}

function loginView() {
  const email = h("input", { placeholder: "Email" });
  const password = h("input", { type: "password", placeholder: "Password" });
  async function finishPasswordAuth(path) {
    try {
      const res = await apiRequest(path, { method: "POST", body: JSON.stringify({ email: email.value, password: password.value }) }, false);
      clearActorSettings();
      apiState.error = res.actor_recommendation || "";
      await refresh();
    } catch (err) {
      apiState.error = err.message || "Could not authenticate";
      render();
    }
  }
  return h("div", { class: "login" },
    h("div", { class: "panel form login-panel" },
      h("h1", {}, "TaskPilot"),
      h("p", { class: "meta" }, "Sign up or log in with email and password. TaskPilot creates your first actor automatically."),
      email, password,
      h("div", { class: "button-row" },
        h("button", { class: "primary", onclick: async () => finishPasswordAuth("/api/auth/login") }, "Log In"),
        h("button", { onclick: async () => finishPasswordAuth("/api/auth/signup") }, "Sign Up")
      ),
      h("p", { class: "meta" }, "You can add, rename, or delete agent identities later from the Actors page.")
    )
  );
}

async function logout(callServer = true) {
  stopEventStream();
  if (callServer) {
    try { await api("/api/auth/logout", { method: "POST", body: "{}" }); } catch {}
  }
  apiState.principal = null;
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
    const content = apiState.tab === "board" ? board()
      : apiState.tab === "detail" ? detailView()
      : apiState.tab === "projects" ? projectsView()
      : apiState.tab === "conflicts" ? conflictsView()
      : apiState.tab === "actors" ? actorsView()
      : apiState.tab === "handoffs" ? handoffsView()
      : settings();
    root.append(h("div", { class: "shell" },
      h("div", { class: "topbar" },
        h("div", { class: "brand" }, "TaskPilot"),
        h("div", { class: "tabs" }, tabs.map(t => h("button", { class: apiState.tab === t ? "active" : "", onclick: () => { apiState.tab = t; render(); } },
          h("span", { class: "nav-icon" }, navIcon(t)),
          h("span", {}, t)
        ))),
        h("div", { class: "identity" },
          h("span", {}, identityLabel()),
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
