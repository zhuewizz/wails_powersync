// 前端主逻辑：表清单 + 数据表格 + 增删改 + 同步状态。
//
// 所有后端能力都来自 Go 侧的 Service（wails3 generate bindings 生成的绑定），
// 同步状态与数据变化通过事件推送（sync:status / data:changed）。
import { Events } from "@wailsio/runtime";
import { Service } from "../bindings/powersyncwails";
import { DataChanged, SyncStatus } from "../bindings/events";

const $ = (id) => document.getElementById(id);

const state = {
  tables: [],
  table: null,       // 当前表名
  columns: [],       // 当前表的列定义（{name,type,isID}）
  page: null,        // RowsPage
  offset: 0,
  limit: 100,
  filter: "",
  onlyData: true,
  includeDeleted: false,
  setupOpen: false,
  editingID: null,   // null = 新增
  ready: false,
};

// 把前端异常写到 Go 日志（WebView 的 console 不方便看）
function reportError(where, err) {
  const msg = `${where}: ${err && err.message ? err.message : err}`;
  console.error(msg, err);
  try { Service.Log(msg); } catch (_) { /* 后端还没起来 */ }
}
window.addEventListener("error", (e) => reportError("window.error", e.error || e.message));
window.addEventListener("unhandledrejection", (e) => reportError("unhandledrejection", e.reason));

// ---------------------------------------------------------------- 工具

function toast(msg, kind = "") {
  const el = $("toast");
  el.textContent = msg;
  el.className = "toast " + kind;
  el.hidden = false;
  clearTimeout(toast._t);
  toast._t = setTimeout(() => { el.hidden = true; }, kind === "err" ? 6000 : 2600);
}

function cellText(v) {
  if (v === null || v === undefined) return { text: "NULL", null: true };
  if (typeof v === "object") return { text: JSON.stringify(v), null: false };
  const s = String(v);
  if (s.length > 160) return { text: s.slice(0, 160) + "…", null: false };
  return { text: s, null: false };
}

function fmtRows(n) {
  return n >= 10000 ? (n / 1000).toFixed(1) + "k" : String(n);
}

// ---------------------------------------------------------------- 状态栏

function renderStatus(st) {
  state.ready = !!st.ready;
  $("dbpath").textContent = st.dbPath + (st.readOnly ? "  （只读：另一个实例在同步）" : "");

  const conn = $("pill-conn");
  if (!st.ready) {
    conn.textContent = "连接 …";
    conn.className = "pill busy";
  } else if (st.connected) {
    conn.textContent = "已连接";
    conn.className = "pill ok";
  } else {
    conn.textContent = "未连接";
    conn.className = "pill bad";
  }

  const net = $("pill-net");
  net.textContent = st.online ? `网络 ${st.latencyMs}ms` : "网络 离线";
  net.className = "pill " + (st.online ? "ok" : "bad");

  const cur = $("pill-cursor");
  cur.textContent = "cursor " + (st.cursor || "—");
  cur.className = "pill";

  const bk = $("pill-buckets");
  bk.textContent = "buckets " + st.buckets;
  bk.className = "pill";

  const q = $("pill-queue");
  q.textContent = "待上传 " + st.uploadQueue;
  q.className = "pill " + (st.uploadQueue > 0 ? "warn" : "");

  const sy = $("pill-sync");
  sy.textContent = st.message
    ? st.message
    : st.lastSyncedAt ? "同步于 " + st.lastSyncedAt : "尚未同步";
  sy.className = "pill " + (st.message ? "busy" : "");

  $("btn-insert").disabled = st.readOnly || !st.ready || st.needsSetup;
  $("btn-sync").disabled = st.readOnly || !st.ready || st.needsSetup;
  if (st.needsSetup) {
    openSetup(st.config || {});
  } else if (state.setupOpen) {
    closeSetup();
  }
}

// ---------------------------------------------------------------- 连接设置

function openSetup(cfg) {
  if (state.setupOpen) return;
  state.setupOpen = true;
  const set = (id, v) => { const el = $(id); if (el) el.value = v || ""; };
  set("cfg-endpoint", cfg.endpoint);
  set("cfg-rpc", cfg.rpc);
  set("cfg-rest", cfg.rest);
  set("cfg-box", cfg.box);
  set("cfg-org", cfg.org);
  $("setup").hidden = false;
  const first = $("cfg-endpoint");
  if (!cfg.endpoint && first) first.focus();
}

function closeSetup() {
  state.setupOpen = false;
  $("setup").hidden = true;
}

async function saveSetup() {
  const cfg = {
    endpoint: $("cfg-endpoint").value.trim(),
    rpc: $("cfg-rpc").value.trim(),
    rest: $("cfg-rest").value.trim(),
    box: $("cfg-box").value.trim(),
    org: $("cfg-org").value.trim(),
  };
  if (!cfg.endpoint || !cfg.rpc || !cfg.box || !cfg.org) {
    toast("服务地址 / RPC / box_code / org_code 都必须填", "err");
    return;
  }
  try {
    await Service.SaveConfig(cfg);
    toast("已保存，正在连接…", "ok");
    closeSetup();
    state.tables = [];
    setTimeout(tickOnce, 500);
  } catch (err) {
    toast("保存失败：" + err, "err");
  }
}

// ---------------------------------------------------------------- 表清单

async function loadTables() {
  try {
    state.tables = await Service.Tables();
  } catch (err) {
    console.error(err);
    $("tables-foot").textContent = "读取表清单失败：" + err;
    return;
  }
  renderTables();
  if (!state._loggedBoot) {
    state._loggedBoot = true;
    try { Service.Log("前端启动完成：表清单 " + state.tables.length + " 张"); } catch (_) {}
  }
}

function renderTables() {
  const ul = $("tables");
  ul.innerHTML = "";
  const kw = state.filter.trim().toLowerCase();
  const list = state.tables.filter((t) => {
    if (state.onlyData && t.rows === 0) return false;
    if (kw && !t.name.toLowerCase().includes(kw)) return false;
    return true;
  });

  for (const t of list) {
    const li = document.createElement("li");
    li.className = (t.alias ? "alias " : "") + (t.name === state.table ? "active" : "");
    li.title = t.alias
      ? `${t.name}：同步规则里的短别名表（object_type 就是别名）`
      : t.name;
    const delTxt = t.deleted > 0 ? ` <span class="del" title="软删除行（deleted_at 非空），默认不显示">+${t.deleted}</span>` : "";
    li.innerHTML = `<span class="name">${t.name}</span><span class="rows">${fmtRows(t.rows)}${delTxt}</span>`;
    li.onclick = () => selectTable(t.name);
    ul.appendChild(li);
  }

  const totalRows = state.tables.reduce((a, t) => a + t.rows, 0);
  const nonEmpty = state.tables.filter((t) => t.rows > 0).length;
  $("tables-foot").textContent =
    `共 ${state.tables.length} 张表（有数据 ${nonEmpty} 张），显示 ${list.length} 张，合计 ${totalRows} 行`;
  if (list.length === 0) {
    ul.innerHTML = `<li style="cursor:default;color:var(--fg2)">（没有匹配的表；取消「只看有数据」或清空过滤）</li>`;
  }
}

// ---------------------------------------------------------------- 数据表格

async function selectTable(name) {
  state.table = name;
  state.offset = 0;
  state.editingID = null;
  renderTables();
  $("table-title").textContent = name;
  try {
    state.columns = await Service.Columns(name);
    await Service.Watch(name);
  } catch (err) {
    toast("选择表失败：" + err, "err");
    return;
  }
  await loadRows();
}

async function loadRows() {
  if (!state.table) return;
  try {
    state.page = await Service.Rows(state.table, state.limit, state.offset, state.includeDeleted);
  } catch (err) {
    toast("读取数据失败：" + err, "err");
    return;
  }
  renderGrid();
}

function renderGrid() {
  const page = state.page;
  const head = $("grid-head");
  const body = $("grid-body");
  head.innerHTML = "";
  body.innerHTML = "";

  if (!page) {
    $("grid-empty").hidden = false;
    $("grid-empty").textContent = "还没有选择表";
    return;
  }

  const delHint = page.deleted > 0
    ? (page.includeDeleted ? `（含 ${page.deleted} 行软删除）` : `（另有 ${page.deleted} 行软删除未显示）`)
    : "";
  $("table-hint").textContent =
    `共 ${page.total} 行${delHint} · 第 ${page.total === 0 ? 0 : page.offset + 1}–${Math.min(page.offset + page.limit, page.total)} 行`;

  // 表头
  const tr = document.createElement("tr");
  for (const c of page.columns) {
    const th = document.createElement("th");
    th.textContent = c;
    tr.appendChild(th);
  }
  const thOps = document.createElement("th");
  thOps.textContent = "操作";
  tr.appendChild(thOps);
  head.appendChild(tr);

  if (page.rows.length === 0) {
    $("grid-empty").hidden = false;
    $("grid-empty").textContent = "（这张表没有数据）";
    return;
  }
  $("grid-empty").hidden = true;

  for (const row of page.rows) {
    const tr = document.createElement("tr");
    const rowDeleted = row.deleted_at !== null && row.deleted_at !== undefined;
    for (const c of page.columns) {
      const td = document.createElement("td");
      const { text, null: isNull } = cellText(row[c]);
      td.textContent = text;
      td.title = text;
      if (isNull) td.classList.add("null");
      if (typeof row[c] === "number") td.classList.add("num");
      if (rowDeleted) td.classList.add("deleted");
      tr.appendChild(td);
    }
    const td = document.createElement("td");
    const wrap = document.createElement("div");
    wrap.className = "row-actions";

    const edit = document.createElement("button");
    edit.textContent = "编辑";
    edit.onclick = () => openModal(row);
    wrap.appendChild(edit);

    const del = document.createElement("button");
    del.textContent = "删除";
    del.onclick = () => removeRow(row);
    wrap.appendChild(del);

    td.appendChild(wrap);
    tr.appendChild(td);
    body.appendChild(tr);
  }

  const from = page.offset + 1;
  const to = Math.min(page.offset + page.limit, page.total);
  $("page-info").textContent = `${from}–${to} / ${page.total}`;
  $("btn-prev").disabled = page.offset === 0;
  $("btn-next").disabled = to >= page.total;
}

// ---------------------------------------------------------------- 弹窗（新增/编辑）

function openModal(row) {
  state.editingID = row ? row.id : null;
  const isEdit = !!row;
  $("modal-title").textContent = isEdit ? `编辑 ${state.table} / ${row.id}` : `新增 ${state.table}`;
  const box = $("modal-fields");
  box.innerHTML = "";

  const cols = (state.columns || []).filter((c) => !c.isID);
  if (cols.length === 0) {
    toast("这张表没有可编辑的列（或列定义未加载）", "err");
    return;
  }
  for (const c of cols) {
    const wrap = document.createElement("div");
    wrap.className = "field";
    const value = isEdit ? row[c.name] : undefined;
    const isNull = isEdit && (value === null || value === undefined);
    const text = value === null || value === undefined ? "" : String(value);

    const label = document.createElement("label");
    label.innerHTML = `<span>${c.name}</span><span class="type">${c.type}</span>`;

    const input = document.createElement("input");
    input.type = "text";
    input.dataset.col = c.name;
    input.dataset.ctype = c.type;
    input.value = text;
    input.disabled = isNull;

    const nullRow = document.createElement("div");
    nullRow.className = "nullrow";
    const nullChk = document.createElement("input");
    nullChk.type = "checkbox";
    nullChk.checked = isNull;
    nullChk.onchange = () => {
      input.disabled = nullChk.checked;
      if (nullChk.checked) input.value = "";
    };
    nullRow.appendChild(nullChk);
    nullRow.appendChild(document.createTextNode("写 NULL"));

    wrap.appendChild(label);
    wrap.appendChild(input);
    wrap.appendChild(nullRow);
    box.appendChild(wrap);
  }

  $("modal").hidden = false;
  const first = box.querySelector("input[type=text]:not([disabled])");
  if (first) first.focus();
}

function closeModal() {
  $("modal").hidden = true;
  state.editingID = null;
}

function collectValues() {
  const values = {};
  for (const input of $("modal-fields").querySelectorAll("input[data-col]")) {
    const name = input.dataset.col;
    const type = input.dataset.ctype;
    const nullChk = input.parentElement.querySelector(".nullrow input[type=checkbox]");
    if (nullChk && nullChk.checked) {
      values[name] = null;
      continue;
    }
    const raw = input.value;
    if (raw === "") {
      values[name] = "";
      continue;
    }
    if (type === "INTEGER" && /^-?\d+$/.test(raw.trim())) {
      values[name] = Number(raw.trim());
    } else if (type === "REAL" && /^-?\d+(\.\d+)?$/.test(raw.trim())) {
      values[name] = Number(raw.trim());
    } else {
      values[name] = raw;
    }
  }
  return values;
}

async function saveModal() {
  if (!state.table) return;
  const values = collectValues();
  try {
    if (state.editingID) {
      const n = await Service.Update(state.table, state.editingID, values);
      toast(`已更新（影响 ${n} 行），等待同步…`, "ok");
    } else {
      const id = await Service.Insert(state.table, values);
      toast(`已新增 ${id}，等待同步…`, "ok");
    }
    closeModal();
    setTimeout(loadRows, 300);
    setTimeout(loadTables, 600);
  } catch (err) {
    toast("保存失败：" + err, "err");
  }
}

async function removeRow(row) {
  const desc = row.error_desc || row.mat_name || row.user_name || row.id;
  if (!confirm(`删除 ${state.table} 的这一行？\n\n${desc}`)) return;
  try {
    const n = await Service.Delete(state.table, row.id);
    toast(`已删除（影响 ${n} 行），等待同步…`, "ok");
    setTimeout(loadRows, 300);
    setTimeout(loadTables, 600);
  } catch (err) {
    toast("删除失败：" + err, "err");
  }
}

// ---------------------------------------------------------------- 事件绑定

function bindUI() {
  $("modal").hidden = true;   // 兜底：任何情况下启动都不显示弹窗
  $("filter").addEventListener("input", (e) => {
    state.filter = e.target.value;
    renderTables();
  });
  $("only-data").addEventListener("change", (e) => {
    state.onlyData = e.target.checked;
    renderTables();
  });
  const delChk = $("include-deleted");
  if (delChk) {
    delChk.checked = state.includeDeleted;
    delChk.onchange = (e) => {
      state.includeDeleted = e.target.checked;
      state.offset = 0;
      loadRows();
    };
  }
  $("page-size").addEventListener("change", (e) => {
    state.limit = Number(e.target.value) || 100;
    state.offset = 0;
    loadRows();
  });
  $("btn-prev").onclick = () => {
    state.offset = Math.max(0, state.offset - state.limit);
    loadRows();
  };
  $("btn-next").onclick = () => {
    state.offset += state.limit;
    loadRows();
  };
  $("btn-reload").onclick = () => {
    loadRows();
    loadTables();
  };
  $("btn-insert").onclick = async () => {
    if (!state.table) {
      toast("先在左侧选一张表，再点新增", "err");
      return;
    }
    if (!state.columns || state.columns.length === 0) {
      try {
        state.columns = await Service.Columns(state.table);
      } catch (err) {
        toast("读取列定义失败：" + err, "err");
        return;
      }
    }
    openModal(null);
  };
  $("btn-sync").onclick = async () => {
    try {
      await Service.SyncNow();
      toast("已重连同步", "ok");
    } catch (err) {
      toast("重连失败：" + err, "err");
    }
  };
  $("btn-check").onclick = async () => {
    if (!state.table) return;
    try {
      const r = await Service.Check(state.table);
      const icon = r.ok ? "✅" : "⚠️";
      const del = (r.localDeleted || r.remoteDeleted)
        ? `（软删除：本地 ${r.localDeleted} / 后端 ${r.remoteDeleted}）`
        : "";
      toast(`${icon} ${r.table}: 未删除 本地 ${r.local} / 后端 ${r.remote}${del} — ${r.note || ""}`, r.ok ? "ok" : "err");
    } catch (err) {
      toast("核对失败：" + err, "err");
    }
  };
  $("btn-settings").onclick = () => openSetup((state.status && state.status.config) || {});
  $("setup-cancel").onclick = () => {
    if (state.status && state.status.needsSetup) {
      toast("还没配置连接参数，填完才能同步", "err");
      return;
    }
    closeSetup();
  };
  $("setup-save").onclick = saveSetup;
  $("modal-close").onclick = closeModal;
  $("modal-cancel").onclick = closeModal;
  $("modal-ok").onclick = saveModal;
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && !$("modal").hidden) closeModal();
    if (e.key === "Enter" && e.ctrlKey && !$("modal").hidden) saveModal();
  });
}

// ---------------------------------------------------------------- 启动

async function tickOnce() {
  try {
    const st = await Service.Status();
    state.status = st;
    renderStatus(st);
    if (st.ready && !st.needsSetup && state.tables.length === 0) await loadTables();
  } catch (err) {
    /* 后端还没起来 */
  }
}

async function boot() {
  bindUI();

  Events.On(SyncStatus, (ev) => {
    renderStatus(ev.data || {});
    if (!state.table && ev.data && ev.data.ready) loadTables();
  });
  Events.On(DataChanged, (ev) => {
    if (ev.data === state.table) loadRows();
    loadTables();
  });

  // 事件可能比窗口加载更早；先主动拉一次状态与表清单，然后持续轮询兜底。
  await tickOnce();
  setInterval(tickOnce, 2000);
}

boot().catch((err) => {
  reportError("boot", err);
  document.getElementById("tables-foot").textContent = "前端启动失败：" + (err && err.message ? err.message : err);
});
