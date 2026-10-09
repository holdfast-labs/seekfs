const { normalizeLiveQuery } = window.SeekfsQuery;
const { debounce, formatSize, formatDate, escapeHtml, sortSupported } = window.SeekfsUtil;

const state = {
  seq: 0,
  rows: [],
  selectedSet: new Set(),
  anchor: -1,
  lastClicked: -1,
  sort: "",
  lastQuery: "",
};

const els = {
  query: document.getElementById("query"),
  results: document.getElementById("results"),
  empty: document.getElementById("empty"),
  summary: document.getElementById("summary"),
  selected: document.getElementById("selected"),
  menu: document.getElementById("menu"),
  headers: Array.from(document.querySelectorAll("th")),
  healthDot: document.getElementById("health-dot"),
  healthText: document.getElementById("health-text"),
  health: document.getElementById("health"),
  clear: document.getElementById("clear"),
  count: document.getElementById("count"),
};

function setEmpty(title, sub) {
  if (!els.empty) return;
  const titleEl = els.empty.querySelector(".empty-title");
  const subEl = els.empty.querySelector(".empty-sub");
  if (titleEl || subEl) {
    if (titleEl) titleEl.textContent = title || "";
    if (subEl) subEl.textContent = sub || "";
    return;
  }
  els.empty.textContent = title || "";
}

function setCount(n) {
  if (!els.count) return;
  const value = Number(n || 0);
  els.count.textContent = value.toLocaleString();
}

function syncClearButton() {
  if (!els.clear || !els.query) return;
  els.clear.hidden = !els.query.value;
}

function api() {
  return window.go && window.go.main && window.go.main.UIApp;
}

async function call(method, ...args) {
  const backend = await waitForBackend();
  if (!backend || !backend[method]) throw new Error("seekfs UI backend is not ready");
  return backend[method](...args);
}

function waitForBackend() {
  const ready = api();
  if (ready) return Promise.resolve(ready);
  return new Promise((resolve) => {
    const started = Date.now();
    const timer = setInterval(() => {
      const backend = api();
      if (backend || Date.now() - started > 2500) {
        clearInterval(timer);
        resolve(backend);
      }
    }, 25);
  });
}

function waitForRuntime() {
  if (window.runtime && window.runtime.EventsOn) return Promise.resolve(window.runtime);
  return new Promise((resolve) => {
    const started = Date.now();
    const timer = setInterval(() => {
      if ((window.runtime && window.runtime.EventsOn) || Date.now() - started > 2500) {
        clearInterval(timer);
        resolve(window.runtime);
      }
    }, 25);
  });
}

function selectedRows() {
  if (!state.rows.length) return [];
  const rows = [];
  state.selectedSet.forEach((index) => {
    if (index >= 0 && index < state.rows.length) rows.push(state.rows[index]);
  });
  return rows;
}

function selectedPaths() {
  return selectedRows().map((row) => row.path);
}

function clearSelection() {
  state.selectedSet.clear();
  state.anchor = -1;
  state.lastClicked = -1;
}

function setSelected(index, opts) {
  opts = opts || {};
  const count = state.rows.length;
  if (!count) {
    clearSelection();
    applySelectionClasses();
    updateSelectedFooter();
    return;
  }
  const clamped = Math.max(0, Math.min(count - 1, index));
  if (opts.range && state.anchor >= 0) {
    state.selectedSet.clear();
    const lo = Math.min(state.anchor, clamped);
    const hi = Math.max(state.anchor, clamped);
    for (let i = lo; i <= hi; i++) state.selectedSet.add(i);
  } else if (opts.extend) {
    if (state.anchor < 0) state.anchor = clamped;
    const lo = Math.min(state.anchor, clamped);
    const hi = Math.max(state.anchor, clamped);
    for (let i = lo; i <= hi; i++) state.selectedSet.add(i);
  } else if (opts.toggle) {
    if (state.selectedSet.has(clamped)) {
      state.selectedSet.delete(clamped);
    } else {
      state.selectedSet.add(clamped);
    }
    if (opts.anchor !== undefined) state.anchor = opts.anchor;
  } else if (opts.add) {
    state.selectedSet.add(clamped);
  } else {
    state.selectedSet.clear();
    state.selectedSet.add(clamped);
    state.anchor = clamped;
  }
  if (opts.lastClicked !== undefined) state.lastClicked = opts.lastClicked;
  else state.lastClicked = clamped;
  applySelectionClasses();
  updateSelectedFooter();
}

function applySelectionClasses() {
  const rows = els.results.querySelectorAll("tr");
  for (let i = 0; i < rows.length; i++) {
    rows[i].classList.toggle("selected", state.selectedSet.has(i));
    rows[i].classList.toggle("focused", i === state.lastClicked);
    rows[i].setAttribute("aria-selected", state.selectedSet.has(i) ? "true" : "false");
    rows[i].setAttribute("aria-current", i === state.lastClicked ? "true" : "false");
  }
  if (state.lastClicked >= 0 && state.lastClicked < rows.length) {
    els.results.setAttribute("aria-activedescendant", `row-${state.lastClicked}`);
  } else {
    els.results.removeAttribute("aria-activedescendant");
  }
}

function updateSelectedFooter() {
  const size = state.selectedSet.size;
  if (size > 1) {
    els.selected.textContent = `${size} selected`;
  } else if (size === 1) {
    const first = state.selectedSet.values().next().value;
    const row = first >= 0 && first < state.rows.length ? state.rows[first] : null;
    els.selected.textContent = row ? row.path : "";
  } else {
    els.selected.textContent = "";
  }
}

// Per-row file-type icons (16x16 shell icons as PNG data URIs), cached by
// extension key so a result set costs at most a few backend calls.
const iconCache = new Map();
const iconPending = new Map();

function iconKeyFor(row) {
  if (row.is_dir) return "dir";
  const dot = (row.name || "").lastIndexOf(".");
  if (dot > 0) return row.name.slice(dot + 1).toLowerCase();
  return "file";
}

function ensureIcon(key, path, isDir) {
  if (iconCache.has(key)) return Promise.resolve(iconCache.get(key));
  if (iconPending.has(key)) return iconPending.get(key);
  const pending = call("GetFileIcon", path, isDir)
    .then((uri) => {
      const data = uri || "";
      iconCache.set(key, data);
      iconPending.delete(key);
      return data;
    })
    .catch(() => {
      iconCache.set(key, "");
      iconPending.delete(key);
      return "";
    });
  iconPending.set(key, pending);
  return pending;
}

function iconImg(row) {
  const key = iconKeyFor(row);
  const src = iconCache.has(key) && iconCache.get(key) ? ` src="${iconCache.get(key)}"` : "";
  return `<img class="row-icon" data-icon-key="${key}" alt=""${src} />`;
}

// primeIcons requests icons for every row's extension once and fills the
// matching <img> elements as each resolves.
function primeIcons() {
  const pending = [];
  const rows = els.results.querySelectorAll("tr[data-index]");
  for (const tr of rows) {
    const img = tr.querySelector("img.row-icon");
    if (!img || img.src) continue;
    const key = img.dataset.iconKey;
    const row = state.rows[Number(tr.dataset.index)];
    if (!row) continue;
    pending.push({ key, path: row.path, isDir: !!row.is_dir });
  }
  const seen = new Set();
  for (const item of pending) {
    if (seen.has(item.key)) continue;
    seen.add(item.key);
    ensureIcon(item.key, item.path, item.isDir).then((uri) => {
      if (!uri) return;
      const imgs = els.results.querySelectorAll(`img.row-icon[data-icon-key="${item.key}"]`);
      for (const img of imgs) {
        if (!img.src) img.src = uri;
      }
    });
  }
}

function renderRows(rows) {
  const wasSingle = state.selectedSet.size === 1;
  const oldActivePath = wasSingle
    ? (state.rows[state.lastClicked] || state.rows[state.selectedSet.values().next().value] || {}).path
    : undefined;
  state.rows = rows;
  state.selectedSet.clear();
  state.anchor = -1;
  if (rows.length) {
    if (oldActivePath !== undefined) {
      const preserved = rows.findIndex((row) => row.path === oldActivePath);
      if (preserved >= 0) {
        state.selectedSet.add(preserved);
        state.lastClicked = preserved;
        state.anchor = preserved;
      } else {
        state.lastClicked = -1;
      }
    } else {
      state.lastClicked = -1;
    }
  } else {
    state.lastClicked = -1;
  }
  els.results.textContent = "";
  els.results.setAttribute("role", "listbox");
  els.results.setAttribute("aria-label", "Search results");
  const frag = document.createDocumentFragment();
  rows.forEach((row, index) => {
    const tr = document.createElement("tr");
    tr.title = row.path;
    tr.dataset.index = String(index);
    tr.id = `row-${index}`;
    tr.className = "";
    tr.setAttribute("role", "option");
    tr.setAttribute("aria-selected", "false");
    tr.setAttribute("aria-current", "false");
    tr.innerHTML = `
      <td>${iconImg(row)}${escapeHtml(row.name || "")}</td>
      <td class="path-cell">${escapeHtml(row.dir || row.path || "")}</td>
      <td class="size-cell">${escapeHtml(formatSize(row))}</td>
      <td class="date-cell">${escapeHtml(formatDate(row.modified))}</td>
    `;
    frag.appendChild(tr);
  });
  els.results.appendChild(frag);
  cacheRowMetrics();
  primeIcons();
  applySelectionClasses();
  els.empty.style.display = rows.length ? "none" : "block";
  updateSelectedFooter();
}

function rowFromEvent(event) {
  const tr = event.target && event.target.closest ? event.target.closest("tr[data-index]") : null;
  if (!tr) return -1;
  return Number(tr.dataset.index);
}

// Drag-to-select gestures, Explorer-style. Two modes:
//   "row"    - mousedown on a row, then drag to extend the selection range.
//   "marquee"- mousedown on empty space, then drag to draw a rubber-band box
//              that selects every row it touches.
// Both are driven by document-level mouse events (the most reliable mechanism
// in WebView2).
let dragMode = "";
let dragAnchorIndex = -1;
let dragRaf = 0;
let dragPointerX = 0;
let dragPointerY = 0;
let dragAutoScroll = 0;
let marqueeEl = null;
let marqueeStartX = 0;
let marqueeStartY = 0;
let marqueeRaf = 0;
let pendingX = 0;
let pendingY = 0;
let rowMetrics = { scrollTopAtRender: 0, rows: [] };

// Row vertical positions cached at render time (viewport coords). Since the
// results list scrolls only inside .results-wrap, the current viewport position
// of row i is rowMetrics.rows[i].top - (wrap.scrollTop - scrollTopAtRender).
// This lets the marquee and row drag do pure arithmetic instead of measuring
// every row's bounding rect on each mouse move.
function cacheRowMetrics() {
  const wrap = els.results.closest(".results-wrap");
  const rows = els.results.querySelectorAll("tr");
  const metrics = { scrollTopAtRender: wrap ? wrap.scrollTop : 0, rows: [] };
  for (const tr of rows) {
    const rect = tr.getBoundingClientRect();
    metrics.rows.push({ top: rect.top, height: rect.height });
  }
  rowMetrics = metrics;
}

function marqueeElement() {
  if (marqueeEl) return marqueeEl;
  const wrap = els.results.closest(".results-wrap");
  marqueeEl = document.createElement("div");
  marqueeEl.className = "marquee";
  wrap.appendChild(marqueeEl);
  return marqueeEl;
}

function endDragSelection() {
  dragMode = "";
  dragAnchorIndex = -1;
  if (marqueeEl) marqueeEl.style.display = "none";
  if (dragRaf) cancelAnimationFrame(dragRaf);
  dragRaf = 0;
  if (marqueeRaf) cancelAnimationFrame(marqueeRaf);
  marqueeRaf = 0;
  if (dragAutoScroll) cancelAnimationFrame(dragAutoScroll);
  dragAutoScroll = 0;
}

function rowIndexAtY(clientY) {
  const wrap = els.results.closest(".results-wrap");
  const rows = rowMetrics.rows;
  const count = rows.length;
  if (!count || !wrap) return -1;
  const delta = wrap.scrollTop - rowMetrics.scrollTopAtRender;
  if (clientY < rows[0].top - delta) return 0;
  if (clientY >= rows[count - 1].top - delta + rows[count - 1].height) return count - 1;
  let lo = 0;
  let hi = count - 1;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (rows[mid].top - delta <= clientY) lo = mid + 1;
    else hi = mid;
  }
  return Math.max(0, lo - 1);
}

function applyDragRange(toIndex) {
  const count = state.rows.length;
  if (!count || dragAnchorIndex < 0) return;
  const lo = Math.min(dragAnchorIndex, toIndex);
  const hi = Math.max(dragAnchorIndex, toIndex);
  state.selectedSet.clear();
  for (let i = lo; i <= hi; i++) state.selectedSet.add(i);
  state.lastClicked = toIndex;
  applySelectionClasses();
  updateSelectedFooter();
}

// Draw the rubber-band box between marqueeStart and the current pointer, and
// select every row it touches. Like Explorer's details view, selection is by
// row band (vertical intersection) regardless of horizontal position, so
// dragging over the empty space right of the columns still selects rows. Uses
// the cached row metrics so no per-row layout is triggered.
function updateMarquee(clientX, clientY) {
  const wrap = els.results.closest(".results-wrap");
  if (!wrap) return;
  dragPointerX = clientX;
  dragPointerY = clientY;
  const wrapRect = wrap.getBoundingClientRect();
  const left = Math.max(wrapRect.left, Math.min(marqueeStartX, clientX));
  const top = Math.max(wrapRect.top, Math.min(marqueeStartY, clientY));
  const right = Math.min(wrapRect.right, Math.max(marqueeStartX, clientX));
  const bottom = Math.min(wrapRect.bottom, Math.max(marqueeStartY, clientY));
  const el = marqueeElement();
  el.style.display = "block";
  el.style.left = `${left}px`;
  el.style.top = `${top}px`;
  el.style.width = `${Math.max(0, right - left)}px`;
  el.style.height = `${Math.max(0, bottom - top)}px`;
  // A near-zero box (a plain click) selects nothing.
  if (right - left < 3 || bottom - top < 3) {
    state.selectedSet.clear();
    applySelectionClasses();
    updateSelectedFooter();
    return;
  }
  const delta = wrap.scrollTop - rowMetrics.scrollTopAtRender;
  const rows = rowMetrics.rows;
  state.selectedSet.clear();
  for (let i = 0; i < rows.length; i++) {
    const r = rows[i];
    const rtop = r.top - delta;
    if (rtop <= bottom && rtop + r.height >= top) {
      state.selectedSet.add(i);
    }
  }
  state.lastClicked = -1;
  applySelectionClasses();
  updateSelectedFooter();
}

function dragScrollTick() {
  const wrap = els.results.closest(".results-wrap");
  if (!wrap) {
    dragAutoScroll = 0;
    return;
  }
  const rect = wrap.getBoundingClientRect();
  const margin = 28;
  let dy = 0;
  if (dragPointerY < rect.top + margin) {
    dy = -Math.max(1, Math.ceil((rect.top + margin - dragPointerY) / 3));
  } else if (dragPointerY > rect.bottom - margin) {
    dy = Math.max(1, Math.ceil((dragPointerY - (rect.bottom - margin)) / 3));
  }
  if (dy) {
    wrap.scrollTop += dy;
    if (dragMode === "row") {
      const index = rowIndexAtY(dragPointerY);
      if (index >= 0 && index !== dragAnchorIndex) {
        dragAnchorIndex = index;
        applyDragRange(index);
      }
    } else if (dragMode === "marquee") {
      updateMarquee(dragPointerX, dragPointerY);
    }
    dragAutoScroll = requestAnimationFrame(dragScrollTick);
  } else {
    dragAutoScroll = 0;
  }
}

function onDocumentMouseDown(event) {
  if (event.button !== 0) return;
  const target = event.target;
  if (target && target.closest) {
    if (target.closest(".searchbar") || target.closest("thead") || target.closest("#menu") || target.closest(".footer")) {
      return;
    }
  }
  els.results.focus();
  const index = rowFromEvent(event);
  if (index < 0) {
    // Empty space: start a rubber-band selection box.
    clearSelection();
    applySelectionClasses();
    updateSelectedFooter();
    dragMode = "marquee";
    marqueeStartX = event.clientX;
    marqueeStartY = event.clientY;
    updateMarquee(event.clientX, event.clientY);
    return;
  }
  if (event.ctrlKey || event.metaKey) {
    setSelected(index, { toggle: true, anchor: index, lastClicked: index });
    endDragSelection();
    return;
  }
  if (event.shiftKey) {
    setSelected(index, { range: true, lastClicked: index });
    endDragSelection();
    return;
  }
  setSelected(index, { lastClicked: index });
  dragMode = "row";
  dragAnchorIndex = index;
  dragPointerY = event.clientY;
}

function onDocumentMouseMove(event) {
  if (dragMode === "marquee") {
    event.preventDefault();
    pendingX = event.clientX;
    pendingY = event.clientY;
    if (marqueeRaf) return;
    marqueeRaf = requestAnimationFrame(() => {
      marqueeRaf = 0;
      updateMarquee(pendingX, pendingY);
      if (!dragAutoScroll) dragScrollTick();
    });
    return;
  }
  if (dragMode !== "row") return;
  event.preventDefault();
  dragPointerY = event.clientY;
  const index = rowIndexAtY(event.clientY);
  if (index < 0) return;
  if (index === dragAnchorIndex) return;
  dragAnchorIndex = index;
  if (dragRaf) return;
  dragRaf = requestAnimationFrame(() => {
    dragRaf = 0;
    applyDragRange(dragAnchorIndex);
    dragScrollTick();
  });
}

function onDocumentMouseUp(event) {
  if (!dragMode) return;
  endDragSelection();
}

document.addEventListener("mousedown", onDocumentMouseDown);
document.addEventListener("mousemove", onDocumentMouseMove);
document.addEventListener("mouseup", onDocumentMouseUp);

function scrollToIndex(index) {
  const tr = els.results.querySelectorAll("tr")[index];
  if (tr) tr.scrollIntoView({ block: "nearest" });
}

function activeIndex() {
  if (state.lastClicked >= 0 && state.lastClicked < state.rows.length) return state.lastClicked;
  if (state.selectedSet.size) return state.selectedSet.values().next().value;
  return state.rows.length ? 0 : -1;
}

function updateHealthDot(health, message) {
  if (!els.healthDot) return;
  const level = ["ok", "degraded", "error"].includes(health) ? health : "error";
  els.healthDot.classList.remove("health-ok", "health-degraded", "health-error");
  els.healthDot.classList.add(`health-${level}`);
  els.healthText.textContent = message || "";
  if (els.health) {
    els.health.title = message || `Index health: ${level}`;
  }
}

async function refreshStatus() {
  try {
    const status = await call("Status");
    updateHealthDot(status.health, status.health_message);
    if (!status.ok) {
      els.summary.textContent = status.message || "Service unavailable";
      return;
    }
    const loading = status.loading ? " loading" : "";
    if (!state.lastQuery) {
      els.summary.textContent = `${status.entries.toLocaleString()} items${loading}`;
    }
  } catch (err) {
    updateHealthDot("error", err.message);
    els.summary.textContent = err.message;
  }
}

function buildQueryWithSort(rawQuery) {
  const fields = rawQuery.split(/\s+/).filter(Boolean);
  const kept = [];
  for (const field of fields) {
    const matched = /^sort:([a-z]+)/i.exec(field);
    if (matched) {
      // A typed sort: token drives the header indicator too.
      if (sortSupported(matched[1].toLowerCase())) state.sort = matched[1].toLowerCase();
      continue;
    }
    kept.push(field);
  }
  // The service sorts before it applies the result limit, so the page always
  // holds the globally top-N rows for the active sort rather than the first N
  // matches reordered locally.
  if (state.sort) kept.push(`sort:${state.sort}`);
  applySortIndicator();
  return kept.join(" ");
}

async function searchNow() {
  const rawQuery = els.query.value.trim();
  const query = normalizeLiveQuery(buildQueryWithSort(rawQuery));
  state.lastQuery = rawQuery;
  const seq = ++state.seq;
  hideMenu();
  syncClearButton();
  if (!rawQuery) {
    renderRows([]);
    setCount(0);
    els.summary.textContent = "Ready";
    setEmpty("Type to search", "");
    return;
  }
  if (!query) {
    renderRows([]);
    setCount(0);
    els.summary.textContent = "Keep typing";
    setEmpty("Keep typing", "");
    return;
  }
  els.summary.textContent = "Searching...";
  try {
    await call("SearchAsync", {
      query,
      limit: 200,
    }, seq);
  } catch (err) {
    if (seq !== state.seq) return;
    renderRows([]);
    setCount(0);
    setEmpty(err.message, "");
    els.summary.textContent = err.message;
  }
}

function handleSearchResponse(response) {
  if (!response || response.seq !== state.seq) return;
  if ((response.message || "").toLowerCase() === "query superseded") return;
  if (!response.ok) {
    renderRows([]);
    setCount(0);
    setEmpty(response.message || "Search failed", "");
    els.summary.textContent = response.message || "Search failed";
    return;
  }
  renderRows(response.results || []);
  const count = response.count || (response.results || []).length;
  setCount(count);
  els.summary.textContent = `${count.toLocaleString()} items  |  ${response.elapsed_ms} ms`;
  setEmpty("No matches", "");
}

const searchSoon = debounce(searchNow, 90);

function headerColumn(header) {
  return header && header.dataset ? header.dataset.sort || "" : "";
}

function applySortIndicator() {
  for (const header of els.headers) {
    const column = headerColumn(header);
    const active = column !== "" && column === state.sort;
    header.classList.toggle("sorted", active);
    let arrow = header.querySelector(".sort-arrow");
    if (active) {
      if (!arrow) {
        arrow = document.createElement("span");
        arrow.className = "sort-arrow";
        arrow.textContent = " ▲";
        header.appendChild(arrow);
      }
    } else if (arrow) {
      arrow.remove();
    }
  }
}

// Sorting re-queries the service so the result limit is applied to the sorted
// set; clicking the active column (or the unsorted Name column) clears it.
function setSort(column) {
  if (!sortSupported(column)) {
    if (state.sort === "") return;
    state.sort = "";
  } else {
    state.sort = state.sort === column ? "" : column;
  }
  applySortIndicator();
  searchNow();
}

els.headers.forEach((header) => {
  header.addEventListener("click", () => {
    const column = headerColumn(header);
    if (column) setSort(column);
  });
});

const COLUMN_WIDTHS_KEY = "seekfs.columnWidths";
const COLUMN_MIN_WIDTH = 60;
const COLUMN_ORDER = ["name", "path", "size", "modified"];
const COLUMN_DEFAULT_WIDTHS = { name: 300, path: 340, size: 108, modified: 190 };

function colWidths() {
  let saved = {};
  try {
    saved = JSON.parse(localStorage.getItem(COLUMN_WIDTHS_KEY) || "{}");
  } catch (err) {
    saved = {};
  }
  return saved;
}

function saveColWidths() {
  try {
    localStorage.setItem(COLUMN_WIDTHS_KEY, JSON.stringify(state.colWidths));
  } catch (err) {
    // Ignore storage errors (e.g. private mode).
  }
}

function columnWidth(column) {
  const width = state.colWidths && state.colWidths[column];
  return width || COLUMN_DEFAULT_WIDTHS[column];
}

// The table gets an explicit pixel width: table-layout: fixed only takes
// effect when the table has a non-auto width, otherwise the browser falls back
// to content-based sizing and a long path widens its column after every query.
function applyColWidths() {
  const cols = document.querySelectorAll(".results col");
  const table = document.querySelector(".results");
  let total = 0;
  COLUMN_ORDER.forEach((column, index) => {
    const width = columnWidth(column);
    total += width;
    if (cols[index]) cols[index].style.width = `${width}px`;
  });
  if (table) table.style.width = `${total}px`;
}

function beginColumnResize(header, event) {
  const column = headerColumn(header) || "name";
  if (COLUMN_ORDER.indexOf(column) === -1) return;
  event.preventDefault();
  event.stopPropagation();
  state.colWidths = state.colWidths || {};
  const startX = event.clientX;
  const startWidth = columnWidth(column);
  const table = header.closest("table");
  table.classList.add("resizing");
  let frame = 0;
  let width = startWidth;
  const apply = () => {
    frame = 0;
    state.colWidths[column] = width;
    applyColWidths();
  };
  const onMove = (moveEvent) => {
    width = Math.max(COLUMN_MIN_WIDTH, Math.round(startWidth + moveEvent.clientX - startX));
    if (!frame) frame = requestAnimationFrame(apply);
  };
  const onUp = () => {
    if (frame) {
      cancelAnimationFrame(frame);
      frame = 0;
    }
    table.classList.remove("resizing");
    window.removeEventListener("pointermove", onMove);
    window.removeEventListener("pointerup", onUp);
    state.colWidths[column] = width;
    applyColWidths();
    saveColWidths();
  };
  try {
    header.setPointerCapture(event.pointerId);
  } catch (err) {
    // Ignore: window listeners still track the drag.
  }
  window.addEventListener("pointermove", onMove);
  window.addEventListener("pointerup", onUp);
}

function initColumnResize() {
  state.colWidths = colWidths();
  applyColWidths();
  els.headers.forEach((header) => {
    const resizer = document.createElement("div");
    resizer.className = "th-resizer";
    header.appendChild(resizer);
    resizer.addEventListener("pointerdown", (event) => beginColumnResize(header, event));
    resizer.addEventListener("click", (event) => {
      event.preventDefault();
      event.stopPropagation();
    });
  });
  window.addEventListener("resize", applyColWidths);
}

initColumnResize();

function showMenu(x, y) {
  const width = 190;
  const height = 198;
  els.menu.style.left = `${Math.min(x, window.innerWidth - width - 4)}px`;
  els.menu.style.top = `${Math.min(y, window.innerHeight - height - 4)}px`;
  els.menu.classList.add("open");
}

function hideMenu() {
  els.menu.classList.remove("open");
}

async function openPath(path) {
  if (path) await call("Open", path);
}

async function openSelected() {
  const index = activeIndex();
  if (index < 0) return;
  await call("Open", state.rows[index].path);
}

async function revealSelected() {
  const paths = selectedPaths();
  if (paths.length) await call("Reveal", paths[0]);
}

async function copySelected() {
  const paths = selectedPaths();
  if (paths.length) await call("CopyPaths", paths);
}

async function copyNameSelected() {
  const rows = selectedRows();
  if (rows.length) {
    await call("CopyPaths", rows.map((row) => row.name));
  }
}

async function propsSelected() {
  const rows = selectedRows();
  if (rows.length) await call("Properties", rows[0].path);
}

let renameActive = false;

function startRename(index) {
  if (renameActive || index < 0 || index >= state.rows.length) return;
  const tr = els.results.querySelectorAll("tr")[index];
  const nameCell = tr ? tr.querySelector("td") : null;
  if (!nameCell) return;
  const row = state.rows[index];
  const dot = row.name.lastIndexOf(".");
  const prefix = dot > 0 ? row.name.slice(0, dot) : row.name;
  const input = document.createElement("input");
  input.className = "rename-input";
  input.value = row.name;
  nameCell.textContent = "";
  nameCell.appendChild(input);
  input.focus();
  input.setSelectionRange(0, prefix.length);
  renameActive = true;

  const finish = async (commit) => {
    if (!renameActive) return;
    renameActive = false;
    const value = input.value.trim();
    input.remove();
    nameCell.textContent = escapeHtml(state.rows[index] ? state.rows[index].name || "" : "");
    if (!commit) return;
    if (!value || value === row.name) return;
    try {
      await call("Rename", row.path, value);
      await searchNow();
    } catch (err) {
      els.summary.textContent = err.message || "Rename failed";
    }
  };

  input.addEventListener("keydown", (event) => {
    if (event.key === "Enter") {
      event.preventDefault();
      event.stopPropagation();
      finish(true);
    } else if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      finish(false);
    }
  });
  input.addEventListener("blur", () => finish(true));
}

async function renameSelected() {
  const index = activeIndex();
  if (index >= 0) startRename(index);
}

async function deleteSelected() {
  const paths = selectedPaths();
  if (!paths.length) return;
  if (!confirm(`Move ${paths.length} selected item${paths.length === 1 ? "" : "s"} to the Recycle Bin?`)) return;
  await call("DeleteToRecycleBin", paths);
  await searchNow();
}

function moveActive(delta) {
  if (!state.rows.length) return;
  let next = activeIndex();
  next = next < 0 ? 0 : next;
  next = Math.max(0, Math.min(state.rows.length - 1, next + delta));
  state.selectedSet.clear();
  state.selectedSet.add(next);
  state.anchor = next;
  state.lastClicked = next;
  applySelectionClasses();
  updateSelectedFooter();
  scrollToIndex(next);
}

function extendActive(delta) {
  if (!state.rows.length) return;
  let next = activeIndex();
  next = next < 0 ? 0 : next;
  next = Math.max(0, Math.min(state.rows.length - 1, next + delta));
  if (state.anchor < 0) state.anchor = next;
  const lo = Math.min(state.anchor, next);
  const hi = Math.max(state.anchor, next);
  state.selectedSet.clear();
  for (let i = lo; i <= hi; i++) state.selectedSet.add(i);
  state.lastClicked = next;
  applySelectionClasses();
  updateSelectedFooter();
  scrollToIndex(next);
}

function moveFocus(delta) {
  if (!state.rows.length) return;
  let next = activeIndex();
  next = next < 0 ? 0 : next;
  next = Math.max(0, Math.min(state.rows.length - 1, next + delta));
  state.lastClicked = next;
  applySelectionClasses();
  updateSelectedFooter();
  scrollToIndex(next);
}

function selectAll() {
  state.selectedSet.clear();
  for (let i = 0; i < state.rows.length; i++) state.selectedSet.add(i);
  if (state.rows.length) state.anchor = 0;
  applySelectionClasses();
  updateSelectedFooter();
}

function goToEdge(edge) {
  if (!state.rows.length) return;
  const next = edge === "home" ? 0 : state.rows.length - 1;
  state.selectedSet.clear();
  state.selectedSet.add(next);
  state.anchor = next;
  state.lastClicked = next;
  applySelectionClasses();
  updateSelectedFooter();
  scrollToIndex(next);
}

function toggleFocused() {
  const index = activeIndex();
  if (index < 0) return;
  if (state.selectedSet.has(index)) {
    state.selectedSet.delete(index);
  } else {
    state.selectedSet.add(index);
  }
  state.anchor = index;
  state.lastClicked = index;
  applySelectionClasses();
  updateSelectedFooter();
  scrollToIndex(index);
}

async function openContextMenuAt(index, x, y) {
  if (index < 0) {
    showMenu(x, y);
    return;
  }
  if (!state.selectedSet.has(index)) {
    setSelected(index, { lastClicked: index });
  }
  hideMenu();
  try {
    const result = await call("ShowShellContextMenu", selectedPaths(), Math.round(x), Math.round(y));
    if (result === true) return;
    console.log("shell context menu not shown:", result);
    showMenu(x, y);
  } catch (err) {
    console.log("shell context menu error:", err);
    els.summary.textContent = `menu: ${err.message || err}`;
    showMenu(x, y);
  }
}

function showMenuForEvent(event) {
  const index = rowFromEvent(event);
  openContextMenuAt(index, event.clientX, event.clientY);
}

els.results.addEventListener("dblclick", (event) => {
  const index = rowFromEvent(event);
  if (index < 0) return;
  openPath(state.rows[index].path);
});

els.results.addEventListener("contextmenu", (event) => {
  event.preventDefault();
  showMenuForEvent(event);
});

// Prebuild the native shell context menu on hover so a right-click on the same
// selection shows instantly instead of paying the ~300ms QueryContextMenu cost.
// Uses the same selection logic as a right-click: the whole selection when the
// hovered row is part of it, otherwise just the hovered row.
const prebuildContextMenu = debounce((index) => {
  if (index < 0 || !state.rows.length) return;
  const paths = state.selectedSet.has(index) ? selectedPaths() : [state.rows[index].path];
  if (!paths.length) return;
  call("PrebuildShellContextMenu", paths);
}, 250);

els.results.addEventListener("pointermove", (event) => {
  prebuildContextMenu(rowFromEvent(event));
});

els.results.addEventListener("keydown", async (event) => {
  const key = event.key;
  if (key === "Escape") {
    hideMenu();
    return;
  }
  if (key === "ArrowDown") {
    event.preventDefault();
    if (event.shiftKey) extendActive(1);
    else if (event.ctrlKey || event.metaKey) moveFocus(1);
    else moveActive(1);
    return;
  }
  if (key === "ArrowUp") {
    event.preventDefault();
    if (event.shiftKey) extendActive(-1);
    else if (event.ctrlKey || event.metaKey) moveFocus(-1);
    else moveActive(-1);
    return;
  }
  if (key === "Home") {
    event.preventDefault();
    goToEdge("home");
    return;
  }
  if (key === "End") {
    event.preventDefault();
    goToEdge("end");
    return;
  }
  if (key === " ") {
    event.preventDefault();
    toggleFocused();
    return;
  }
  if ((event.ctrlKey || event.metaKey) && key.toLowerCase() === "a") {
    event.preventDefault();
    selectAll();
    return;
  }
  if ((event.ctrlKey || event.metaKey) && key.toLowerCase() === "c") {
    if (window.getSelection && window.getSelection().toString().length > 0) {
      return;
    }
    event.preventDefault();
    await copySelected();
    return;
  }
  if (key === "Enter") {
    event.preventDefault();
    await openSelected();
    return;
  }
  if (key === "F2") {
    event.preventDefault();
    await renameSelected();
    return;
  }
  if (key === "Delete") {
    event.preventDefault();
    await deleteSelected();
    return;
  }
  if (key === "ContextMenu" || event.keyCode === 93) {
    event.preventDefault();
    const index = activeIndex();
    const tr = index >= 0 ? els.results.querySelectorAll("tr")[index] : null;
    const rect = tr ? tr.getBoundingClientRect() : els.results.getBoundingClientRect();
    openContextMenuAt(index, Math.round(rect.left + rect.width / 2), Math.round(rect.top + rect.height / 2));
    return;
  }
});

els.query.addEventListener("input", (event) => {
  if (event.target.value !== state.lastQuery) {
    state.sort = "";
    applySortIndicator();
  }
  syncClearButton();
  searchSoon();
});

els.query.addEventListener("keydown", (event) => {
  if (event.key === "Escape" && els.query.value) {
    event.stopPropagation();
    els.query.value = "";
    syncClearButton();
    searchNow();
  }
});

els.clear.addEventListener("click", () => {
  els.query.value = "";
  state.sort = "";
  applySortIndicator();
  syncClearButton();
  els.query.focus();
  searchNow();
});

els.menu.addEventListener("click", async (event) => {
  const button = event.target.closest("button[data-action]");
  if (!button) return;
  hideMenu();
  const action = button.dataset.action;
  if (action === "open") await openSelected();
  if (action === "reveal") await revealSelected();
  if (action === "copy") await copySelected();
  if (action === "copy-name") await copyNameSelected();
  if (action === "rename") await renameSelected();
  if (action === "properties") await propsSelected();
  if (action === "delete") await deleteSelected();
});

document.addEventListener("mousedown", (event) => {
  if (!els.menu.contains(event.target)) hideMenu();
});

document.addEventListener("keydown", (event) => {
  if (event.key === "Escape") hideMenu();
});

// Global shortcut fallbacks so Explorer-style shortcuts work even when the
// search input holds focus (the results table may not have DOM focus).
document.addEventListener("keydown", (event) => {
  const tag = event.target && event.target.tagName ? event.target.tagName.toLowerCase() : "";
  const inEditable = tag === "input" || tag === "textarea" || (event.target && event.target.isContentEditable);
  if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "k") {
    event.preventDefault();
    els.query.focus();
    els.query.select();
    return;
  }
  if (event.key === "/" && !inEditable) {
    event.preventDefault();
    els.query.focus();
    return;
  }
  if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "a") {
    if (inEditable) {
      // The search input selects its own text on Ctrl+A; treat a
      // non-empty query as "keep typing" and do not steal the keystroke.
      return;
    }
    if (!state.rows.length) return;
    event.preventDefault();
    selectAll();
    return;
  }
  if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "c" && !inEditable && state.selectedSet.size) {
    if (window.getSelection && window.getSelection().toString().length > 0) return;
    event.preventDefault();
    copySelected();
    return;
  }
});

setTimeout(refreshStatus, 150);
setInterval(refreshStatus, 10000);

waitForRuntime().then((runtime) => {
  if (runtime && runtime.EventsOn) {
    runtime.EventsOn("seekfs:search-results", handleSearchResponse);
  }
});
