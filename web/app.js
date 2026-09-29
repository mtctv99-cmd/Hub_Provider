const $ = s => document.querySelector(s);
const $$ = s => [...document.querySelectorAll(s)];
const esc = v => String(v ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const fmt = sec => sec < 60 ? `${sec}s` : sec < 3600 ? `${Math.floor(sec / 60)}m` : `${Math.floor(sec / 3600)}h ${Math.floor(sec / 60) % 60}m`;
const normStatus = s => String(s || 'UNCONFIGURED').toLowerCase().replaceAll('_', '-');
const icon = (name, className = 'ui-icon') => {
  const paths = {
    open: '<path d="M14 5h5v5"/><path d="M19 5l-8 8"/><path d="M19 13v5a1 1 0 0 1-1 1H6a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1h5"/>',
    refresh: '<path d="M20 11a8 8 0 1 0 2 5"/><path d="M20 4v7h-7"/>',
    validate: '<path d="m5 12 4 4L19 6"/>',
    disable: '<circle cx="12" cy="12" r="9"/><path d="M5.6 5.6 18.4 18.4"/>',
    trash: '<path d="M4 7h16"/><path d="M10 11v6M14 11v6"/><path d="M6 7l1 13h10l1-13M9 7l1-3h4l1 3"/>',
    close: '<path d="m6 6 12 12M18 6 6 18"/>',
    dashboard: '<rect x="4" y="4" width="6" height="6" rx="1"/><rect x="14" y="4" width="6" height="6" rx="1"/><rect x="4" y="14" width="6" height="6" rx="1"/><rect x="14" y="14" width="6" height="6" rx="1"/>',
    accounts: '<circle cx="12" cy="8" r="3"/><path d="M5 20c.8-3.3 3.1-5 7-5s6.2 1.7 7 5"/>',
    settings: '<path d="M12 3v2M12 19v2M3 12h2M19 12h2M5.6 5.6 7 7M17 17l1.4 1.4M18.4 5.6 17 7M7 17l-1.4 1.4"/><circle cx="12" cy="12" r="3"/>',
    voice: '<path d="M4 10v4M8 7v10M12 4v16M16 7v10M20 10v4"/>'
  };
  return `<svg class="${className}" viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">${paths[name] || ''}</svg>`;
};

const formatDuration = ms => {
  const seconds = Math.round(Number(ms || 0) / 1000);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  const remainder = seconds % 60;
  if (minutes < 60) return `${minutes}m ${remainder}s`;
  return `${Math.floor(minutes / 60)}h ${minutes % 60}m`;
};

async function fetchJSON(url, options = {}) {
  const response = await fetch(url, { credentials: 'same-origin', ...options });
  if (response.status === 401 && url.startsWith('/admin/')) {
    showAuth('Phiên đăng nhập đã hết hạn.');
    throw new Error('Phiên đăng nhập đã hết hạn.');
  }
  return response;
}

let authReady = false;
let refreshTimer = null;
let refreshInFlight = false;
let refreshQueued = false;
let trafficPeriod = '24h';
let lastTrafficFetch = 0;
let lastGoogleMarkup = '';
let lastCopilotMarkup = '';
let lastTrafficPoints = '';
let trafficInFlight = false;

function authView(view) {
  ['auth-login-view', 'auth-first-login-view', 'auth-change-view'].forEach(id => {
    const el = document.getElementById(id);
    if (el) el.hidden = id !== view;
  });
  const input = document.querySelector(`#${view} input`);
  if (input) setTimeout(() => input.focus(), 0);
}

function showAuth(message = '') {
  authReady = false;
  if (refreshTimer) clearTimeout(refreshTimer);
  refreshTimer = null;
  refreshInFlight = false;
  refreshQueued = false;
  apiKeyLoadVersion++;
  workerLoadVersion++;
  workerSnapshot = null;
  if ($('#api-key-reveal-dialog')?.open) $('#api-key-reveal-dialog').close();
  if ($('#api-key-create-dialog')?.open) $('#api-key-create-dialog').close();
  const overlay = $('#auth-overlay');
  if (!overlay) return;
  overlay.hidden = false;
  authView('auth-login-view');
  const error = $('#login-error');
  if (error) error.textContent = message;
}

function finishAuth() {
  authReady = true;
  const overlay = $('#auth-overlay');
  if (overlay) overlay.hidden = true;
  if ($('.tab-panel[data-panel="settings"]')?.classList.contains('active')) {
    loadAPIKeys();
    loadPiperSettings();
    loadWorkers();
  }
  refresh();
}

async function checkAdminAuth() {
  try {
    const r = await fetch('/admin/api/auth/check', { credentials: 'same-origin' });
    const state = await r.json();
    if (!state.authenticated) {
      showAuth();
      return false;
    }
    if (state.first_login) {
      $('#auth-overlay').hidden = false;
      authView('auth-first-login-view');
      return false;
    }
    finishAuth();
    return true;
  } catch (_) {
    showAuth('Không thể kiểm tra phiên đăng nhập.');
    return false;
  }
}

async function adminLogin(password) {
  const r = await fetch('/admin/api/login', {
    method: 'POST', credentials: 'same-origin',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({password})
  });
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error('Mật khẩu không chính xác.');
  if (data.first_login) {
    authView('auth-first-login-view');
  } else {
    finishAuth();
  }
}

async function changeAdminPassword(currentPassword, newPassword) {
  const r = await fetch('/admin/api/change-password', {
    method: 'POST', credentials: 'same-origin',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({current_password: currentPassword, new_password: newPassword})
  });
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data?.error?.message || 'Không thể đổi mật khẩu.');
}

function activeTab() {
  return $('.tab-panel.active')?.dataset.panel || 'dashboard';
}

function visibleProvider() {
  return $('.provider-panel.active')?.dataset.providerSection || 'google';
}

function scheduleRefresh(delay = 5000) {
  if (refreshTimer) clearTimeout(refreshTimer);
  refreshTimer = null;
  if (authReady && !document.hidden && activeTab() !== 'settings') {
    refreshTimer = setTimeout(() => { refreshTimer = null; refresh(); }, delay);
  }
}

function setTab(tab) {
  $$('.nav-tab').forEach(x => x.classList.toggle('active', x.dataset.tab === tab));
  $$('.tab-panel').forEach(x => x.classList.toggle('active', x.dataset.panel === tab));
  localStorage.setItem('sp-active-tab', tab);
  location.hash = tab;
  if (!authReady) return;
  if (tab === 'settings') {
    if (refreshTimer) clearTimeout(refreshTimer);
    refreshTimer = null;
    loadAPIKeys();
    loadPiperSettings();
    loadWorkers();
  } else {
    refresh();
  }
}

function initTabs() {
  const initial = location.hash.slice(1) || localStorage.getItem('sp-active-tab') || 'dashboard';
  $$('.nav-tab').forEach(x => x.addEventListener('click', () => setTab(x.dataset.tab)));
  setTab($(`.nav-tab[data-tab="${initial}"]`) ? initial : 'dashboard');
  $$('.provider-switch').forEach(x => x.addEventListener('click', event => {
    event.stopPropagation();
    setTab('accounts');
    $$('.provider-switch').forEach(b => b.classList.toggle('active', b === x));
    $$('.provider-panel').forEach(p => p.classList.toggle('active', p.dataset.providerSection === x.dataset.providerPanel));
	    if (authReady && x.dataset.providerPanel === 'copilot') loadCopilotAccounts();
	    if (authReady && x.dataset.providerPanel === 'deepseek') loadDeepSeekAccounts();
	    if (authReady && x.dataset.providerPanel === 'tiktok') loadTikTokAccounts();
    accountNavWrap?.classList.remove('dock-open');
  }));
  const accountNavWrap = $('.account-nav-wrap');
  let dockCloseTimer;
  const openDock = () => {
    clearTimeout(dockCloseTimer);
    accountNavWrap?.classList.add('dock-open');
  };
  const closeDock = () => {
    clearTimeout(dockCloseTimer);
    dockCloseTimer = setTimeout(() => accountNavWrap?.classList.remove('dock-open'), 550);
  };
  accountNavWrap?.addEventListener('click', event => {
    if (event.target.closest('.provider-switch')) return;
    event.preventDefault();
    event.stopPropagation();
    if (accountNavWrap.classList.contains('dock-open')) {
      accountNavWrap.classList.remove('dock-open');
    } else {
      openDock();
    }
  });
  document.addEventListener('pointermove', event => {
    if (accountNavWrap?.classList.contains('dock-open') && !accountNavWrap.contains(event.target)) {
      closeDock();
    }
  });
  document.addEventListener('pointerdown', event => {
    if (accountNavWrap?.classList.contains('dock-open') && !accountNavWrap.contains(event.target)) {
      accountNavWrap.classList.remove('dock-open');
    }
  });
}

function renderBreakers(status) {
  const breakers = status.breakers || {};
  const keys = Object.keys(breakers);
  const list = $('#breaker-list');
  if (!list) return;
  list.innerHTML = keys.length ? keys.map(k => `
    <span class="chip ${breakers[k] === 'OPEN' ? 'open' : ''}">${esc(k)} · ${esc(breakers[k])}</span>
  `).join('') : '<div class="empty-state">Không có provider circuit breaker nào bị ngắt.</div>';
}

function providerState(c) {
  if (!c) return { label: 'CHƯA CẤU HÌNH', tone: 'idle' };
  const status = normStatus(c.status);
  const active = Number(c.active_leases || 0);
  if (['disabled', 'expired'].includes(status)) return { label: 'SẬP', tone: 'down' };
  if (['auth-required', 'unconfigured'].includes(status)) return { label: 'CẦN AUTH', tone: 'paused' };
  if (['cooldown', 'rate-limited'].includes(status)) return { label: 'TẠM DỪNG', tone: 'paused' };
  if (active > 0) return { label: 'ĐANG CHẠY', tone: 'running' };
  return { label: 'ĐANG RẢNH', tone: 'idle' };
}

function credentialSummary(creds) {
  const geminiState = providerState(creds.find(c => c.domain === 'gemini_web'));
  const colabState = providerState(creds.find(c => c.domain === 'google_colab'));
  return `<div class="credential-summary">
    <div class="credential-summary-title">Trạng thái</div>
    <div class="credential-summary-states">
      <span class="provider-state ${geminiState.tone}"><span class="status-dot ${geminiState.tone}"></span>Gemini <strong>${geminiState.label}</strong></span>
      <span class="provider-state ${colabState.tone}"><span class="status-dot ${colabState.tone}"></span>Colab <strong>${colabState.label}</strong></span>
    </div>
  </div>`;
}

function renderAccounts(accounts = []) {
  const list = $('#account-list');
  if (!list) return;
  if (!accounts.length) {
    const empty = '<div class="empty-state">Chưa có tài khoản Google. Bấm "+ Thêm tài khoản Google" để mở browser và đăng nhập.</div>';
    if (lastGoogleMarkup !== empty) { list.innerHTML = empty; lastGoogleMarkup = empty; }
    return;
  }

  const markup = accounts.map(a => {
    const id = a.identity || {};
    const creds = a.credentials || [];
    const geminiCred = creds.find(c => c.domain === 'gemini_web');
    const colabCred = creds.find(c => c.domain === 'google_colab');
    const targetCred = geminiCred || creds[0] || {};
    const geminiState = providerState(geminiCred);
    const colabState = providerState(colabCred);
    return `
      <article class="account-card unified-account-card">
        <div class="account-card-main">
          <div class="account-primary-info">
            <h3 class="account-email">${esc(id.email || id.id)}</h3>
            <div class="account-badges">
              <span class="provider-state ${geminiState.tone}" title="Gemini: ${geminiState.label}"><span class="status-dot ${geminiState.tone}"></span>Gemini</span>
              <span class="provider-state ${colabState.tone}" title="Colab: ${colabState.label}"><span class="status-dot ${colabState.tone}"></span>Colab</span>
            </div>
          </div>
          <div class="account-actions-group">
            ${targetCred.id ? `<button class="icon-button" data-credential-browser="${esc(targetCred.id)}" title="Mở Chrome Profile">${icon('open')}</button><button class="icon-button" data-credential-refresh="${esc(targetCred.id)}" title="Làm mới qua CDP">${icon('refresh')}</button><button class="icon-button" data-credential-disable="${esc(targetCred.id)}" title="Vô hiệu hóa">${icon('disable')}</button>` : ''}
            <button class="icon-button danger" data-account-delete="${esc(id.id)}" title="Xóa tài khoản">${icon('trash')}</button>
          </div>
        </div>
      </article>
    `;
  }).join('');
  if (markup !== lastGoogleMarkup) { list.innerHTML = markup; lastGoogleMarkup = markup; }
}

const copilotStatusLabels = {
  HEALTHY: 'READY', DEGRADED: 'DEGRADED', RATE_LIMITED: 'RATE LIMITED',
  COOLDOWN: 'COOLDOWN', EXPIRED: 'EXPIRED', AUTH_REQUIRED: 'AUTH REQUIRED',
  DISABLED: 'DISABLED', UNCONFIGURED: 'UNCONFIGURED'
};

function copilotCredentialStatus(account, credential) {
  const global = String(account.identity.global_status || '').toUpperCase();
  const status = String(credential.status || '').toUpperCase();
  if (global === 'DISABLED' || global === 'UNCONFIGURED') return global;
  if (['PARTIAL', 'DEGRADED'].includes(global) && status === 'HEALTHY') return 'DEGRADED';
  return status;
}

let lastTikTokMarkup = '';

let lastDeepSeekMarkup = '';

function renderDeepSeekAccounts(accounts) {
  const list = $('#deepseek-account-list');
  if (!list) return;
  const markup = accounts.length ? accounts.map(a => `
    <article class="account-card unified-account-card">
      <div class="account-primary-info">
        <h3 class="account-email">${esc(a.identity_id || a.id)}</h3>
        <div class="account-badges"><span class="credential-pill ${a.status === 'HEALTHY' ? 'healthy' : 'partial'}">${esc(a.status || 'UNKNOWN')}</span>
          <span class="credential-meta-pill">${esc(a.max_concurrency)} slots</span></div>
      </div>
      <div class="account-actions-group">
        <button class="icon-button" data-deepseek-login="${esc(a.identity_id)}" title="Mở trình duyệt đăng nhập lại">${icon('refresh')}</button>
        <button class="icon-button danger" data-deepseek-delete="${esc(a.identity_id)}" title="Xóa tài khoản DeepSeek">${icon('trash')}</button>
      </div>
    </article>`).join('') : '<div class="empty-state">Chưa có tài khoản DeepSeek. Nhấn "+ Thêm tài khoản DeepSeek" để thêm.</div>';
  if (markup !== lastDeepSeekMarkup) { list.innerHTML = markup; lastDeepSeekMarkup = markup; }
}

async function loadDeepSeekAccounts() {
  const list = $('#deepseek-account-list');
  try {
    const r = await fetchJSON('/admin/api/deepseek/accounts');
    if (!r.ok) throw new Error('Không thể tải tài khoản DeepSeek.');
    const data = await r.json();
    if (!Array.isArray(data.accounts)) throw new Error('Danh sách tài khoản DeepSeek không hợp lệ.');
    renderDeepSeekAccounts(data.accounts);
  } catch (err) {
    if (list) list.textContent = err.message || 'Không thể tải tài khoản DeepSeek.';
  }
}

function renderTikTokAccounts(accounts) {
  const list = $('#tiktok-account-list');
  if (!list) return;
  const markup = accounts.length ? accounts.map(a => `
    <article class="account-card unified-account-card">
      <div class="account-primary-info">
        <h3 class="account-email">${esc(a.identity_id || a.id)}</h3>
        <div class="account-badges"><span class="credential-pill ${a.status === 'HEALTHY' ? 'healthy' : 'partial'}">${esc(a.status || 'UNKNOWN')}</span>
          <span class="credential-meta-pill">${esc(a.max_concurrency)} slots</span></div>
      </div>
      <div class="account-actions-group">
        <button class="icon-button danger" data-tiktok-delete="${esc(a.identity_id)}" title="Xóa tài khoản TikTok">${icon('trash')}</button>
      </div>
    </article>`).join('') : '<div class="empty-state">Chưa có tài khoản TikTok.</div>';
  if (markup !== lastTikTokMarkup) { list.innerHTML = markup; lastTikTokMarkup = markup; }
}

async function loadTikTokAccounts() {
  const list = $('#tiktok-account-list');
  try {
    const r = await fetchJSON('/admin/api/tts/tiktok/accounts');
    if (!r.ok) throw new Error('Không thể tải tài khoản TikTok.');
    const data = await r.json();
    if (!Array.isArray(data.accounts)) throw new Error('Danh sách tài khoản TikTok không hợp lệ.');
    renderTikTokAccounts(data.accounts);
  } catch (err) {
    if (list) list.textContent = err.message || 'Không thể tải tài khoản TikTok.';
  }
}

function renderCopilotAccounts(accounts) {
  const list = $('#copilot-account-list');
  const entries = accounts.filter(a => a && a.identity && a.identity.provider_group === 'copilot');
  if (!list) return;
  const markup = entries.length ? entries.map(a => {
    const creds = (a.credentials || []).filter(c => c && c.domain === 'copilot_ws');
    const c = creds[0];
    const badges = creds.map(credential => {
      const status = copilotCredentialStatus(a, credential);
      const tone = status === 'HEALTHY' ? 'healthy' : ['DEGRADED', 'COOLDOWN', 'RATE_LIMITED'].includes(status) ? 'partial' : 'disabled';
      const role = ['AUTO', 'SESSION', 'WORKER'].includes(credential.pool_role) ? credential.pool_role : '—';
      return `<span class="credential-pill ${tone}">${copilotStatusLabels[status] || 'UNKNOWN'}</span>
        <span class="credential-meta-pill">${esc(credential.max_concurrency)} slots · ${role}</span>`;
    }).join('') || '<span class="credential-pill unconfigured">CHƯA NẠP</span>';
    return `<article class="account-card unified-account-card">
      <div class="account-primary-info">
        <h3 class="account-email">${esc(a.identity.email || '—')}</h3>
        <div class="account-badges">${badges}</div>
      </div>
      <div class="account-actions-group">
        <button class="icon-button" data-copilot-refresh="${esc(a.identity.id)}" title="Mở browser để làm mới token">${icon('refresh')}</button>
        <button class="icon-button danger" data-copilot-delete="${esc(a.identity.id)}" title="Xóa tài khoản Copilot">${icon('trash')}</button>
      </div>
    </article>`;
  }).join('') : '<div class="empty-state">Chưa có tài khoản Copilot. Chọn “Dán Token URL” hoặc “Đăng nhập Browser”.</div>';
  if (markup !== lastCopilotMarkup) { list.innerHTML = markup; lastCopilotMarkup = markup; }
}

async function loadCopilotAccounts() {
  const list = $('#copilot-account-list');
  try {
    const r = await fetchJSON('/admin/api/copilot/accounts');
    if (!r.ok) throw new Error('Không thể tải tài khoản Copilot.');
    const data = await r.json();
    if (!Array.isArray(data.accounts)) throw new Error('Danh sách tài khoản Copilot không hợp lệ.');
    renderCopilotAccounts(data.accounts);
  } catch (err) {
    if (list) {
      const markup = `<div class="empty-state">${esc(err.message || 'Không thể tải tài khoản Copilot.')}</div>`;
      if (markup !== lastCopilotMarkup) { list.innerHTML = markup; lastCopilotMarkup = markup; }
    }
  }
}

function renderModels(models = []) {
  const html = models.map(x => `
    <div style="display:flex;justify-content:space-between;align-items:center;padding:12px;border-bottom:1px solid #e2e8f0">
      <div>
        <strong style="color:#0f172a;font-size:13px">${esc(x.id)}</strong>
        <div class="metric-sub">${esc(x.owned_by)}</div>
      </div>
      <span class="credential-pill ${x.available ? 'healthy' : 'disabled'}">${x.available ? 'READY' : 'OFFLINE'}</span>
    </div>
  `).join('') || '<div class="empty-state">No models configured.</div>';
  if ($('#model-list')) $('#model-list').innerHTML = html;
}

function relativeWhen(value) {
  const time = new Date(value).getTime();
  if (!Number.isFinite(time)) return '—';
  const seconds = Math.max(0, Math.round((Date.now() - time) / 1000));
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}

let currentTrafficChartData = null;
const activeMetrics = new Set(['requests', 'tokens', 'audio', 'duration']);

function applyTrafficMetricVisibility() {
  const toggle = (el, show) => { if (el) el.style.display = show ? '' : 'none'; };
  toggle($('#traffic-polyline'), activeMetrics.has('requests'));
  toggle($('#traffic-polygon'), activeMetrics.has('requests'));
  toggle($('#traffic-polyline-tokens'), activeMetrics.has('tokens'));
  toggle($('#traffic-polygon-tokens'), activeMetrics.has('tokens'));
  toggle($('#traffic-polyline-audio'), activeMetrics.has('audio'));
  toggle($('#traffic-polygon-audio'), activeMetrics.has('audio'));
  toggle($('#traffic-polyline-duration'), activeMetrics.has('duration'));
  toggle($('#traffic-polygon-duration'), activeMetrics.has('duration'));
}

function renderTrafficChart(data) {
  const empty = $('#traffic-chart-empty');
  if (!empty) return;
  const buckets = Array.isArray(data?.buckets) ? data.buckets : [];
  if (!buckets.length) {
    ['#traffic-polyline', '#traffic-polygon', '#traffic-polyline-tokens', '#traffic-polygon-tokens',
     '#traffic-polyline-audio', '#traffic-polygon-audio', '#traffic-polyline-duration', '#traffic-polygon-duration'].forEach(sel => {
      const el = $(sel);
      if (el) el.setAttribute('points', '');
    });
    currentTrafficChartData = null;
    empty.textContent = 'Chưa có request được ghi nhận trong khoảng thời gian này.';
    empty.hidden = false;
    return;
  }

  const interval = Number(data.interval_seconds);
  const start = Number(data.from);
  const end = Number(data.to);
  const bucketsMap = new Map(buckets.map(b => [Number(b.start), b]));

  let timePoints = [];
  if (Number.isFinite(interval) && interval > 0 && Number.isFinite(start) && Number.isFinite(end) && end >= start && (end - start) / interval <= 744) {
    for (let time = start; time <= end; time += interval) {
      timePoints.push(bucketsMap.get(time) || { start: time, requests: 0, tokens: 0, audio_sec: 0, duration_ms: 0 });
    }
  } else {
    timePoints = buckets;
  }

  const width = 600;
  const height = 120;

  const reqVals = timePoints.map(b => Number(b.requests) || 0);
  const tokenVals = timePoints.map(b => Number(b.tokens) || 0);
  const audioVals = timePoints.map(b => Number(b.audio_sec) || 0);
  const durVals = timePoints.map(b => Number(b.duration_ms) || 0);

  const calcGeom = (values, maxVal) => {
    const safeMax = Math.max(1, maxVal);
    const pts = values.map((val, idx) => {
      const x = values.length === 1 ? width / 2 : idx / (values.length - 1) * width;
      const y = height - 12 - (val / safeMax) * (height - 24);
      return { x, y, val };
    });
    const geom = pts.map(p => `${p.x.toFixed(2)},${p.y.toFixed(2)}`).join(' ');
    const areaGeom = `0,${height} ${geom} ${width},${height}`;
    return { line: geom, area: areaGeom, pts };
  };

  const gReq = calcGeom(reqVals, Math.max(1, ...reqVals));
  const gTokens = calcGeom(tokenVals, Math.max(1, ...tokenVals));
  const gAudio = calcGeom(audioVals, Math.max(1, ...audioVals));
  const gDur = calcGeom(durVals, Math.max(1, ...durVals));

  $('#traffic-polyline')?.setAttribute('points', gReq.line);
  $('#traffic-polygon')?.setAttribute('points', gReq.area);

  $('#traffic-polyline-tokens')?.setAttribute('points', gTokens.line);
  $('#traffic-polygon-tokens')?.setAttribute('points', gTokens.area);

  $('#traffic-polyline-audio')?.setAttribute('points', gAudio.line);
  $('#traffic-polygon-audio')?.setAttribute('points', gAudio.area);

  $('#traffic-polyline-duration')?.setAttribute('points', gDur.line);
  $('#traffic-polygon-duration')?.setAttribute('points', gDur.area);

  currentTrafficChartData = {
    timePoints,
    reqPts: gReq.pts,
    tokenPts: gTokens.pts,
    audioPts: gAudio.pts,
    durPts: gDur.pts
  };

  applyTrafficMetricVisibility();
  empty.hidden = true;
}

async function loadTrafficHistory(force = false) {
  if (trafficInFlight) return;
  const now = Date.now();
  if (!force && now - lastTrafficFetch < 25000) return;
  const period = trafficPeriod;
  const empty = $('#traffic-chart-empty');
  trafficInFlight = true;
  try {
    const response = await fetchJSON(`/admin/api/traffic?period=${encodeURIComponent(period)}`);
    if (!response.ok) throw new Error('Không thể tải lịch sử request.');
    const data = await response.json();
    lastTrafficFetch = Date.now();
    if (trafficPeriod !== period) return;
    renderTrafficChart(data);
  } catch (error) {
    if (trafficPeriod !== period || !empty) return;
    if (lastTrafficPoints !== '') {
      $('#traffic-polyline').setAttribute('points', '');
      $('#traffic-polygon').setAttribute('points', '');
      lastTrafficPoints = '';
    }
    empty.textContent = error.message;
    empty.hidden = false;
  } finally {
    trafficInFlight = false;
  }
}

$$('[data-traffic-period]').forEach(button => button.addEventListener('click', () => {
  trafficPeriod = button.dataset.trafficPeriod;
  $$('[data-traffic-period]').forEach(item => {
    const active = item === button;
    item.classList.toggle('active', active);
    item.setAttribute('aria-pressed', String(active));
  });
  loadTrafficHistory(true);
}));

function renderVoiceStats(voices = {}) {
  const container = $('#row2-voice-list');
  if (!container) return;
  const keys = Object.keys(voices);
  if (!keys.length) {
    container.innerHTML = '<div class="metric-sub">Chưa có request tổng hợp giọng nào phát sinh.</div>';
    return;
  }
  container.innerHTML = keys.map(k => `
    <div class="voice-stat-item">
      <span>${icon('voice')} <strong>${esc(k)}</strong></span>
      <span class="voice-count-badge">${voices[k]} req</span>
    </div>
  `).join('');
}

async function refresh() {
  if (!authReady || document.hidden || activeTab() === 'settings') return;
  if (refreshInFlight) { refreshQueued = true; return; }
  refreshInFlight = true;
  if (refreshTimer) clearTimeout(refreshTimer);
  refreshTimer = null;
  try {
    const tab = activeTab();
	    if (tab === 'accounts' && visibleProvider() === 'copilot') {
	      await loadCopilotAccounts();
	      return;
	    }
	    if (tab === 'accounts' && visibleProvider() === 'deepseek') {
	      await loadDeepSeekAccounts();
	      return;
	    }
	    if (tab === 'accounts' && visibleProvider() === 'tiktok') {
      await loadTikTokAccounts();
      return;
    }
    const response = await fetchJSON('/internal/status');
    if (!response.ok) throw new Error('Không tải được trạng thái.');
    const status = await response.json();
    if (!authReady || document.hidden || activeTab() !== tab) return;
    if (tab === 'accounts') {
      renderAccounts(status.accounts || []);
      return;
    }

    const reqTotal = status.requests_total ?? 0;
    const reqPerMin = (status.requests_per_minute ?? 0).toFixed(2);
    const aiTokens = (status.ai_tokens_total ?? 0).toLocaleString();
    
    if ($('#row1-req-total')) $('#row1-req-total').textContent = reqTotal.toLocaleString();
    if ($('#row1-req-min')) $('#row1-req-min').textContent = reqPerMin;
    if ($('#row1-ai-tokens')) $('#row1-ai-tokens').textContent = aiTokens;

    // Cập nhật các chỉ số traffic và bộ đếm theo nhà cung cấp
    const reqPerSec = (status.requests_per_second ?? 0).toFixed(2);
    if ($('#row2-req-sec')) $('#row2-req-sec').textContent = reqPerSec;
    const totalDurationMs = Number(status.total_duration_ms || 0);
    const averageLatencyMs = reqTotal > 0 ? Math.round(totalDurationMs / reqTotal) : 0;
    if ($('#row2-total-duration')) $('#row2-total-duration').textContent = formatDuration(totalDurationMs);
    if ($('#row2-avg-latency')) $('#row2-avg-latency').textContent = `${averageLatencyMs} ms avg`;
    const providerRequests = status.provider_requests || {};
	    const requestCounters = {
	      'status-google-requests': providerRequests.gemini_web,
	      'status-deepseek-requests': providerRequests.deepseek_web,
	      'status-tiktok-requests': providerRequests.tiktok_api,
	      'status-colab-requests': providerRequests.google_colab,
	      'status-copilot-requests': providerRequests.copilot_ws
	    };
    for (const [id, value] of Object.entries(requestCounters)) {
      const counter = document.getElementById(id);
      if (counter) counter.textContent = Number(value || 0).toLocaleString();
    }
    await loadTrafficHistory();
    $('#last-sync').textContent = new Date().toLocaleTimeString();
  } catch (e) {
    // Dashboard remains usable even when a telemetry refresh is temporarily unavailable.
  } finally {
    refreshInFlight = false;
    if (refreshQueued) {
      refreshQueued = false;
      scheduleRefresh(0);
    } else {
      scheduleRefresh();
    }
  }
}

// XỬ LÝ NÚT COPY API ENDPOINT (HÀNG 4)
document.addEventListener('click', e => {
  const btn = e.target.closest('[data-copy-target]');
  if (!btn) return;
  const targetId = btn.dataset.copyTarget;
  const el = document.getElementById(targetId);
  if (!el) return;
  const path = el.textContent.trim();
  const fullUrl = window.location.origin + path;
  navigator.clipboard.writeText(fullUrl).then(() => {
    const originalText = btn.textContent;
    btn.textContent = '✓ Copied!';
    btn.style.background = '#10b981';
    btn.style.color = '#fff';
    setTimeout(() => {
      btn.textContent = originalText;
      btn.style.background = '';
      btn.style.color = '';
    }, 1500);
  }).catch(() => {
    prompt('Copy URL dưới đây:', fullUrl);
  });
});

let googleLoginPoll = null;
const googleBrowserStatus = $('#google-browser-status');

function showGoogleBrowserStatus(message, tone = 'info') {
  if (!googleBrowserStatus) return;
  googleBrowserStatus.hidden = false;
  googleBrowserStatus.className = `browser-flow-status ${tone}`;
  googleBrowserStatus.textContent = message;
}

function stopGoogleLoginPoll() {
  if (googleLoginPoll) {
    clearInterval(googleLoginPoll);
    googleLoginPoll = null;
  }
}

async function pollGoogleLoginStatus() {
  try {
    const r = await fetchJSON('/admin/api/google/auto-login/status');
    const status = await r.json();
    if (status.captured) {
      showGoogleBrowserStatus(status.message || 'Đã kết nối tài khoản Google.', 'success');
      stopGoogleLoginPoll();
      await refresh();
      return;
    }
    if (!status.running) {
      showGoogleBrowserStatus(status.message || 'Trình duyệt đã đóng trước khi bắt được phiên Google.', 'error');
      stopGoogleLoginPoll();
      return;
    }
    showGoogleBrowserStatus(status.message || 'Đang chờ bạn đăng nhập Google trên trình duyệt...', 'info');
  } catch (_) {
    stopGoogleLoginPoll();
  }
}

$('#add-google-account')?.addEventListener('click', async () => {
  const button = $('#add-google-account');
  if (button) button.disabled = true;
  try {
    const r = await fetchJSON('/admin/api/google/auto-login', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({url: 'https://gemini.google.com/app'})
    });
    const data = await r.json().catch(() => ({}));
    if (!r.ok) {
      showGoogleBrowserStatus(data?.error?.message || 'Không thể mở browser trên server.', 'error');
      return;
    }
    showGoogleBrowserStatus('Đã mở browser trên máy chạy server. Hãy đăng nhập Google; hệ thống sẽ tự lưu phiên.', 'info');
    stopGoogleLoginPoll();
    googleLoginPoll = setInterval(pollGoogleLoginStatus, 2000);
    await pollGoogleLoginStatus();
  } catch (err) {
    showGoogleBrowserStatus(err.message || 'Không thể mở browser.', 'error');
  } finally {
    if (button) button.disabled = false;
  }
});

$('#discover-accounts')?.addEventListener('click', async () => {
  const b = $('#discover-accounts');
  b.disabled = true;
  b.textContent = 'Đang quét…';
  try {
    const r = await fetchJSON('/admin/api/google/discover');
    const d = await r.json();
    alert(d.message || `Tìm thấy ${(d.candidates || []).length} tài khoản ứng viên`);
  } catch (err) {
    alert(err.message || 'Quét tài khoản thất bại');
  } finally {
    b.disabled = false;
    b.textContent = '+ Discover';
  }
});

async function loadPiperSettings() {
  const list = $('#piper-voice-settings');
  if (!list) return;
  try {
    const r = await fetchJSON('/admin/api/tts/settings');
    const data = await r.json();
    if (!r.ok) throw new Error(data?.error?.message || 'Không thể đọc cấu hình Piper');
    if ($('#piper-mode-select')) $('#piper-mode-select').value = data.piper_mode || 'auto';
    const voices = data.voices || {};
    const friendlyName = id => {
      if (id === 'ngochuyennew' || id === 'ngoc_huyen') return 'Ngọc Huyền (Tiếng Việt)';
      if (id.includes('female')) return 'Tiếng Thái MMS (Nữ)';
      if (id.includes('male')) return 'Tiếng Thái MMS (Nam)';
      return esc(id.replace('piper:', ''));
    };
    const voiceKeys = Object.keys(voices).filter(id => id !== 'ngoc_huyen').sort();
    list.innerHTML = voiceKeys.map(id => `
      <div class="voice-switch-card">
        <div class="voice-switch-info">
          <span class="voice-switch-name">${friendlyName(id)}</span>
          <span class="voice-switch-tag">${esc(id)}</span>
        </div>
        <label class="voice-toggle" title="Bật/tắt giọng đọc">
          <input type="checkbox" data-piper-voice="${esc(id)}" ${voices[id] ? 'checked' : ''}>
          <span class="toggle-slider"></span>
        </label>
      </div>
    `).join('') || '<div class="empty-state">Chưa phát hiện model Piper.</div>';
  } catch (err) { list.innerHTML = `<div class="empty-state">${esc(err.message)}</div>`; }
}

$('#save-piper-settings')?.addEventListener('click', async () => {
  const mode = $('#piper-mode-select')?.value || 'auto';
  try {
    let r = await fetchJSON('/admin/api/tts/settings', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({piper_mode: mode})});
    if (!r.ok) { const data = await r.json().catch(() => ({})); throw new Error(data?.error?.message || 'Không thể lưu mode Piper'); }
    for (const input of document.querySelectorAll('[data-piper-voice]')) {
      r = await fetchJSON('/admin/api/tts/settings', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({voice_id: input.dataset.piperVoice, enabled: input.checked})});
      if (!r.ok) throw new Error('Không thể lưu voice '+input.dataset.piperVoice);
    }
    alert('Đã lưu cấu hình Piper.');
    await refresh();
  } catch (err) { alert(err.message || 'Không thể lưu cấu hình Piper'); }
});

let apiKeyLoadVersion = 0;
const apiKeyCreateDialog = $('#api-key-create-dialog');
const apiKeyRevealDialog = $('#api-key-reveal-dialog');
const apiKeyCreateForm = $('#api-key-create-form');
let apiKeyCreating = false;
const apiKeyScopeInputs = () => [...document.querySelectorAll('input[name="api-key-scope"]')];
const apiKeyDate = value => {
  if (!value) return '—';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? '—' : date.toLocaleString('vi-VN');
};
const apiKeyError = (element, message) => {
  element.textContent = message || '';
  element.hidden = !message;
};

async function loadAPIKeys() {
  if (!authReady) return;
  const list = $('#api-key-list');
  if (!list) return;
  const version = ++apiKeyLoadVersion;
  try {
    const r = await fetchJSON('/admin/api/apikeys');
    const data = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(data?.error?.message || 'Không thể tải danh sách API keys.');
    if (!Array.isArray(data.keys)) throw new Error('Danh sách API keys không hợp lệ.');
    if (!authReady || version !== apiKeyLoadVersion) return;
    apiKeyError($('#api-key-list-error'), '');
    list.innerHTML = data.keys.length ? data.keys.map(k => {
      const expired = !!k.expires_at && new Date(k.expires_at).getTime() <= Date.now();
      const scopes = Array.isArray(k.scopes) ? k.scopes.join(', ') : '—';
      return `<article class="api-key-row">
        <div class="api-key-info">
          <div class="api-key-heading"><strong>${esc(k.name)}</strong><span class="credential-pill ${expired || !k.active ? 'disabled' : 'healthy'}">${expired ? 'HẾT HẠN' : k.active ? 'ACTIVE' : 'REVOKED'}</span></div>
          <small class="api-key-id">ID: ${esc(k.id)}</small>
          <div class="api-key-meta"><span>Quyền: ${esc(scopes)}</span><span>RPM: ${Number(k.rate_limit_rpm) || 0 ? esc(k.rate_limit_rpm) : 'Không giới hạn'}</span><span>Hết hạn: ${k.expires_at ? apiKeyDate(k.expires_at) : 'Không'}</span><span>Tạo: ${apiKeyDate(k.created_at)}</span><span>Dùng gần nhất: ${apiKeyDate(k.last_used_at)}</span></div>
        </div>
        <div class="api-key-actions">
          ${k.active ? `<button type="button" class="btn secondary" data-api-key-action="revoke" data-api-key-id="${esc(k.id)}" data-api-key-name="${esc(k.name)}">Thu hồi</button>` : `<button type="button" class="btn secondary" data-api-key-action="activate" data-api-key-id="${esc(k.id)}" data-api-key-name="${esc(k.name)}" ${expired ? 'disabled title="Key đã hết hạn"' : ''}>Kích hoạt</button>`}
          <button type="button" class="btn secondary api-key-delete" data-api-key-action="delete" data-api-key-id="${esc(k.id)}" data-api-key-name="${esc(k.name)}">Xóa</button>
        </div>
      </article>`;
    }).join('') : '<div class="empty-state">Chưa có API key nào.</div>';
  } catch (e) {
    if (!authReady || version !== apiKeyLoadVersion) return;
    apiKeyError($('#api-key-list-error'), e.message || 'Không thể tải danh sách API keys.');
    list.innerHTML = '<div class="empty-state">Không thể tải danh sách API keys. Hãy mở lại Settings để thử lại.</div>';
  }
}

$('#create-api-key')?.addEventListener('click', () => {
  if (!authReady) return;
  apiKeyCreateForm.reset();
  apiKeyError($('#api-key-create-error'), '');
  apiKeyCreateDialog.showModal();
  $('#api-key-name').focus();
});
$$('[data-close-api-key-create]').forEach(button => button.addEventListener('click', () => apiKeyCreateDialog.close()));
apiKeyCreateDialog?.addEventListener('close', () => {
  apiKeyCreateForm.reset();
  apiKeyError($('#api-key-create-error'), '');
});
apiKeyScopeInputs().forEach(input => input.addEventListener('change', () => {
  const all = apiKeyScopeInputs().find(item => item.value === 'all');
  if (input === all && input.checked) apiKeyScopeInputs().filter(item => item !== all).forEach(item => { item.checked = false; });
  else if (input !== all && input.checked) all.checked = false;
}));
apiKeyCreateForm?.addEventListener('submit', async event => {
  event.preventDefault();
  if (apiKeyCreating) return;
  const scopes = apiKeyScopeInputs().filter(input => input.checked).map(input => input.value);
  if (!scopes.length) {
    apiKeyError($('#api-key-create-error'), 'Chọn ít nhất một quyền truy cập.');
    return;
  }
  const spec = {
    name: $('#api-key-name').value.trim(),
    scopes,
    expires_in_days: Number($('#api-key-expiry').value),
    rate_limit_rpm: Number($('#api-key-rpm').value)
  };
  if (!spec.name) {
    apiKeyError($('#api-key-create-error'), 'Vui lòng nhập tên key.');
    return;
  }
  apiKeyError($('#api-key-create-error'), '');
  const submit = $('#api-key-create-submit');
  submit.disabled = true;
  apiKeyCreating = true;
  try {
    const r = await fetchJSON('/admin/api/apikeys', { method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(spec) });
    const data = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(data?.error?.message || 'Tạo API key thất bại.');
    if (!authReady) return;
    if (typeof data.key !== 'string' || !data.key) throw new Error('Key đã được tạo nhưng server không trả về giá trị. Hãy xóa và tạo lại.');
    apiKeyCreateDialog.close();
    $('#api-key-secret').textContent = data.key;
    $('#api-key-copy-status').textContent = '';
    apiKeyRevealDialog.showModal();
    loadAPIKeys();
  } catch (err) {
    if (authReady) apiKeyError($('#api-key-create-error'), err.message || 'Tạo API key thất bại.');
  } finally {
    submit.disabled = false;
    apiKeyCreating = false;
  }
});
$$('[data-close-api-key-reveal]').forEach(button => button.addEventListener('click', () => apiKeyRevealDialog.close()));
apiKeyRevealDialog?.addEventListener('close', () => {
  $('#api-key-secret').textContent = '';
  $('#api-key-copy-status').textContent = '';
});
$('#api-key-copy')?.addEventListener('click', async () => {
  const secret = $('#api-key-secret').textContent;
  if (!secret) return;
  try {
    await navigator.clipboard.writeText(secret);
    if (apiKeyRevealDialog.open) $('#api-key-copy-status').textContent = 'Đã sao chép. Lưu key ở nơi an toàn.';
  } catch (_) {
    if (apiKeyRevealDialog.open) $('#api-key-copy-status').textContent = 'Không thể sao chép tự động. Hãy chọn và sao chép key thủ công.';
  }
});
$('#api-key-list')?.addEventListener('click', async event => {
  const button = event.target.closest('button[data-api-key-action]');
  if (!button || button.disabled || !authReady) return;
  const {apiKeyAction: action, apiKeyId: id, apiKeyName: name} = button.dataset;
  const labels = {revoke: 'thu hồi', activate: 'kích hoạt', delete: 'xóa vĩnh viễn'};
  if (!labels[action] || !id || !confirm(`Bạn có chắc muốn ${labels[action]} API key “${name}”?`)) return;
  const method = action === 'delete' ? 'DELETE' : 'POST';
  button.disabled = true;
  apiKeyError($('#api-key-list-error'), '');
  try {
    const r = await fetchJSON(`/admin/api/apikeys/${action}/${encodeURIComponent(id)}`, {method});
    if (!r.ok) {
      const data = await r.json().catch(() => ({}));
      throw new Error(data?.error?.message || `Không thể ${labels[action]} API key (${r.status}).`);
    }
    await loadAPIKeys();
  } catch (err) {
    if (authReady) apiKeyError($('#api-key-list-error'), err.message || `Không thể ${labels[action]} API key.`);
  } finally {
    button.disabled = false;
  }
});

const tiktokModal = $('#add-tiktok-modal');
$('#add-tiktok-account')?.addEventListener('click', () => tiktokModal?.showModal());
$('#close-tiktok-modal')?.addEventListener('click', () => tiktokModal?.close());
$('#cancel-tiktok-modal')?.addEventListener('click', () => tiktokModal?.close());
$('#tiktok-account-list')?.addEventListener('click', async event => {
  const button = event.target.closest('[data-tiktok-delete]');
  const identityID = button?.dataset.tiktokDelete;
  if (!identityID || !confirm('Xóa tài khoản TikTok này và session đã mã hóa?')) return;
  button.disabled = true;
  try {
    const response = await fetchJSON('/admin/api/tts/tiktok/accounts', {
      method: 'DELETE', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({identity_id: identityID})
    });
    const data = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(data?.error?.message || 'Không thể xóa tài khoản TikTok.');
    await loadTikTokAccounts();
  } catch (err) {
    alert(err.message || 'Không thể xóa tài khoản TikTok.');
  } finally {
    button.disabled = false;
  }
});
$('#add-tiktok-form')?.addEventListener('submit', async event => {
  event.preventDefault();
  const email = $('#tiktok-email-input')?.value.trim();
  const session = $('#tiktok-session-input')?.value.trim();
  const concurrency = Number($('#tiktok-concurrency-input')?.value || 2);
  if (!email || !session) { alert('Tên định danh và session ID là bắt buộc.'); return; }
  if (!Number.isInteger(concurrency) || concurrency < 1 || concurrency > 16) {
    alert('Max concurrency phải từ 1 đến 16.');
    return;
  }
	  try {
	    const r = await fetchJSON('/admin/api/tts/tiktok/accounts', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({email, session_id:session, max_concurrency:concurrency})});
	    const data = await r.json().catch(() => ({}));
	    if (!r.ok) { alert(data?.error?.message || 'Không thể lưu session TikTok'); return; }
	    tiktokModal?.close(); $('#add-tiktok-form')?.reset(); alert('Đã lưu session TikTok dạng mã hóa.'); await loadTikTokAccounts();
	  } catch (err) { alert(err.message || 'Không thể lưu session TikTok'); }
	});

	let deepseekLoginPoll = null;
	function showDeepSeekBrowserStatus(message, type = 'info') {
	  const el = $('#deepseek-browser-status');
	  if (!el) return;
	  el.hidden = !message;
	  el.className = `browser-flow-status ${type}`.trim();
	  el.textContent = message || '';
	}
	function stopDeepSeekLoginPoll() {
	  if (deepseekLoginPoll) clearInterval(deepseekLoginPoll);
	  deepseekLoginPoll = null;
	}
	async function pollDeepSeekLoginStatus() {
	  try {
	    const r = await fetchJSON('/admin/api/deepseek/auto-login/status');
	    if (!r.ok) return;
	    const status = await r.json();
	    if (status.message) {
	      showDeepSeekBrowserStatus(status.message, status.captured ? 'success' : status.running ? 'info' : 'error');
	    }
	    if (status.captured || !status.running) {
	      stopDeepSeekLoginPoll();
	      await loadDeepSeekAccounts();
	    }
	  } catch (_) {
	    stopDeepSeekLoginPoll();
	  }
	}

	$('#open-deepseek-browser')?.addEventListener('click', async () => {
	  const button = $('#open-deepseek-browser');
	  if (button) button.disabled = true;
	  try {
	    const r = await fetchJSON('/admin/api/deepseek/auto-login', {
	      method: 'POST',
	      headers: {'Content-Type': 'application/json'},
	      body: '{}'
	    });
	    const data = await r.json().catch(() => ({}));
	    if (!r.ok) {
	      showDeepSeekBrowserStatus(data?.error?.message || 'Không thể mở browser DeepSeek trên server.', 'error');
	      return;
	    }
	    showDeepSeekBrowserStatus('Đã mở Chrome DeepSeek. Hãy đăng nhập trên màn hình desktop/VNC; token sẽ được tự động bắt và lưu vào hệ thống!', 'info');
	    stopDeepSeekLoginPoll();
	    deepseekLoginPoll = setInterval(pollDeepSeekLoginStatus, 2000);
	    await pollDeepSeekLoginStatus();
	  } catch (err) {
	    showDeepSeekBrowserStatus(err.message || 'Không thể mở browser DeepSeek.', 'error');
	  } finally {
	    if (button) button.disabled = false;
	  }
	});

	$('#deepseek-account-list')?.addEventListener('click', async event => {
		  const loginBtn = event.target.closest('[data-deepseek-login]');
		  const identityIDForLogin = loginBtn?.dataset.deepseekLogin;
		  if (identityIDForLogin) {
		    loginBtn.disabled = true;
		    try {
		      const r = await fetchJSON('/admin/api/deepseek/auto-login', {
		        method: 'POST',
		        headers: {'Content-Type': 'application/json'},
		        body: JSON.stringify({id: identityIDForLogin})
		      });
		      const data = await r.json().catch(() => ({}));
		      if (!r.ok) {
		        showDeepSeekBrowserStatus(data?.error?.message || 'Không thể mở browser DeepSeek.', 'error');
		        return;
		      }
		      showDeepSeekBrowserStatus(`Đã mở Chrome đăng nhập cho ${identityIDForLogin}. Đăng nhập trên màn hình desktop/VNC, hệ thống sẽ tự bắt token và đóng cửa sổ!`, 'info');
		      stopDeepSeekLoginPoll();
		      deepseekLoginPoll = setInterval(pollDeepSeekLoginStatus, 2000);
		      await pollDeepSeekLoginStatus();
		    } catch (err) {
		      showDeepSeekBrowserStatus(err.message || 'Lỗi khi mở browser DeepSeek.', 'error');
		    } finally {
		      loginBtn.disabled = false;
		    }
		    return;
		  }

		  const button = event.target.closest('[data-deepseek-delete]');
		  const identityID = button?.dataset.deepseekDelete;
		  if (!identityID || !confirm('Xóa tài khoản DeepSeek này và token đã mã hóa?')) return;
		  button.disabled = true;
		  try {
		    const response = await fetchJSON('/admin/api/deepseek/accounts', {
		      method: 'DELETE', headers: {'Content-Type': 'application/json'},
		      body: JSON.stringify({identity_id: identityID})
		    });
		    const data = await response.json().catch(() => ({}));
		    if (!response.ok) throw new Error(data?.error?.message || 'Không thể xóa tài khoản DeepSeek.');
		    await loadDeepSeekAccounts();
		  } catch (err) {
		    alert(err.message || 'Không thể xóa tài khoản DeepSeek.');
		  } finally {
		    button.disabled = false;
		  }
		});

const copilotModal = $('#add-copilot-modal');
const copilotForm = $('#add-copilot-form');
function closeCopilotModal() { copilotModal?.close(); }
$('#open-copilot-modal-btn')?.addEventListener('click', () => copilotModal?.showModal());
let copilotLoginTimer = null;
function showCopilotBrowserStatus(message, tone = 'info') {
  const el = $('#copilot-browser-status');
  if (!el) return;
  el.hidden = false;
  el.className = `browser-flow-status ${tone}`;
  el.textContent = message;
}
function stopCopilotPolling() {
  if (copilotLoginTimer) clearInterval(copilotLoginTimer);
  copilotLoginTimer = null;
}
async function pollCopilotLogin() {
  try {
    const r = await fetchJSON('/admin/api/copilot/auto-login/status');
    if (!r.ok) throw new Error('Không đọc được trạng thái đăng nhập.');
    const status = await r.json();
    showCopilotBrowserStatus(status.message || 'Đang chờ đăng nhập Microsoft.', status.captured ? 'success' : status.running ? 'info' : 'error');
    $('#stop-copilot-login').hidden = !status.running;
    if (status.captured || !status.running) {
      stopCopilotPolling();
      await loadCopilotAccounts();
    }
  } catch (err) {
    showCopilotBrowserStatus(err.message || 'Không đọc được trạng thái Copilot.', 'error');
    stopCopilotPolling();
  }
}
$('#add-copilot-account')?.addEventListener('click', async () => {
  const button = $('#add-copilot-account');
  button.disabled = true;
  try {
    const r = await fetchJSON('/admin/api/copilot/auto-login', {method: 'POST', headers: {'Content-Type':'application/json'}, body: '{}'});
    const data = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(data?.error?.message || 'Không mở được browser Copilot.');
    showCopilotBrowserStatus('Browser Copilot đã mở trên máy server. Đăng nhập Microsoft trên cửa sổ đó; hệ thống sẽ tự bắt phiên.', 'info');
    $('#stop-copilot-login').hidden = false;
    stopCopilotPolling();
    copilotLoginTimer = setInterval(pollCopilotLogin, 2000);
  } catch (err) {
    showCopilotBrowserStatus(err.message || 'Không mở được browser Copilot.', 'error');
  } finally { button.disabled = false; }
});
$('#copilot-account-list')?.addEventListener('click', async event => {
  const refreshId = event.target.closest('[data-copilot-refresh]')?.dataset.copilotRefresh;
  const deleteId = event.target.closest('[data-copilot-delete]')?.dataset.copilotDelete;
  if (refreshId) {
    try {
      const response = await fetchJSON('/admin/api/copilot/auto-login', {
        method: 'POST', headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({id: refreshId})
      });
      const data = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(data?.error?.message || 'Không mở được browser Copilot.');
      showCopilotBrowserStatus('Browser Copilot đã mở cho tài khoản đã chọn; đăng nhập, hệ thống sẽ tự bắt và lưu token mới.', 'info');
      $('#stop-copilot-login').hidden = false;
      stopCopilotPolling();
      copilotLoginTimer = setInterval(pollCopilotLogin, 2000);
    } catch (err) {
      showCopilotBrowserStatus(err.message || 'Không thể làm mới tài khoản Copilot.', 'error');
    }
  }
  if (deleteId && confirm('Xóa tài khoản Copilot này và token đã mã hóa?')) {
    try {
      const response = await fetchJSON('/admin/api/copilot/delete', {
        method: 'POST', headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({id: deleteId})
      });
      const data = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(data?.error?.message || 'Không xóa được tài khoản Copilot.');
      await loadCopilotAccounts();
    } catch (err) {
      showCopilotBrowserStatus(err.message || 'Không xóa được tài khoản Copilot.', 'error');
    }
  }
});
$('#stop-copilot-login')?.addEventListener('click', async () => {
  stopCopilotPolling();
  try { await fetchJSON('/admin/api/copilot/auto-login/stop', {method:'POST'}); }
  finally { $('#stop-copilot-login').hidden = true; showCopilotBrowserStatus('Đã dừng browser đăng nhập Copilot.'); }
});
$('#close-copilot-modal')?.addEventListener('click', closeCopilotModal);
$('#cancel-copilot-modal')?.addEventListener('click', closeCopilotModal);
copilotModal?.addEventListener('close', () => {
  copilotForm?.reset();
  $('#copilot-token-input').value = '';
  $('#copilot-form-error').hidden = true;
});
copilotForm?.addEventListener('submit', async event => {
  event.preventDefault();
  const error = $('#copilot-form-error');
  error.hidden = true;
  const submit = $('#submit-copilot-account');
  const body = JSON.stringify({
    identity: { email: $('#copilot-email-input').value.trim() },
    credential: {
      token_url: $('#copilot-token-input').value.trim(),
      max_concurrency: Number($('#copilot-concurrency-input').value),
      pool_role: $('#copilot-role-input').value
    }
  });
  // Never keep the token URL in the form after a submission attempt.
  $('#copilot-token-input').value = '';
  submit.disabled = true;
  try {
    const r = await fetchJSON('/admin/api/copilot/add', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body
    });
    if (!r.ok) {
      const data = await r.json().catch(() => ({}));
      throw new Error(data?.error?.message || 'Không thể lưu tài khoản Copilot.');
    }
    closeCopilotModal();
    await loadCopilotAccounts();
  } catch (err) {
    error.textContent = err.message || 'Không thể lưu tài khoản Copilot.';
    error.hidden = false;
  } finally {
    submit.disabled = false;
  }
});

$('#account-list')?.addEventListener('click', async e => {
  const apiCredID = e.target.dataset.credentialGetapi;
  if (apiCredID) {
    try {
      const open = await fetchJSON(`/admin/api/google/credentials/${encodeURIComponent(apiCredID)}/get-api`, { method: 'POST' });
      const opened = await open.json().catch(() => ({}));
      if (!open.ok) { alert(opened?.error?.message || 'Không thể mở AI Studio.'); return; }
      const apiKey = prompt('Đã mở Google AI Studio. Dán API key vừa tạo vào đây để mã hóa và lưu:');
      if (!apiKey) return;
      const save = await fetchJSON(`/admin/api/google/credentials/${encodeURIComponent(apiCredID)}/store-api-key`, {
        method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({api_key: apiKey})
      });
      const saved = await save.json().catch(() => ({}));
      alert(save.ok ? 'API key đã được mã hóa và lưu an toàn.' : (saved?.error?.message || 'Không thể lưu API key.'));
      if (save.ok) await refresh();
    } catch (err) { alert(err.message || 'Lỗi khi lấy API key.'); }
    return;
  }
  const button = e.target.closest('button');
  if (!button) return;
  const browserCredID = button.dataset.credentialBrowser;
  if (browserCredID) {
    try {
      const r = await fetchJSON(`/admin/api/google/credentials/${encodeURIComponent(browserCredID)}/open-profile`, { method: 'POST' });
      const data = await r.json().catch(() => ({}));
      if (!r.ok) {
        alert(data?.error?.message || 'Không thể mở trình duyệt.');
      } else {
        alert(data.message || 'Đã mở trình duyệt để kiểm tra đăng nhập.');
      }
    } catch (err) {
      alert(err.message || 'Lỗi khi gọi mở trình duyệt.');
    }
    return;
  }
  const refreshCredentialID = button.dataset.credentialRefresh;
  if (refreshCredentialID) {
    button.disabled = true;
    try {
      const r = await fetchJSON(`/admin/api/google/credentials/${encodeURIComponent(refreshCredentialID)}/refresh-cdp`, { method: 'POST' });
      const data = await r.json().catch(() => ({}));
      if (!r.ok) alert(data?.error?.message || 'Không thể làm mới phiên Google.');
      else alert('Đã làm mới phiên Google qua browser profile.');
      await refresh();
    } catch (err) { alert(err.message || 'Làm mới phiên thất bại'); }
    finally { button.disabled = false; }
    return;
  }
  const credentialID = button.dataset.credentialValidate;
  if (credentialID) {
    button.disabled = true;
    try {
      const r = await fetchJSON(`/admin/api/google/credentials/${encodeURIComponent(credentialID)}/validate`, { method: 'POST' });
      const data = await r.json().catch(() => ({}));
      if (!r.ok) alert(data?.error?.message || 'Cookie Gemini không hợp lệ hoặc đã hết hạn.');
      else alert('Cookie Gemini hợp lệ và tài khoản đã sẵn sàng.');
      await refresh();
    } catch (err) { alert(err.message || 'Xác thực thất bại'); }
    finally { button.disabled = false; }
    return;
  }
  const credentialDeleteID = button.dataset.credentialDelete;
  if (credentialDeleteID && confirm('Xóa credential này khỏi tài khoản?')) {
    const r = await fetchJSON(`/admin/api/google/credentials/${encodeURIComponent(credentialDeleteID)}`, { method: 'DELETE' });
    if (!r.ok) { const data = await r.json().catch(() => ({})); alert(data?.error?.message || 'Xóa credential thất bại'); }
    else await refresh();
    return;
  }
  const accountDeleteID = button.dataset.accountDelete;
  if (accountDeleteID && confirm('Xóa toàn bộ tài khoản Google và credentials liên quan?')) {
    const r = await fetchJSON(`/admin/api/google/accounts/${encodeURIComponent(accountDeleteID)}`, { method: 'DELETE' });
    if (!r.ok) { const data = await r.json().catch(() => ({})); alert(data?.error?.message || 'Xóa tài khoản thất bại'); }
    else await refresh();
    return;
  }
  const disableCredentialID = button.dataset.credentialDisable;
  if (disableCredentialID && confirm('Bạn có chắc muốn vô hiệu hóa credential này?')) {
    try {
      const r = await fetchJSON(`/admin/api/google/credentials/${encodeURIComponent(disableCredentialID)}/disable`, { method: 'POST' });
      if (!r.ok) alert('Vô hiệu hóa thất bại');
      else await refresh();
    } catch (_) {}
  }
});

let lastEnteredPassword = '';

$('#admin-login-form')?.addEventListener('submit', async e => {
  e.preventDefault();
  const password = $('#admin-password').value;
  lastEnteredPassword = password;
  const errorEl = $('#login-error');
  if (errorEl) errorEl.textContent = '';
  try {
    await adminLogin(password);
  } catch (err) {
    if (errorEl) errorEl.textContent = err.message || 'Đăng nhập thất bại.';
  }
});

$('#change-password-choice')?.addEventListener('click', () => {
  authView('auth-change-view');
});

$('#skip-password-choice')?.addEventListener('click', async () => {
  try {
    await fetch('/admin/api/first-login/dismiss', { method: 'POST', credentials: 'same-origin' });
  } catch (_) {}
  lastEnteredPassword = '';
  finishAuth();
});

$('#change-password-form')?.addEventListener('submit', async e => {
  e.preventDefault();
  const newPass = $('#new-admin-password').value;
  const confirmPass = $('#confirm-admin-password').value;
  const errorEl = $('#password-error');
  if (errorEl) errorEl.textContent = '';

  if (newPass !== confirmPass) {
    if (errorEl) errorEl.textContent = 'Mật khẩu nhập lại không khớp.';
    return;
  }
  if (newPass.length < 6) {
    if (errorEl) errorEl.textContent = 'Mật khẩu phải có ít nhất 6 ký tự.';
    return;
  }
  try {
    await changeAdminPassword(lastEnteredPassword || '123456', newPass);
    lastEnteredPassword = '';
    alert('Đổi mật khẩu thành công!');
    finishAuth();
  } catch (err) {
    if (errorEl) errorEl.textContent = err.message || 'Đổi mật khẩu thất bại.';
  }
});

$('#logout-btn')?.addEventListener('click', async () => {
  try {
    await fetch('/admin/api/logout', { method: 'POST', credentials: 'same-origin' });
  } catch (_) {}
  if (refreshTimer) {
    clearTimeout(refreshTimer);
    refreshTimer = null;
  }
  showAuth('Bạn đã đăng xuất.');
});

document.addEventListener('visibilitychange', () => {
  if (document.hidden) {
    if (refreshTimer) clearTimeout(refreshTimer);
    refreshTimer = null;
  } else if (authReady && activeTab() !== 'settings') {
    scheduleRefresh(0);
  }
});


let workerLoadVersion = 0;
let workerSnapshot = null;
let workerBusy = false;
const workerSafeText = value => {
  const text = String(value ?? '');
  return /[\/]|token|secret|cookie|password|bearer|api[_-]?key/i.test(text) ? '—' : esc(text);
};
const workerCount = value => Number.isFinite(Number(value)) && Number(value) >= 0 ? Math.floor(Number(value)) : 0;
const workerDate = value => {
  if (!value) return '—';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? '—' : esc(date.toLocaleString('vi-VN'));
};
function workerStatus(message, error = false) {
  const element = $('#worker-action-status');
  element.textContent = message;
  element.hidden = !message;
  element.classList.toggle('error', error);
}
function workerList(workers, empty) {
  if (!Array.isArray(workers) || !workers.length) return `<div class="empty-state">${empty}</div>`;
  return workers.filter(w => w && typeof w === 'object').map(w => {
    const id = typeof w.worker_id === 'string' ? w.worker_id : '';
    const active = workerCount(w.active);
    const idle = typeof w.active === 'number' && Number.isInteger(w.active) && w.active === 0;
    const models = Array.isArray(w.models) ? w.models.filter(m => typeof m === 'string').map(workerSafeText).join(', ') : '';
    return `<div class="worker-item"><div class="worker-item-heading"><strong>${workerSafeText(id)}</strong>
      <button type="button" class="btn secondary" data-worker-disconnect="${esc(id)}" ${!idle || !id || workerBusy ? 'disabled' : ''}>Ngắt kết nối</button></div>
      <div class="worker-meta"><span>Đang chạy: ${active}/${workerCount(w.max_concurrency)}</span><span>Thấy lần cuối: ${workerDate(w.last_seen)}</span><span>Kết nối: ${workerDate(w.connected_at)}</span></div>
      <div class="worker-models">Models: ${models || '—'}</div></div>`;
  }).join('');
}
function renderWorkers(data) {
  const local = data.piper_local && typeof data.piper_local === 'object' ? data.piper_local : {};
  const voices = Array.isArray(local.voices) ? local.voices.filter(v => typeof v === 'string').map(workerSafeText).join(', ') : '';
  $('#worker-piper-local').innerHTML = `<div class="worker-item"><strong>${local.ready === true ? 'Sẵn sàng' : 'Chưa sẵn sàng'}</strong><div class="worker-models">Voices: ${voices || '—'}</div></div>`;
  $('#worker-piper-remote').innerHTML = workerList(data.piper_remote, 'Chưa có remote Piper worker.');
  $('#worker-colab').innerHTML = workerList(data.colab_workers, 'Chưa có Colab worker.');
  const accounts = Array.isArray(data.colab_accounts) ? data.colab_accounts.filter(a => a && typeof a === 'object') : [];
  $('#worker-accounts').innerHTML = accounts.length ? accounts.map(a => `<div class="worker-item"><div class="worker-item-heading"><strong>${workerSafeText(a.email || a.id)}</strong><span>${workerSafeText(a.status)}</span></div><div class="worker-meta"><span>Cooldown đến: ${workerDate(a.cooldown_until)}</span></div></div>`).join('') : '<div class="empty-state">Chưa có supervisor account.</div>';
  const select = $('#worker-account-select');
  const selected = select.value;
  select.innerHTML = '<option value="">Chọn tài khoản</option>' + accounts.filter(a => typeof a.id === 'string' && a.id).map(a => `<option value="${esc(a.id)}">${workerSafeText(a.email || a.id)}</option>`).join('');
  if (accounts.some(a => a.id === selected)) select.value = selected;
  const available = data.colab_supervisor_available === true && !workerBusy;
  select.disabled = !available || !accounts.length;
  $('#start-colab-worker').disabled = !available || !select.value;
  $('#rotate-colab-worker').disabled = !available;
  $('#worker-tiktok-slots').innerHTML = `<div class="worker-item"><strong>${workerCount(data.tiktok_async_slots)}</strong> slot bất đồng bộ</div>`;
  if ($('#wsum-local')) $('#wsum-local').textContent = local.ready === true ? 'Sẵn sàng' : 'Chưa sẵn sàng';
  if ($('#wsum-piper')) $('#wsum-piper').textContent = (Array.isArray(data.piper_remote) ? data.piper_remote.length : 0);
  if ($('#wsum-colab')) $('#wsum-colab').textContent = (Array.isArray(data.colab_workers) ? data.colab_workers.length : 0);
  if ($('#wsum-tiktok')) $('#wsum-tiktok').textContent = workerCount(data.tiktok_async_slots);
}
async function loadWorkers() {
  if (!authReady) return;
  const version = ++workerLoadVersion;
  const button = $('#refresh-workers');
  button.disabled = true;
  try {
    const response = await fetchJSON('/admin/api/workers');
    if (!response.ok) throw new Error('Không thể tải trạng thái worker.');
    const data = await response.json();
    if (!authReady || version !== workerLoadVersion) return;
    if (!data || typeof data !== 'object') throw new Error('Dữ liệu worker không hợp lệ.');
    workerSnapshot = data;
    $('#worker-load-error').hidden = true;
    renderWorkers(data);
  } catch (_) {
    if (!authReady || version !== workerLoadVersion) return;
    $('#worker-load-error').textContent = 'Không thể tải trạng thái worker. Hãy thử làm mới.';
    $('#worker-load-error').hidden = false;
  } finally {
    if (version === workerLoadVersion) button.disabled = false;
  }
}
$('#refresh-workers')?.addEventListener('click', loadWorkers);
$('#worker-account-select')?.addEventListener('change', () => {
  $('#start-colab-worker').disabled = !workerSnapshot?.colab_supervisor_available || !$('#worker-account-select').value || workerBusy;
});
$('#worker-groups')?.addEventListener('click', async event => {
  const button = event.target.closest('button[data-worker-disconnect]');
  if (!button || button.disabled || !authReady || workerBusy) return;
  const id = button.dataset.workerDisconnect;
  if (!id) return;
  workerBusy = true;
  button.disabled = true;
  workerStatus('');
  try {
    const check = await fetchJSON('/admin/api/workers');
    if (!check.ok) throw new Error('Không thể xác nhận trạng thái worker.');
    const latest = await check.json();
    if (!authReady) return;
    const workers = [...(Array.isArray(latest.piper_remote) ? latest.piper_remote : []), ...(Array.isArray(latest.colab_workers) ? latest.colab_workers : [])];
    const entry = workers.find(w => w?.worker_id === id);
    if (!entry || typeof entry.active !== 'number' || entry.active !== 0) {
      workerStatus('Worker không còn rảnh hoặc đã ngắt. Hãy làm mới.', true);
      await loadWorkers();
      return;
    }
    if (!confirm(`Ngắt kết nối worker đang rảnh “${id}”?`)) return;
    const response = await fetchJSON(`/admin/api/workers/${encodeURIComponent(id)}/disconnect`, {method:'POST'});
    if (!response.ok) throw new Error('Không thể ngắt kết nối worker.');
    workerStatus('Đã gửi lệnh ngắt kết nối worker.');
    await loadWorkers();
  } catch (_) {
    if (authReady) workerStatus('Không thể ngắt kết nối worker. Hãy làm mới và thử lại.', true);
  } finally {
    workerBusy = false;
    if (authReady && workerSnapshot) renderWorkers(workerSnapshot);
  }
});
async function colabCommand(action) {
  if (!authReady || workerBusy || workerSnapshot?.colab_supervisor_available !== true) return;
  const accountId = $('#worker-account-select').value;
  if (action === 'start' && (!accountId || !Array.isArray(workerSnapshot.colab_accounts) || !workerSnapshot.colab_accounts.some(a => a?.id === accountId))) return;
  workerBusy = true;
  workerStatus('');
  renderWorkers(workerSnapshot);
  try {
    const response = await fetchJSON(`/admin/api/workers/colab/${action}`, {
      method:'POST', ...(action === 'start' ? {headers:{'Content-Type':'application/json'}, body:JSON.stringify({account_id:accountId})} : {})
    });
    if (!response.ok) throw new Error('Không thể thực hiện lệnh Colab.');
    if (authReady) workerStatus('command executed; connection not guaranteed');
    await loadWorkers();
  } catch (_) {
    if (authReady) workerStatus('Không thể thực hiện lệnh Colab. Hãy thử lại.', true);
  } finally {
    workerBusy = false;
    if (authReady && workerSnapshot) renderWorkers(workerSnapshot);
  }
}
$('#start-colab-worker')?.addEventListener('click', () => colabCommand('start'));
$('#rotate-colab-worker')?.addEventListener('click', () => colabCommand('rotate'));

initTabs();
checkAdminAuth();


// TRAFFIC CHART INTERACTION (HOVER & LEGEND)
(function() {
  const shell = document.getElementById('traffic-chart-shell');
  const tooltip = document.getElementById('traffic-tooltip');
  const crosshair = document.getElementById('traffic-crosshair');
  const dotReq = document.getElementById('traffic-dot-req');
  const dotTokens = document.getElementById('traffic-dot-tokens');
  const dotAudio = document.getElementById('traffic-dot-audio');
  const dotDur = document.getElementById('traffic-dot-duration');

  if (shell && tooltip) {
    shell.addEventListener('mousemove', e => {
      if (!currentTrafficChartData || !currentTrafficChartData.timePoints || !currentTrafficChartData.timePoints.length) return;
      const rect = shell.getBoundingClientRect();
      const mouseX = e.clientX - rect.left;
      const ratio = Math.max(0, Math.min(1, mouseX / rect.width));
      const total = currentTrafficChartData.timePoints.length;
      const idx = Math.min(total - 1, Math.max(0, Math.round(ratio * (total - 1))));
      const b = currentTrafficChartData.timePoints[idx];
      if (!b) return;

      const ptX = total === 1 ? 300 : idx / (total - 1) * 600;
      if (crosshair) {
        crosshair.setAttribute('x1', ptX.toFixed(2));
        crosshair.setAttribute('x2', ptX.toFixed(2));
        crosshair.removeAttribute('hidden');
      }

      const updateDot = (dot, pts, metric) => {
        if (!dot) return;
        if (activeMetrics.has(metric) && pts && pts[idx]) {
          dot.setAttribute('cx', ptX.toFixed(2));
          dot.setAttribute('cy', pts[idx].y.toFixed(2));
          dot.removeAttribute('hidden');
        } else {
          dot.setAttribute('hidden', '');
        }
      };

      updateDot(dotReq, currentTrafficChartData.reqPts, 'requests');
      updateDot(dotTokens, currentTrafficChartData.tokenPts, 'tokens');
      updateDot(dotAudio, currentTrafficChartData.audioPts, 'audio');
      updateDot(dotDur, currentTrafficChartData.durPts, 'duration');

      const d = new Date(b.start * 1000);
      const timeStr = `${String(d.getHours()).padStart(2,'0')}:${String(d.getMinutes()).padStart(2,'0')} - ${d.getDate()}/${d.getMonth()+1}`;
      const reqVal = Number(b.requests || 0).toLocaleString();
      const tokenVal = Number(b.tokens || 0).toLocaleString();
      const audioVal = `${Number(b.audio_sec || 0).toLocaleString()}s`;
      const durVal = b.requests ? `${Math.round(b.duration_ms / b.requests)}ms` : `${Number(b.duration_ms || 0)}ms`;

      tooltip.innerHTML = `
        <div class="traffic-tooltip-time">🕒 ${timeStr}</div>
        <div class="traffic-tooltip-row"><span class="label"><span class="legend-dot dot-req"></span>Requests:</span><strong>${reqVal}</strong></div>
        <div class="traffic-tooltip-row"><span class="label"><span class="legend-dot dot-tokens"></span>Tokens AI:</span><strong>${tokenVal}</strong></div>
        <div class="traffic-tooltip-row"><span class="label"><span class="legend-dot dot-audio"></span>Tổng tg Audio:</span><strong>${audioVal}</strong></div>
        <div class="traffic-tooltip-row"><span class="label"><span class="legend-dot dot-duration"></span>TG xử lý tb:</span><strong>${durVal}</strong></div>
      `;
      tooltip.hidden = false;

      let tipLeft = mouseX;
      if (tipLeft < 90) tipLeft = 90;
      if (tipLeft > rect.width - 90) tipLeft = rect.width - 90;
      tooltip.style.left = `${tipLeft}px`;
      tooltip.style.top = `${Math.max(25, e.clientY - rect.top)}px`;
    });

    shell.addEventListener('mouseleave', () => {
      if (crosshair) crosshair.setAttribute('hidden', '');
      if (dotReq) dotReq.setAttribute('hidden', '');
      if (dotTokens) dotTokens.setAttribute('hidden', '');
      if (dotAudio) dotAudio.setAttribute('hidden', '');
      if (dotDur) dotDur.setAttribute('hidden', '');
      tooltip.hidden = true;
    });
  }

  document.getElementById('traffic-chart-legend')?.addEventListener('click', e => {
    const btn = e.target.closest('[data-metric]');
    if (!btn) return;
    const metric = btn.dataset.metric;
    if (activeMetrics.has(metric)) {
      if (activeMetrics.size > 1) {
        activeMetrics.delete(metric);
        btn.classList.remove('active');
      }
    } else {
      activeMetrics.add(metric);
      btn.classList.add('active');
    }
    applyTrafficMetricVisibility();
  });
})();

// SETTINGS FILTER BAR & ADMIN PASSWORD
(function() {
  document.querySelectorAll('.settings-filter-btn').forEach(btn => {
    btn.addEventListener('click', () => {
      document.querySelectorAll('.settings-filter-btn').forEach(b => b.classList.toggle('active', b === btn));
      const filter = btn.dataset.settingsFilter;
      document.querySelectorAll('[data-settings-section]').forEach(sec => {
        sec.hidden = (filter !== 'all' && sec.dataset.settingsSection !== filter);
      });
    });
  });

  const changePasswordModal = document.getElementById('change-password-modal');
  document.getElementById('open-change-password-modal')?.addEventListener('click', () => {
    document.getElementById('settings-password-form')?.reset();
    const msg = document.getElementById('settings-pass-msg');
    if (msg) msg.hidden = true;
    changePasswordModal?.showModal();
  });
  document.getElementById('close-password-modal')?.addEventListener('click', () => changePasswordModal?.close());
  document.getElementById('cancel-password-modal')?.addEventListener('click', () => changePasswordModal?.close());

  const passForm = document.getElementById('settings-password-form');
  if (passForm) {
    passForm.addEventListener('submit', async e => {
      e.preventDefault();
      const curr = document.getElementById('settings-curr-pass')?.value || '';
      const newPass = document.getElementById('settings-new-pass')?.value || '';
      const confirmPass = document.getElementById('settings-confirm-pass')?.value || '';
      const msg = document.getElementById('settings-pass-msg');
      const btn = document.getElementById('settings-pass-btn');
      if (!msg || !btn) return;
      if (newPass !== confirmPass) {
        msg.className = 'settings-pass-msg error';
        msg.textContent = 'Mật khẩu mới và xác nhận mật khẩu không khớp.';
        msg.hidden = false;
        return;
      }
      btn.disabled = true;
      msg.hidden = true;
      try {
        const r = await fetchJSON('/admin/api/change-password', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ current_password: curr, new_password: newPass })
        });
        const d = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(d?.error?.message || 'Không thể cập nhật mật khẩu');
        msg.className = 'settings-pass-msg success';
        msg.textContent = 'Đã cập nhật mật khẩu Admin thành công!';
        msg.hidden = false;
        passForm.reset();
        setTimeout(() => { changePasswordModal?.close(); }, 1200);
      } catch (err) {
        msg.className = 'settings-pass-msg error';
        msg.textContent = err.message || 'Lỗi khi đổi mật khẩu.';
        msg.hidden = false;
      } finally {
        btn.disabled = false;
      }
    });
  }
})();
