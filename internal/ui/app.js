"use strict";

// The page reads five lists from attentiond and draws them. Everything it
// shows comes from those responses; nothing is cached in the browser.

const REFRESH_MS = 5000;

// The complete tone vocabulary. Colour alone fails for anyone who cannot
// separate red from green, so every tone except neutral also carries a glyph.
const GLYPHS = { ready: ">", attention: "!", failed: "x", active: "~", done: ".", neutral: "" };

// Results a job can end with, and the tone each one reads in.
const RESULT_TONES = {
  failed: "failed",
  "needs-human": "attention",
  "needs-conflicts": "attention",
};

// Failures of the last click, by item and action. A refresh rebuilds the
// rows, so the message has to live outside them.
const actionErrors = new Map();
const pendingActions = new Set();

let refreshTimer = 0;
let refreshing = false;
let refreshAgain = false;

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function ago(iso) {
  const seconds = Math.round((Date.now() - new Date(iso).getTime()) / 1000);
  if (Number.isNaN(seconds)) return "";
  if (seconds < 10) return "just now";
  if (seconds < 60) return seconds + "s ago";
  if (seconds < 3600) return Math.floor(seconds / 60) + "m ago";
  if (seconds < 86400) return Math.floor(seconds / 3600) + "h ago";
  return Math.floor(seconds / 86400) + "d ago";
}

function timeNode(iso, prefix) {
  const node = el("time", "when", (prefix ? prefix + " " : "") + ago(iso));
  node.dateTime = iso;
  node.title = new Date(iso).toLocaleString();
  return node;
}

function toneOf(tone) {
  return tone in GLYPHS ? tone : "neutral";
}

function labelNode(label, tone) {
  tone = toneOf(tone);
  const node = el("span", "label tone-" + tone);
  const glyph = GLYPHS[tone];
  if (glyph) node.append(el("span", "glyph", glyph), " ");
  node.append(label);
  return node;
}

function titleNode(item) {
  const node = el("span", "title");
  if (item.snoozed) {
    const mark = el("span", "mark", "zz");
    mark.title = item.snoozed_until
      ? "Snoozed until " + new Date(item.snoozed_until).toLocaleString()
      : "Snoozed until something changes";
    node.append(mark, " ");
  }
  if (item.bumped) {
    const mark = el("span", "mark", "^");
    mark.title = "Bumped";
    node.append(mark, " ");
  }
  node.append(item.title);
  return node;
}

function isWebURL(href) {
  try {
    const url = new URL(href);
    return url.protocol === "http:" || url.protocol === "https:";
  } catch {
    return false;
  }
}

// runAction posts to the page's own origin whatever host the href names. A
// --public-url that differs from the address the page was loaded from would
// otherwise be a cross-origin request, which attentiond never answers. The
// custom header is what attentiond requires of a request that changes state.
async function runAction(item, action) {
  const key = item.id + "\n" + action.id;
  const url = new URL(action.href);
  pendingActions.add(key);
  actionErrors.delete(key);
  render();
  try {
    const response = await fetch(url.pathname + url.search, {
      method: action.method,
      headers: { "X-Attentiond": "1" },
    });
    if (!response.ok) {
      let message = "HTTP " + response.status;
      try {
        const body = await response.json();
        if (body && body.error) message = body.error;
      } catch {
        // The status is all there is to say.
      }
      actionErrors.set(key, message);
    }
  } catch (error) {
    actionErrors.set(key, String(error.message || error));
  } finally {
    pendingActions.delete(key);
  }
  await refresh();
}

function actionNode(item, action) {
  const key = item.id + "\n" + action.id;
  const node = el("span", "action");

  if (action.method === "GET") {
    if (isWebURL(action.href)) {
      const link = el("a", "", action.label);
      link.href = action.href;
      link.target = "_blank";
      link.rel = "noopener noreferrer";
      node.append(link);
    }
    return node;
  }

  const button = el("button", "", action.label);
  button.type = "button";
  button.dataset.href = action.href;
  button.disabled = pendingActions.has(key);
  button.addEventListener("click", () => runAction(item, action));
  node.append(button);

  const message = actionErrors.get(key);
  if (message) node.append(el("span", "action-error", message));
  return node;
}

function jobNode(job) {
  const node = el("div", "job");
  node.append(el("span", "job-tool", job.tool));

  if (job.status === "finished") {
    const result = job.result || "finished";
    node.append(el("span", "job-result tone-" + (RESULT_TONES[result] || "done"), result));
    if (job.detail) node.append(el("span", "job-detail", job.detail));
    if (job.finished_at) node.append(timeNode(job.finished_at, "finished"));
  } else if (job.status === "running") {
    node.append(el("span", "job-status", "running"));
    node.append(timeNode(job.started_at || job.queued_at, "started"));
  } else {
    node.append(el("span", "job-status", "queued"));
    node.append(timeNode(job.queued_at, "queued"));
  }

  if (job.log) {
    node.append(el("span", "job-log-name", "log"), el("code", "job-log", job.log));
  }
  return node;
}

function itemNode(item, options) {
  const node = el("li", "item");
  node.dataset.id = item.id;
  if (item.tone === "ready" && !options.muted) node.classList.add("ready");
  if (item.snoozed) node.classList.add("snoozed");
  if (options.muted) node.classList.add("muted");

  const row = el("div", "row");
  row.append(labelNode(item.label, options.muted ? "neutral" : item.tone));
  row.append(titleNode(item));
  if (item.updated_at) row.append(timeNode(item.updated_at));
  node.append(row);

  const meta = el("div", "meta");
  meta.append(el("span", "source", item.source));
  if (item.watched) {
    const mark = el("span", "mark watched", "Watched");
    mark.title = "attentiond acts on this pull request";
    meta.append(mark);
  }
  for (const action of item.actions || []) meta.append(actionNode(item, action));
  node.append(meta);

  if (options.job && item.job) node.append(jobNode(item.job));
  return node;
}

// setList swaps the rows only when they changed, so a refresh that found
// nothing new does not move a button out from under the pointer.
function setList(section, items, options) {
  const list = section.querySelector("ul.items");
  const next = document.createElement("ul");
  for (const item of items) next.append(itemNode(item, options || {}));
  if (next.innerHTML !== list.innerHTML) list.replaceChildren(...next.childNodes);

  section.querySelector(".empty").hidden = items.length > 0;
  const count = section.querySelector(".count");
  count.textContent = items.length > 0 ? "(" + items.length + ")" : "";
}

function renderWarnings(lists) {
  const seen = new Set();
  for (const list of lists) for (const warning of (list && list.warnings) || []) seen.add(warning);
  const box = document.getElementById("warnings");
  box.replaceChildren(...[...seen].map((warning) => el("p", "warning", warning)));
}

let latest = null;

function render() {
  if (!latest) return;
  const { attention, work, stale, jobs } = latest;
  const byId = (id) => document.getElementById(id);

  setList(byId("needs-me"), attention.items);

  // /api/jobs is a 404 on an attentiond that predates agent tools.
  byId("working").hidden = !jobs;
  byId("finished").hidden = !jobs;
  if (jobs) {
    const withJob = jobs.items.filter((item) => item.job);
    const running = withJob.filter((item) => ["queued", "running"].includes(item.job.status));
    const finished = withJob.filter((item) => item.job.status === "finished");
    setList(byId("working"), running, { job: true });
    setList(byId("finished"), finished, { job: true });
  }

  // stale_count is what this list is not showing. A board that looks
  // complete while holding work back is worse than one that admits it.
  const note = byId("stale-note");
  note.hidden = !work.stale_count;
  note.textContent = work.stale_count + " stale, below";
  setList(byId("work"), work.items);

  setList(byId("stale"), stale.items, { muted: true });
  renderWarnings([attention, work, stale, jobs]);
}

async function getJSON(path) {
  const response = await fetch(path, { cache: "no-store" });
  if (!response.ok) {
    const error = new Error(path + ": HTTP " + response.status);
    error.status = response.status;
    throw error;
  }
  return response.json();
}

async function load() {
  const jobs = getJSON("/api/jobs").catch((error) => {
    if (error.status === 404) return null;
    throw error;
  });
  const [attention, work, stale, jobsBody] = await Promise.all([
    getJSON("/api/attention"),
    getJSON("/api/work"),
    getJSON("/api/stale"),
    jobs,
  ]);
  return { attention, work, stale, jobs: jobsBody };
}

function setStatus(text, failed) {
  const status = document.getElementById("status");
  status.textContent = text;
  status.classList.toggle("error", failed);
}

function schedule() {
  clearTimeout(refreshTimer);
  if (!document.hidden) refreshTimer = setTimeout(refresh, REFRESH_MS);
}

async function refresh() {
  if (refreshing) {
    refreshAgain = true;
    return;
  }
  refreshing = true;
  try {
    latest = await load();
    render();
    setStatus("Updated " + new Date().toLocaleTimeString(), false);
  } catch (error) {
    setStatus("attentiond is not answering: " + error.message, true);
  } finally {
    refreshing = false;
    schedule();
    if (refreshAgain) {
      refreshAgain = false;
      refresh();
    }
  }
}

// A hidden tab stops polling and catches up the moment it is shown again.
document.addEventListener("visibilitychange", () => {
  if (document.hidden) clearTimeout(refreshTimer);
  else refresh();
});

refresh();
