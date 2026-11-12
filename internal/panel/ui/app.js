/* MiniMax Code 2 API 管理台前端
   无框架、无构建步骤；所有文本走 textContent，避免把上游错误信息当 HTML 注入。 */
(() => {
  'use strict';

  const $ = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

  const KEY_STORE = 'mmx2a.key';

  const state = {
    key: localStorage.getItem(KEY_STORE) || '',
    view: 'overview',
    accounts: [],
    tasks: [],
    pollTimer: null,
    logTimer: null,
    busy: false,
  };

  // ---------- 基础工具 ----------

  const fmtNum = (n) => {
    n = Number(n) || 0;
    if (n >= 1e8) return (n / 1e8).toFixed(2) + ' 亿';
    if (n >= 1e4) return (n / 1e4).toFixed(n >= 1e6 ? 0 : 1) + ' 万';
    return String(n);
  };

  const fmtDuration = (ms) => {
    ms = Number(ms) || 0;
    if (ms < 1000) return ms + 'ms';
    if (ms < 60000) return (ms / 1000).toFixed(1) + 's';
    return (ms / 60000).toFixed(1) + 'min';
  };

  const fmtClock = (iso) => {
    if (!iso) return '—';
    const d = new Date(iso);
    if (isNaN(d)) return '—';
    const p = (x) => String(x).padStart(2, '0');
    return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
  };

  const fmtLeft = (sec) => {
    sec = Number(sec) || 0;
    if (sec <= 0) return '已过期';
    if (sec < 3600) return Math.round(sec / 60) + ' 分钟';
    if (sec < 86400) return (sec / 3600).toFixed(1) + ' 小时';
    return (sec / 86400).toFixed(1) + ' 天';
  };

  const el = (tag, cls, text) => {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = String(text);
    return n;
  };

  let toastTimer = null;
  const toast = (msg, isErr = false) => {
    const t = $('#toast');
    // 🔴 hidden 必须显式去掉：CSS 里有 [hidden]{display:none!important}，
    // 只改 className 的话提示永远是隐藏的 —— 之前面板所有操作都"没有反馈"，
    // 根因就在这里（toast 元素带 hidden 属性，JS 从没移除过）。
    t.hidden = false;
    t.textContent = msg;
    t.className = 'toast show' + (isErr ? ' err' : '');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { t.className = 'toast'; }, 3200);
  };

  // ---------- API ----------

  async function api(path, opts = {}) {
    const headers = Object.assign({ 'Content-Type': 'application/json' }, opts.headers || {});
    if (state.key) headers.Authorization = 'Bearer ' + state.key;
    const res = await fetch(path, Object.assign({}, opts, { headers }));
    const text = await res.text();
    let data = null;
    if (text) {
      try { data = JSON.parse(text); } catch { data = { raw: text }; }
    }
    if (!res.ok) {
      const msg = (data && data.error && (data.error.message || data.error.code)) || text || ('HTTP ' + res.status);
      const err = new Error(typeof msg === 'string' ? msg : JSON.stringify(msg));
      err.status = res.status;
      throw err;
    }
    return data;
  }

  // ---------- 登录 ----------

  function showGate() {
    $('#gate').hidden = false;
    $('#shell').hidden = true;
    setTimeout(() => { const k = $('#gate-key'); if (k) k.focus(); }, 50);
  }

  function showShell() {
    $('#gate').hidden = true;
    $('#shell').hidden = false;
    navigate(location.hash || '#/overview', true);
  }

  async function tryLogin(key) {
    state.key = key;
    await api('/panel/api/login', { method: 'POST', body: JSON.stringify({ key }) });
    localStorage.setItem(KEY_STORE, key);
  }

  $('#gate-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const key = $('#gate-key').value.trim();
    const errBox = $('#gate-err');
    errBox.hidden = true;
    if (!key) return;
    try {
      await tryLogin(key);
      showShell();
    } catch (err) {
      errBox.textContent = err.message || '登录失败';
      errBox.hidden = false;
    }
  });

  $('#logout').addEventListener('click', () => {
    localStorage.removeItem(KEY_STORE);
    state.key = '';
    stopPolling();
    stopLogAuto();
    stopTaskAuto();
    stopTaskSync();
    showGate();
  });

  // ---------- 路由 ----------

  const views = ['overview', 'accounts', 'models', 'tasks', 'signin', 'logs', 'settings'];

  function navigate(hash, silent) {
    const name = String(hash || '').replace(/^#\/?/, '') || 'overview';
    const view = views.includes(name) ? name : 'overview';
    state.view = view;
    $$('.view').forEach((v) => v.classList.toggle('active', v.id === 'view-' + view));
    $$('#nav a').forEach((a) => a.classList.toggle('active', a.dataset.view === view));
    if (!silent) location.hash = '#/' + view;
    loadView(view);
  }

  window.addEventListener('hashchange', () => navigate(location.hash, true));

  function loadView(view) {
    // 离开任务页就停掉轮询：面板切走了还在后台打上游，是最容易忘的泄漏。
    if (view !== 'tasks') {
      stopTaskAuto();
      stopTaskSync();
    }
    if (view === 'overview') { loadOverview(); loadUsage(); }
    if (view === 'accounts') loadAccounts();
    if (view === 'models') loadModels();
    if (view === 'tasks') loadTasks();
    if (view === 'signin') loadSignin();
    if (view === 'logs') loadLogs();
    if (view === 'settings') loadConfig();
  }

  // ---------- 概览 ----------

  async function loadOverview() {
    try {
      const d = await api('/panel/api/overview');
      $('#ver').textContent = 'v' + d.version;
      renderStats(d);
      renderEndpoints(d);
      loadQuota();
    } catch (err) {
      if (err.status === 401) return showGate();
      toast(err.message, true);
    }
  }

  function renderStats(d) {
    const box = $('#stats');
    box.textContent = '';
    const today = d.today || {};
    const all = d.all_time || {};
    const items = [
      { k: '账号', v: `${(d.accounts && d.accounts.enabled) || 0}/${(d.accounts && d.accounts.total) || 0}`, s: ((d.accounts && d.accounts.cooling) || 0) + ' 个冷却中' },
      { k: '今日请求', v: fmtNum(today.requests), s: (today.errors || 0) + ' 次失败' },
      { k: '今日 Token', v: fmtNum((today.prompt_tokens || 0) + (today.output_tokens || 0)), s: `入 ${fmtNum(today.prompt_tokens)} / 出 ${fmtNum(today.output_tokens)}` },
      { k: '累计请求', v: fmtNum(all.requests), s: `缓存命中 ${fmtNum(all.cached_tokens)} tok` },
    ];
    for (const it of items) {
      const card = el('div', 'stat');
      card.appendChild(el('div', 'k', it.k));
      card.appendChild(el('div', 'v', it.v));
      card.appendChild(el('div', 's', it.s));
      box.appendChild(card);
    }
  }

  function renderEndpoints(d) {
    const box = $('#endpoints');
    box.textContent = '';
    const origin = location.origin;
    const rows = [
      ['OpenAI 兼容', origin + '/v1/chat/completions'],
      ['Anthropic 直通', origin + '/v1/messages'],
      ['模型列表', origin + '/v1/models'],
      ['健康检查', origin + '/healthz'],
      ['上游', d.base_url || '—'],
      ['调度策略', d.strategy || '—'],
      ['运行时长', fmtDuration((d.uptime_seconds || 0) * 1000)],
    ];
    for (const [k, v] of rows) {
      const row = el('div', 'kv');
      row.appendChild(el('span', 'k', k));
      row.appendChild(el('span', 'v mono', v));
      box.appendChild(row);
    }
  }

  // 图片生成额度：次数池，读不到就只显示原因，绝不影响概览其它部分。
  async function loadQuota() {
    const card = $('#quota-card');
    if (!card) return;
    try {
      const d = await api('/panel/api/quota');
      renderQuota(d);
    } catch (err) {
      if (err.status === 401) return showGate();
      card.hidden = false;
      const box = $('#quota-list');
      box.textContent = '';
      box.appendChild(el('div', 'q-err', '额度读取失败：' + err.message));
    }
  }

  function renderQuota(d) {
    const card = $('#quota-card');
    const box = $('#quota-list');
    const list = (d && d.accounts) || [];
    if (!card || !box) return;
    if (!list.length) {
      card.hidden = true;
      return;
    }
    card.hidden = false;
    box.textContent = '';
    for (const q of list) {
      const row = el('div', 'quota-row');
      row.appendChild(el('div', 'q-name', q.label || q.id));
      if (q.error) {
        row.appendChild(el('div', 'q-err', '读取失败：' + q.error));
        box.appendChild(row);
        continue;
      }
      const bar = el('div', 'q-bar');
      const fill = el('i');
      const total = Number(q.total) || 0;
      const used = Number(q.used) || 0;
      const pct = total > 0 ? Math.min(100, Math.round((used / total) * 100)) : 0;
      fill.style.width = pct + '%';
      bar.appendChild(fill);
      row.appendChild(bar);
      row.appendChild(el('div', 'q-num', `已用 ${used} / ${total}　剩余 ${Number(q.remaining) || 0}`));
      box.appendChild(row);
    }
    const note = $('#quota-note');
    if (note) {
      note.textContent = d.any_active
        ? '限时免费 · 按次数计'
        : '当前不在免费活动期';
    }
  }

  async function loadUsage() {
    try {
      const d = await api('/panel/api/usage?days=14');
      renderChart(d.series || []);
    } catch (err) {
      if (err.status === 401) return showGate();
    }
  }

  function renderChart(series) {
    const box = $('#chart');
    box.textContent = '';
    if (!series.length) {
      box.appendChild(el('div', 'empty', '暂无用量数据'));
      return;
    }
    const max = Math.max(1, ...series.map((s) => (s.total && s.total.requests) || 0));
    const today = new Date().toISOString().slice(5, 10);
    for (const s of series) {
      const req = (s.total && s.total.requests) || 0;
      const col = el('div', 'bar-col');
      if (s.date && s.date.slice(5) === today) col.classList.add('on');
      const bar = el('div', 'bar');
      const h = Math.max(2, Math.round((req / max) * 108));
      bar.style.height = h + 'px';
      bar.title = `${s.date}：${req} 次请求`;
      col.appendChild(bar);
      col.appendChild(el('div', 'bar-label', s.date ? s.date.slice(5) : ''));
      box.appendChild(col);
    }
  }

  $('#refresh-overview').addEventListener('click', () => { loadOverview(); loadUsage(); });

  $('#probe-btn').addEventListener('click', async (e) => {
    const btn = e.currentTarget;
    btn.disabled = true;
    btn.textContent = '自检中…';
    try {
      const d = await api('/panel/api/probe', { method: 'POST', body: '{}' });
      if (d.ok) toast(`上游正常 · ${d.model} · ${d.latency_ms}ms`);
      else toast('上游异常：' + d.message, true);
    } catch (err) {
      toast(err.message, true);
    } finally {
      btn.disabled = false;
      btn.textContent = '连通性自检';
    }
  });

  // ---------- 账号 ----------

  async function loadAccounts() {
    try {
      const d = await api('/panel/api/accounts');
      state.accounts = d.accounts || [];
      renderAccounts();
    } catch (err) {
      if (err.status === 401) return showGate();
      toast(err.message, true);
    }
  }

  function renderAccounts() {
    const box = $('#accounts-table');
    box.textContent = '';
    if (!state.accounts.length) {
      box.appendChild(el('div', 'empty', '还没有账号。点右上角「添加账号」，用设备授权扫码登录，或粘贴 auth.json 里的 refresh_token。'));
      return;
    }
    const table = el('table');
    const thead = el('thead');
    const htr = el('tr');
    ['状态', '账号', 'Token', '过期', '用量', '操作'].forEach((h) => htr.appendChild(el('th', '', h)));
    thead.appendChild(htr);
    table.appendChild(thead);

    const tbody = el('tbody');
    for (const a of state.accounts) {
      const tr = el('tr');

      // 状态
      const tdState = el('td');
      if (a.disabled) tdState.appendChild(el('span', 'badge', '已停用'));
      else if (a.cooldown_left_sec > 0) tdState.appendChild(el('span', 'badge warn', '冷却 ' + fmtLeft(a.cooldown_left_sec)));
      else if (a.expired) tdState.appendChild(el('span', 'badge warn', 'token 过期'));
      else if (a.last_error) tdState.appendChild(el('span', 'badge err', '异常'));
      else tdState.appendChild(el('span', 'badge ok', '可用'));
      tr.appendChild(tdState);

      // 账号
      const tdName = el('td');
      tdName.appendChild(el('div', '', a.label || a.id));
      // 名称和 ID 相同时不重复显示，避免一列里出现两行一模一样的字。
      if (a.label && a.label !== a.id) {
        const sub = el('div', 'muted small mono', a.id + (a.user_id ? ' · ' + a.user_id : ''));
        tdName.appendChild(sub);
      }
      if (a.last_error) tdName.appendChild(el('div', 'small', a.last_error)).style.color = 'var(--err)';
      tr.appendChild(tdName);

      // Token
      const tdTok = el('td', 'mono');
      tdTok.textContent = a.masked_token || '—';
      tdTok.appendChild(el('div', 'muted small', a.has_refresh ? '有 refresh' : '无 refresh'));
      tr.appendChild(tdTok);

      // 过期
      const tdExp = el('td');
      tdExp.appendChild(el('div', 'small', a.expires_at || '长期'));
      tdExp.appendChild(el('div', 'muted small', a.expires_in_sec > 0 ? '剩 ' + fmtLeft(a.expires_in_sec) : '—'));
      tr.appendChild(tdExp);

      // 用量
      const tdUse = el('td', 'num');
      tdUse.textContent = fmtNum(a.request_count) + ' 次';
      tdUse.appendChild(el('div', 'muted small', '入 ' + fmtNum(a.prompt_tokens) + ' / 出 ' + fmtNum(a.output_tokens)));
      tr.appendChild(tdUse);

      // 操作
      const tdOps = el('td');
      const ops = el('div');
      ops.style.display = 'flex';
      ops.style.gap = '4px';
      ops.style.flexWrap = 'wrap';

      ops.appendChild(opBtn('测试', async () => {
        toast('正在测试…');
        try {
          const r = await api(`/panel/api/accounts/${encodeURIComponent(a.id)}/test`, { method: 'POST', body: '{}' });
          if (r.ok) toast(`可用 · ${r.latency_ms}ms · 回复：${(r.reply || '').slice(0, 30)}`);
          else toast('失败：' + r.message, true);
          loadAccounts();
        } catch (err) { toast(err.message, true); }
      }));

      ops.appendChild(opBtn(a.disabled ? '启用' : '停用', async () => {
        try {
          await api(`/panel/api/accounts/${encodeURIComponent(a.id)}/toggle`, { method: 'POST', body: '{}' });
          toast(a.disabled ? '已启用' : '已停用');
          loadAccounts();
        } catch (err) { toast(err.message, true); }
      }));

      ops.appendChild(opBtn('改名', async () => {
        const name = prompt('账号名称（最多 40 字）', a.label || a.id);
        if (name === null) return;
        try {
          await api(`/panel/api/accounts/${encodeURIComponent(a.id)}/label`, {
            method: 'POST',
            body: JSON.stringify({ label: name }),
          });
          toast('已改名');
          loadAccounts();
        } catch (err) { toast(err.message, true); }
      }));

      ops.appendChild(opBtn('清冷却', async () => {
        try {
          await api(`/panel/api/accounts/${encodeURIComponent(a.id)}/reset`, { method: 'POST', body: '{}' });
          toast('已清除冷却与错误');
          loadAccounts();
        } catch (err) { toast(err.message, true); }
      }));

      const del = opBtn('删除', async () => {
        if (!confirm(`确定删除账号 ${a.label || a.id}？\n这会同时尝试撤销服务端 refresh_token。`)) return;
        try {
          await api(`/panel/api/accounts/${encodeURIComponent(a.id)}/remove`, { method: 'POST', body: '{}' });
          toast('已删除');
          loadAccounts();
        } catch (err) { toast(err.message, true); }
      });
      del.className = 'btn btn-ghost btn-danger';
      ops.appendChild(del);

      tdOps.appendChild(ops);
      tr.appendChild(tdOps);
      tbody.appendChild(tr);
    }
    table.appendChild(tbody);
    box.appendChild(table);
  }

  function opBtn(label, onClick) {
    const b = el('button', 'btn btn-ghost', label);
    b.style.padding = '4px 8px';
    b.style.fontSize = '12px';
    b.addEventListener('click', onClick);
    return b;
  }

  $('#refresh-accounts').addEventListener('click', loadAccounts);

  // ---------- 添加账号 ----------

  function openModal() {
    $('#modal').hidden = false;
    $('#modal-err').hidden = true;
    $('#device-box').hidden = true;
    $('#device-code').textContent = '————';
    $('#device-status').textContent = '等待授权…';
  }

  function closeModal() {
    $('#modal').hidden = true;
    stopPolling();
  }

  $('#add-account').addEventListener('click', openModal);
  $('#modal-close').addEventListener('click', closeModal);
  $('#modal').addEventListener('click', (e) => { if (e.target === $('#modal')) closeModal(); });

  $$('#add-tabs .tab').forEach((tab) => {
    tab.addEventListener('click', () => {
      $$('#add-tabs .tab').forEach((t) => t.classList.toggle('active', t === tab));
      $$('.tab-panel').forEach((p) => p.classList.toggle('active', p.id === 'tab-' + tab.dataset.tab));
    });
  });

  function stopPolling() {
    if (state.pollTimer) {
      clearInterval(state.pollTimer);
      state.pollTimer = null;
    }
  }

  $('#device-start').addEventListener('click', async (e) => {
    const btn = e.currentTarget;
    btn.disabled = true;
    stopPolling();
    try {
      const d = await api('/panel/api/accounts/device/start', { method: 'POST', body: '{}' });
      $('#device-box').hidden = false;
      $('#device-code').textContent = d.user_code;
      const link = $('#device-link');
      link.href = d.verification_uri_complete || d.verification_uri;
      $('#device-status').textContent = `请在浏览器确认授权（${Math.round(d.expires_in / 60)} 分钟内有效）`;
      window.open(link.href, '_blank', 'noopener');

      const interval = Math.max(2, d.interval || 3) * 1000;
      let polls = 0;
      state.pollTimer = setInterval(async () => {
        polls++;
        if (polls > 100) { stopPolling(); $('#device-status').textContent = '轮询超时，请重新生成'; return; }
        try {
          const r = await api('/panel/api/accounts/device/poll?code=' + encodeURIComponent(d.device_code));
          if (r.status === 'ok') {
            stopPolling();
            $('#device-status').textContent = '授权成功，账号已加入池';
            toast('账号已添加');
            closeModal();
            loadAccounts();
          } else if (r.status === 'pending') {
            $('#device-status').textContent = '等待在浏览器确认…';
          } else if (r.status === 'slow_down') {
            $('#device-status').textContent = '服务端要求降频，稍候…';
          } else if (r.status === 'expired') {
            stopPolling();
            $('#device-status').textContent = r.message || '授权已过期';
          } else if (r.status === 'error') {
            stopPolling();
            const box = $('#modal-err');
            box.textContent = r.message || '授权失败';
            box.hidden = false;
          }
        } catch (err) {
          stopPolling();
          const box = $('#modal-err');
          box.textContent = err.message;
          box.hidden = false;
        }
      }, interval);
    } catch (err) {
      const box = $('#modal-err');
      box.textContent = err.message;
      box.hidden = false;
    } finally {
      btn.disabled = false;
    }
  });

  $('#import-submit').addEventListener('click', async (e) => {
    const btn = e.currentTarget;
    const box = $('#modal-err');
    box.hidden = true;
    const refresh = $('#import-refresh').value.trim();
    const access = $('#import-access').value.trim();
    const label = $('#import-label').value.trim();
    if (!refresh && !access) {
      box.textContent = '至少填一个 token';
      box.hidden = false;
      return;
    }
    btn.disabled = true;
    btn.textContent = '导入中…';
    try {
      await api('/panel/api/accounts/import', {
        method: 'POST',
        body: JSON.stringify({ refresh_token: refresh, access_token: access, label }),
      });
      toast('导入成功');
      $('#import-refresh').value = '';
      $('#import-access').value = '';
      $('#import-label').value = '';
      closeModal();
      loadAccounts();
    } catch (err) {
      box.textContent = err.message;
      box.hidden = false;
    } finally {
      btn.disabled = false;
      btn.textContent = '导入';
    }
  });

  // ---------- 模型 ----------

  async function loadModels() {
    try {
      const d = await api('/panel/api/models');
      const box = $('#models-body');
      box.textContent = '';

      const table = el('table');
      const thead = el('thead');
      const htr = el('tr');
      ['模型', '类型', '上下文', '最大输出', '思考', '默认'].forEach((h) => htr.appendChild(el('th', '', h)));
      thead.appendChild(htr);
      table.appendChild(thead);

      // 类别与端点一一对应：徽章表达的是"该发哪个接口"，不是"好坏"。
      const KIND = {
        chat: ['对话', 'badge ok'],
        image: ['图片', 'badge accent'],
        video: ['视频', 'badge'],
      };
      const tbody = el('tbody');
      for (const m of d.models || []) {
        const tr = el('tr');
        const tdID = el('td', 'mono nowrap', m.id);
        if (m.aliases && m.aliases.length) {
          tdID.appendChild(el('div', 'muted small nowrap', '别名 ' + m.aliases.join(' / ')));
        }
        tr.appendChild(tdID);

        const tdKind = el('td');
        const kind = KIND[m.type] || KIND.chat;
        tdKind.appendChild(el('span', kind[1], kind[0]));
        tr.appendChild(tdKind);

        // 媒体模型没有上下文/思考这些概念，用「—」而不是 0 或"未知"。
        tr.appendChild(el('td', 'num', m.context_window ? fmtNum(m.context_window) : '—'));
        tr.appendChild(el('td', 'num', m.max_output ? fmtNum(m.max_output) : '—'));
        const tdThink = el('td');
        if (m.thinking === 'forced_adaptive') tdThink.appendChild(el('span', 'badge warn', '强制自适应'));
        else if (m.thinking === 'switchable') tdThink.appendChild(el('span', 'badge ok', '可开关'));
        else tdThink.textContent = '—';
        tr.appendChild(tdThink);
        const tdDef = el('td');
        if (m.id === d.default) tdDef.appendChild(el('span', 'badge accent', '默认'));
        tr.appendChild(tdDef);
        tbody.appendChild(tr);
      }
      table.appendChild(tbody);
      box.appendChild(table);

      const aliasWrap = el('div');
      aliasWrap.style.marginTop = '16px';
      aliasWrap.appendChild(el('div', 'muted small', '别名映射：'));
      const alias = el('div', 'mono small');
      alias.style.marginTop = '6px';
      const entries = Object.entries(d.aliases || {});
      alias.textContent = entries.length ? entries.map(([k, v]) => `${k} → ${v}`).join('   ·   ') : '（无）';
      aliasWrap.appendChild(alias);
      box.appendChild(aliasWrap);
    } catch (err) {
      if (err.status === 401) return showGate();
      toast(err.message, true);
    }
  }

  $('#refresh-models').addEventListener('click', loadModels);

  // ---------- 任务 ----------

  // 六态与上游视频协议一致；未知状态由网关收敛成 running 并标 unnormalized。
  const TASK_STATE = {
    queued: ['排队中', 'badge'],
    running: ['进行中', 'badge accent'],
    succeeded: ['已完成', 'badge ok'],
    failed: ['失败', 'badge err'],
    expired: ['已过期', 'badge warn'],
    cancelled: ['已取消', 'badge warn'],
  };
  const TASK_TERMINAL = ['succeeded', 'failed', 'expired', 'cancelled'];

  let taskTimer = null;
  let taskSyncTimer = null;

  async function loadTasks() {
    try {
      const d = await api('/panel/api/tasks?limit=200');
      state.tasks = d.items || [];
      renderTasks(state.tasks);
    } catch (err) {
      if (err.status === 401) return showGate();
      toast(err.message, true);
    }
  }

  function renderTasks(items) {
    const box = $('#tasks-body');
    box.textContent = '';
    if (!items.length) {
      box.appendChild(el('div', 'empty', '还没有任务。通过 /v1/contents/generations/tasks 提交的异步任务会出现在这里。'));
      return;
    }
    const table = el('table');
    const thead = el('thead');
    const htr = el('tr');
    ['创建', '任务 ID', '模型', '状态', '参数', '更新', '操作'].forEach((h) => htr.appendChild(el('th', '', h)));
    thead.appendChild(htr);
    table.appendChild(thead);

    const tbody = el('tbody');
    for (const t of items) {
      const tr = el('tr');
      tr.appendChild(el('td', 'num small', fmtClock(t.created_at ? new Date(t.created_at * 1000).toISOString() : '')));

      const tdID = el('td', 'mono small', t.id);
      tdID.style.maxWidth = '180px';
      tdID.style.overflow = 'hidden';
      tdID.style.textOverflow = 'ellipsis';
      tr.appendChild(tdID);

      tr.appendChild(el('td', 'mono small', t.client_model || t.upstream_model || '—'));

      const tdState = el('td');
      const [label, cls] = TASK_STATE[t.status] || [t.status || '未知', 'badge'];
      tdState.appendChild(el('span', cls, label));
      if (t.unnormalized_status) tdState.appendChild(el('span', 'badge warn', '未归一'));
      tr.appendChild(tdState);

      const p = t.params || {};
      const bits = [];
      if (p.resolution) bits.push(p.resolution);
      if (p.ratio) bits.push(p.ratio);
      if (p.duration) bits.push(p.duration + 's');
      tr.appendChild(el('td', 'small muted', bits.join(' · ') || '—'));

      const tdUpd = el('td', 'small muted');
      tdUpd.textContent = t.updated_at ? fmtClock(new Date(t.updated_at * 1000).toISOString()) : '—';
      if (t.error_message) {
        tdUpd.appendChild(el('div', 'small', t.error_message.slice(0, 60))).style.color = 'var(--err)';
      }
      tr.appendChild(tdUpd);

      const tdOps = el('td');
      const ops = el('div');
      ops.style.display = 'flex';
      ops.style.gap = '4px';

      if (!TASK_TERMINAL.includes(t.status)) {
        ops.appendChild(opBtn('同步', async () => {
          try {
            await api(`/panel/api/tasks/${encodeURIComponent(t.id)}/sync`, { method: 'POST', body: '{}' });
            toast('已同步上游状态');
          } catch (err) {
            toast(err.message, true);
          }
          loadTasks();
        }));
      } else {
        if (t.video_url) {
          const a = el('a', 'btn btn-ghost', '产物');
          a.href = t.video_url;
          a.target = '_blank';
          a.rel = 'noopener';
          a.style.padding = '4px 8px';
          a.style.fontSize = '12px';
          ops.appendChild(a);
        }
        const del = opBtn('删除', async () => {
          if (!confirm(`删除任务 ${t.id} 的本地记录？`)) return;
          try {
            await api(`/panel/api/tasks/${encodeURIComponent(t.id)}/delete`, { method: 'POST', body: '{}' });
            toast('已删除');
          } catch (err) {
            toast(err.message, true);
          }
          loadTasks();
        });
        del.className = 'btn btn-ghost btn-danger';
        ops.appendChild(del);
      }

      tdOps.appendChild(ops);
      tr.appendChild(tdOps);
      tbody.appendChild(tr);
    }
    table.appendChild(tbody);
    box.appendChild(table);
  }

  function stopTaskAuto() {
    if (taskTimer) {
      clearInterval(taskTimer);
      taskTimer = null;
    }
    const cb = $('#task-auto');
    if (cb) cb.checked = false;
  }

  function stopTaskSync() {
    if (taskSyncTimer) {
      clearInterval(taskSyncTimer);
      taskSyncTimer = null;
    }
    const cb = $('#task-auto-sync');
    if (cb) cb.checked = false;
  }

  // 自动同步：只推非终态的任务，且一次最多 5 个 —— 面板的轮询不该变成
  // 对上游的请求放大（每个任务一次上游往返）。
  async function syncRunningTasks() {
    const running = (state.tasks || []).filter((t) => !TASK_TERMINAL.includes(t.status));
    if (!running.length) return;
    for (const t of running.slice(0, 5)) {
      try {
        await api(`/panel/api/tasks/${encodeURIComponent(t.id)}/sync`, { method: 'POST', body: '{}' });
      } catch { /* 单个失败不打断整轮，下一轮还会试 */ }
    }
    loadTasks();
  }

  $('#refresh-tasks').addEventListener('click', loadTasks);
  $('#task-auto').addEventListener('change', (e) => {
    stopTaskAuto();
    if (e.currentTarget.checked) taskTimer = setInterval(loadTasks, 3000);
  });
  $('#task-auto-sync').addEventListener('change', (e) => {
    stopTaskSync();
    if (e.currentTarget.checked) {
      syncRunningTasks();
      taskSyncTimer = setInterval(syncRunningTasks, 15000);
    }
  });

  // ---------- 签到 ----------

  const SIGNIN_STATE = {
    1: ['未到', 'badge'],
    2: ['可领', 'badge accent'],
    3: ['已领', 'badge ok'],
  };

  async function loadSignin() {
    try {
      const d = await api('/panel/api/signin');
      renderSignin(d);
    } catch (err) {
      if (err.status === 401) return showGate();
      toast(err.message, true);
    }
  }

  function renderSignin(d) {
    const s = d.summary || {};

    $('#signin-auto').checked = !!d.auto;
    $('#signin-at').value = d.at || '';
    $('#signin-auto-note').textContent = d.auto
      ? `每天 ${d.at}（${d.timezone || 'Asia/Shanghai'}）自动签，进程内定时器`
      : '自动签到已关闭';

    const box = $('#signin-stats');
    box.textContent = '';
    const items = [
      { k: '账号', v: String(s.accounts || 0), s: `${s.claimed || 0} 个本次新领` },
      { k: '本次积分', v: String(s.points || 0), s: '签到积分 30 天有效' },
      { k: '连续天数', v: s.day_no ? '第 ' + s.day_no + ' 天' : '—', s: '满 7 天一轮，4000 分' },
      { k: '失败', v: String(s.failed || 0), s: '失败的号不影响其他号' },
    ];
    for (const it of items) {
      const card = el('div', 'stat');
      card.appendChild(el('div', 'k', it.k));
      card.appendChild(el('div', 'v', it.v));
      card.appendChild(el('div', 's', it.s));
      box.appendChild(card);
    }

    const wrap = $('#signin-body');
    wrap.textContent = '';
    const list = d.results || [];
    if (!list.length) {
      wrap.appendChild(el('div', 'empty', '还没有账号。先在「账号」页添加。'));
      return;
    }
    const table = el('table');
    const thead = el('thead');
    const htr = el('tr');
    ['状态', '账号', '今天', '本次', '说明'].forEach((h) => htr.appendChild(el('th', '', h)));
    thead.appendChild(htr);
    table.appendChild(thead);

    const tbody = el('tbody');
    for (const r of list) {
      const tr = el('tr');
      const tdOK = el('td');
      if (r.ok) tdOK.appendChild(el('span', 'badge ok', '正常'));
      else tdOK.appendChild(el('span', 'badge err', '失败'));
      tr.appendChild(tdOK);

      const tdName = el('td');
      tdName.appendChild(el('div', '', r.label || r.account_id));
      if (r.label && r.label !== r.account_id) tdName.appendChild(el('div', 'muted small mono', r.account_id));
      tr.appendChild(tdName);

      const tdToday = el('td');
      const [text, cls] = SIGNIN_STATE[r.today_status] || ['—', 'badge'];
      tdToday.appendChild(el('span', cls, r.day_no ? `第 ${r.day_no} 天 · ${text}` : text));
      tr.appendChild(tdToday);

      tr.appendChild(el('td', 'num', r.claimed ? '+' + r.points : '—'));

      const tdMsg = el('td', 'small');
      if (r.error) {
        tdMsg.textContent = r.error;
        tdMsg.style.color = 'var(--err)';
        tdMsg.style.maxWidth = '320px';
      } else {
        tdMsg.textContent = r.claimed ? '已领取' : (r.today_status === 3 ? '今日已签过' : '—');
        tdMsg.className = 'small muted';
      }
      tr.appendChild(tdMsg);
      tbody.appendChild(tr);
    }
    table.appendChild(tbody);
    wrap.appendChild(table);
  }

  $('#refresh-signin').addEventListener('click', loadSignin);

  $('#signin-claim').addEventListener('click', async (e) => {
    const btn = e.currentTarget;
    btn.disabled = true;
    btn.textContent = '签到中…';
    try {
      const d = await api('/panel/api/signin', { method: 'POST', body: '{}' });
      const s = d.summary || {};
      if (s.points > 0) toast(`签到成功 +${s.points} 积分`);
      else if (s.failed > 0) toast(`${s.failed} 个账号签到失败，见详情`, true);
      else toast('今日已签到');
      await loadSignin();
    } catch (err) {
      toast(err.message, true);
    } finally {
      btn.disabled = false;
      btn.textContent = '立即签到';
    }
  });

  $('#signin-save').addEventListener('click', async () => {
    const auto = $('#signin-auto').checked;
    const at = ($('#signin-at').value || '').trim();
    try {
      await api('/panel/api/config', {
        method: 'POST',
        body: JSON.stringify({ signin_auto: auto, signin_at: at || '00:10' }),
      });
      toast('已保存，1 分钟内生效');
      await loadSignin();
    } catch (err) {
      toast(err.message, true);
    }
  });

  // ---------- 日志 ----------

  async function loadLogs() {
    try {
      const d = await api('/panel/api/logs?limit=200');
      renderLogs(d.logs || []);
    } catch (err) {
      if (err.status === 401) return showGate();
      toast(err.message, true);
    }
  }

  function renderLogs(logs) {
    const box = $('#logs-body');
    box.textContent = '';
    if (!logs.length) {
      box.appendChild(el('div', 'empty', '还没有请求记录。'));
      return;
    }
    const table = el('table');
    const thead = el('thead');
    const htr = el('tr');
    ['时间', '模型', '状态', '耗时', 'Token', '账号', '来源', '错误'].forEach((h) => htr.appendChild(el('th', '', h)));
    thead.appendChild(htr);
    table.appendChild(thead);

    const tbody = el('tbody');
    for (const l of logs) {
      const tr = el('tr');
      tr.appendChild(el('td', 'num small', fmtClock(l.time)));
      const tdModel = el('td', 'mono small', l.model || '—');
      if (l.stream) tdModel.appendChild(el('span', 'badge', 'stream'));
      tr.appendChild(tdModel);

      const tdStatus = el('td');
      const code = l.status || 0;
      const cls = code === 0 ? 'err' : code < 400 ? 'ok' : code < 500 ? 'warn' : 'err';
      tdStatus.appendChild(el('span', 'badge ' + cls, code === 0 ? '未响应' : String(code)));
      tr.appendChild(tdStatus);

      tr.appendChild(el('td', 'num small', fmtDuration(l.duration_ms)));
      tr.appendChild(el('td', 'num small', `${fmtNum(l.prompt_tokens)} / ${fmtNum(l.output_tokens)}`));
      tr.appendChild(el('td', 'small', l.account || '—'));
      const tdSrc = el('td', 'small muted');
      tdSrc.textContent = (l.client_ip || '') + (l.ua ? ' · ' + l.ua.slice(0, 24) : '');
      tr.appendChild(tdSrc);

      const tdErr = el('td', 'small');
      if (l.error) {
        tdErr.textContent = l.error;
        tdErr.style.color = 'var(--err)';
        tdErr.style.maxWidth = '260px';
      } else {
        tdErr.textContent = '—';
      }
      tr.appendChild(tdErr);
      tbody.appendChild(tr);
    }
    table.appendChild(tbody);
    box.appendChild(table);
  }

  function stopLogAuto() {
    if (state.logTimer) {
      clearInterval(state.logTimer);
      state.logTimer = null;
    }
    const cb = $('#log-auto');
    if (cb) cb.checked = false;
  }

  $('#refresh-logs').addEventListener('click', loadLogs);
  $('#log-auto').addEventListener('change', (e) => {
    stopLogAuto();
    if (e.currentTarget.checked) {
      state.logTimer = setInterval(loadLogs, 3000);
    }
  });

  // ---------- 设置 ----------

  async function loadConfig() {
    try {
      const d = await api('/panel/api/config');
      $('#cfg-api-key').value = '';
      $('#cfg-api-key').placeholder = d.api_key_masked ? `当前 ${d.api_key_masked}（留空不改）` : '未设置';
      $('#cfg-admin-password').value = '';
      $('#cfg-default-model').value = (d.defaults && d.defaults.model) || '';
      $('#cfg-max-tokens').value = (d.defaults && d.defaults.max_tokens) || '';
      $('#cfg-thinking').value = (d.defaults && d.defaults.thinking_policy) || 'auto';
      $('#cfg-strategy').value = (d.pool && d.pool.strategy) || 'weighted';
      $('#cfg-inflight').value = (d.pool && d.pool.max_in_flight) || '';
      $('#cfg-inflight-acct').value = (d.pool && d.pool.max_in_flight_per_account) || '';
      $('#cfg-aliases').value = JSON.stringify((d.models && d.models.aliases) || {}, null, 2);
    } catch (err) {
      if (err.status === 401) return showGate();
      toast(err.message, true);
    }
  }

  $('#save-config').addEventListener('click', async (e) => {
    const btn = e.currentTarget;
    let aliases = null;
    try {
      aliases = JSON.parse($('#cfg-aliases').value || '{}');
    } catch {
      toast('别名不是合法 JSON', true);
      return;
    }
    const patch = {
      default_model: $('#cfg-default-model').value.trim(),
      max_tokens: Number($('#cfg-max-tokens').value) || 0,
      thinking_policy: $('#cfg-thinking').value,
      strategy: $('#cfg-strategy').value,
      max_in_flight: Number($('#cfg-inflight').value) || 0,
      max_in_flight_per_account: Number($('#cfg-inflight-acct').value) || 0,
      aliases,
    };
    const key = $('#cfg-api-key').value.trim();
    if (key) patch.api_key = key;
    const admin = $('#cfg-admin-password').value.trim();
    if (admin) patch.admin_password = admin;

    btn.disabled = true;
    btn.textContent = '保存中…';
    try {
      await api('/panel/api/config', { method: 'POST', body: JSON.stringify(patch) });
      if (key) {
        state.key = key;
        localStorage.setItem(KEY_STORE, key);
      }
      toast('已保存并生效');
    } catch (err) {
      toast(err.message, true);
    } finally {
      btn.disabled = false;
      btn.textContent = '保存';
    }
  });

  // ---------- 启动 ----------

  (async function boot() {
    if (!state.key) {
      showGate();
      return;
    }
    try {
      await api('/panel/api/overview');
      showShell();
    } catch {
      localStorage.removeItem(KEY_STORE);
      state.key = '';
      showGate();
    }
  })();
})();
