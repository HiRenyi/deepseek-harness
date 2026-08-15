/**
 * Browser MCP — Popup Panel Logic
 *
 * Handles: status display, connection control, MCP config copy,
 * CDP security toggles, quick tools, tab group status.
 */

// Profile is loaded via popup.html <script src="profile.js"> before this file.
// window.__BMCP_PROFILE supplies the env-specific NM host name / port / URLs /
// server name / tab-group title (test vs prod differ). Fall back to a sane
// default if the script tag is missing (e.g. popup opened standalone).
const P = (window.__BMCP_PROFILE || {
  NM_HOST_NAME: 'com.browser.mcp',
  MCP_PORT: 58080,
  HEALTH_URL: 'http://127.0.0.1:58080/health',
  EXT_INFO_URL: 'http://127.0.0.1:58080/api/extension-info',
  MCP_SERVER_NAME: 'browser-mcp',
  TAB_GROUP_TITLE: '🤖 Browser MCP',
});
const MCP_BASE = `http://127.0.0.1:${P.MCP_PORT}`;
const STATUS_URL = `${MCP_BASE}/api/status`;

// ---------------------------------------------------------------------------
// DOM references
// ---------------------------------------------------------------------------

const $ = (id) => document.getElementById(id);

const statusLed       = $('status-led');
const statusText      = $('status-text');
const statusDetail    = $('status-detail');
const btnDisconnect   = $('btn-disconnect');
const btnReconnect    = $('btn-reconnect');
const btnEditor       = $('btn-editor');
const btnAnnotation   = $('btn-annotation');
const toggleRawCdp    = $('toggle-raw-cdp');
const toggleEvalJs    = $('toggle-eval-js');
const toggleFetchDomain = $('toggle-fetch-domain');
const configSection   = $('config-section');
const configTabs      = $('config-tabs');
let   currentFormat   = 'http';   // 'http' | 'sse' — tab-switched (no dropdown)
const configJson      = $('config-json');
const btnCopyConfig   = $('btn-copy-config');
const copyFeedback    = $('copy-feedback');
const tabgroupSection = $('tabgroup-section');
const tabgroupInfo    = $('tabgroup-info');
const inputIdleTimeout = $('input-idle-timeout');
const btnSaveIdle      = $('btn-save-idle');

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

let currentStatus = null;
let editorActive = false;
let annotationActive = false;
// reconnect-clean-states: while true, the STATUS_CHANGED listener is suppressed
// so SW broadcasts (nm-host up but bridge not yet, etc.) don't flash intermediate
// disconnected states. Only the reconnect poll renders during this window —
// steady "重连中" until connected or 5s timeout.
let reconnecting = false;

// ---------------------------------------------------------------------------
// Status rendering
// ---------------------------------------------------------------------------

function render(status) {
  currentStatus = status;

  // Bridge version (git describe) in footer — runtime dev-build confirmation.
  const bv = $('bridge-ver');
  if (bv) bv.textContent = status?.bridgeVersion ? `bridge ${status.bridgeVersion}` : 'bridge —';

  const nmConnected = status?.nmConnected ?? false;
  const connecting = status?.connecting ?? false;
  const healthOk = status?.healthStatus === 'ok';

  // LED
  statusLed.className = 'led';
  if (connecting) {
    statusLed.classList.add('led-yellow');
  } else if (nmConnected && healthOk) {
    statusLed.classList.add('led-green');
  } else if (nmConnected && !healthOk) {
    statusLed.classList.add('led-yellow');
  } else if (!nmConnected) {
    statusLed.classList.add('led-red');
  } else {
    statusLed.classList.add('led-gray');
  }

  // Status text
  const transportLabel = ({ pipe: 'Named Pipe', unix: 'Unix Socket', tcp: 'TCP Token' })[status?.transportType] || (status?.transportType || '—');
  if (connecting) {
    statusText.textContent = '● 连接中…';
    statusText.className = 'status-text partial';
    statusDetail.textContent = '正在连接 nm-host';
  } else if (nmConnected && healthOk) {
    statusText.textContent = '● 已连接';
    statusText.className = 'status-text connected';
    statusDetail.textContent = `连接方式: ${transportLabel} | 端口: ${status.mcpPort || P.MCP_PORT} | MCP: ${(status.transports || []).join(' + ')}`;
  } else if (nmConnected) {
    statusText.textContent = '● 部分连接';
    statusText.className = 'status-text partial';
    statusDetail.textContent = 'NM 已连接，MCP 服务未响应';
  } else {
    statusText.textContent = '● 已断开';
    statusText.className = 'status-text disconnected';
    statusDetail.textContent = status?.reconnectFailed
      ? '重连失败：bridge 未能在 5s 内启动，请检查 bridge 是否已安装或手动启动'
      : status?.userDisconnected
        ? '用户主动断开 — 点「重连」恢复（断开期间不会自动重连/拉起 bridge）'
        : 'Bridge 未运行或扩展未连接';
  }

  // Buttons
  btnDisconnect.disabled = !nmConnected || connecting;
  btnReconnect.disabled = connecting;

  // CDP permissions
  if (status?.cdpPermissions) {
    toggleRawCdp.checked = status.cdpPermissions.allow_raw_cdp ?? true;
    toggleEvalJs.checked = status.cdpPermissions.allow_eval_js ?? true;
    toggleFetchDomain.checked = status.cdpPermissions.allow_fetch_domain ?? true;
  }
  renderToggleStatus();

  // Idle timeout echo (ms → seconds)
  const idleSec = Math.round((status?.idleTimeoutMs ?? 120000) / 1000);
  inputIdleTimeout.value = String(idleSec);

  // MCP config — always visible (no longer gated on connection state)
  renderConfig();

  // Tab group
  if (status?.tabGroup?.id != null) {
    tabgroupSection.style.display = '';
    tabgroupInfo.textContent = `标签组: ${status.tabGroup.title || P.TAB_GROUP_TITLE}`;
  } else {
    tabgroupSection.style.display = 'none';
  }
}

function renderToggleStatus() {
  $('toggle-raw-cdp-status').textContent = toggleRawCdp.checked ? '开' : '关';
  $('toggle-eval-js-status').textContent = toggleEvalJs.checked ? '开' : '关';
  $('toggle-fetch-domain-status').textContent = toggleFetchDomain.checked ? '开' : '关';
}

// ---------------------------------------------------------------------------
// MCP config
// ---------------------------------------------------------------------------

function getConfigJson(format) {
  if (format === 'sse') {
    return JSON.stringify({
      mcpServers: {
        [P.MCP_SERVER_NAME]: {
          type: 'sse',
          url: `${MCP_BASE}/sse`
        }
      }
    }, null, 2);
  }
  return JSON.stringify({
    mcpServers: {
      [P.MCP_SERVER_NAME]: {
        type: 'http',
        url: `${MCP_BASE}/mcp`
      }
    }
  }, null, 2);
}

function renderConfig() {
  configJson.textContent = getConfigJson(currentFormat);
}

// ---------------------------------------------------------------------------
// Event handlers
// ---------------------------------------------------------------------------

// Connection
btnDisconnect.addEventListener('click', () => {
  chrome.runtime.sendMessage({ type: 'DISCONNECT' }, (resp) => {
    if (resp?.success) queryStatus();
  });
});

// P3.6: dashboard entry removed (web dashboard page deleted). Button no longer
// in popup.html — guard null in case an older cached popup still references it.
const btnDashboard = $('btn-dashboard');
if (btnDashboard) {
  btnDashboard.addEventListener('click', () => {
    chrome.tabs.create({ url: P.HEALTH_URL });
  });
}

btnReconnect.addEventListener('click', () => {
  // reconnect-clean-states: strict 3-state machine — 已断开 → 重连中 → 已连接
  // (or 已断开·bridge无法连接 on 5s timeout). During the 5s window the
  // STATUS_CHANGED listener is suppressed (reconnecting=true) so SW broadcasts
  // (nm-host up but bridge not yet, etc.) can't flash intermediate disconnected
  // states. Only this poll renders — steady "重连中" the whole window.
  reconnecting = true;
  render({ nmConnected: false, connecting: true }); // immediate "重连中"
  chrome.runtime.sendMessage({ type: 'RECONNECT' }, (resp) => {
    if (!resp?.success) {
      // RECONNECT rejected — drop to ground truth (no synthetic state).
      reconnecting = false;
      queryStatus();
      return;
    }
    const DEADLINE_MS = 5000;
    const POLL_INTERVAL_MS = 500;
    const start = Date.now();
    const poll = async () => {
      // Still inside the 5s window?
      if (Date.now() - start >= DEADLINE_MS) {
        reconnecting = false;
        render({ nmConnected: false, reconnectFailed: true }); // 已断开 · bridge无法连接
        return;
      }
      const st = await fetchStatus();
      if (st?.nmConnected) {
        reconnecting = false;
        render(st); // 已连接
        return;
      }
      // Keep steady "重连中" — never render the intermediate disconnected state.
      render({ nmConnected: false, connecting: true });
      setTimeout(poll, POLL_INTERVAL_MS);
    };
    setTimeout(poll, POLL_INTERVAL_MS);
  });
});

// Quick tools
btnEditor.addEventListener('click', () => {
  chrome.runtime.sendMessage({ type: 'TOGGLE_EDITOR' }, (resp) => {
    if (resp?.success) {
      editorActive = resp.active;
      btnEditor.classList.toggle('active', editorActive);
    }
  });
});

btnAnnotation.addEventListener('click', () => {
  chrome.runtime.sendMessage({ type: 'TOGGLE_ANNOTATION' }, (resp) => {
    if (resp?.success) {
      annotationActive = resp.active;
      btnAnnotation.classList.toggle('active', annotationActive);
    }
  });
});

// CDP security toggles
function handleCdpToggle(key, checkbox) {
  chrome.runtime.sendMessage({
    type: 'SET_CDP_PERMISSION',
    key: key,
    value: checkbox.checked
  }, (resp) => {
    if (!resp?.success) {
      // Revert on failure
      checkbox.checked = !checkbox.checked;
      renderToggleStatus();
    }
  });
}

toggleRawCdp.addEventListener('change', () => {
  handleCdpToggle('allow_raw_cdp', toggleRawCdp);
  renderToggleStatus();
});

toggleEvalJs.addEventListener('change', () => {
  handleCdpToggle('allow_eval_js', toggleEvalJs);
  renderToggleStatus();
});

toggleFetchDomain.addEventListener('change', () => {
  handleCdpToggle('allow_fetch_domain', toggleFetchDomain);
  renderToggleStatus();
});

// Idle timeout save
btnSaveIdle.addEventListener('click', () => {
  const sec = Number(inputIdleTimeout.value);
  if (!Number.isFinite(sec) || sec < 60) {
    alert('空闲灭灯秒数最小 60(chrome.alarms 下限)');
    return;
  }
  chrome.runtime.sendMessage({ type: 'SET_IDLE_TIMEOUT', value: Math.round(sec * 1000) }, (resp) => {
    if (!resp || !resp.success) {
      alert(resp?.error || '保存失败');
      return;
    }
    btnSaveIdle.textContent = '已保存';
    setTimeout(() => { btnSaveIdle.textContent = '保存'; }, 1200);
  });
});

// MCP config format — tab switch (HTTP / SSE)
if (configTabs) {
  configTabs.addEventListener('click', (ev) => {
    const t = ev.target.closest('.config-tab');
    if (!t) return;
    currentFormat = t.dataset.fmt;
    configTabs.querySelectorAll('.config-tab').forEach(b => b.classList.toggle('active', b === t));
    renderConfig();
  });
}

// Copy
btnCopyConfig.addEventListener('click', async () => {
  const text = getConfigJson(currentFormat);
  try {
    await navigator.clipboard.writeText(text);
    copyFeedback.classList.add('show');
    setTimeout(() => copyFeedback.classList.remove('show'), 2000);
  } catch (e) {
    console.error('Copy failed:', e);
  }
});

// Status change listener
chrome.runtime.onMessage.addListener((msg) => {
  if (msg?.type === 'STATUS_CHANGED') {
    // reconnect-clean-states: suppress during the reconnect window so SW
    // broadcasts (nm-host up but bridge not yet) don't flash intermediate
    // disconnected states — only the reconnect poll renders in that window.
    if (reconnecting) return;
    queryStatus();
  }
});

// ---------------------------------------------------------------------------
// Init
// ---------------------------------------------------------------------------

// fetchStatus queries GET_STATUS (+ /health fallback) and resolves with the
// status WITHOUT rendering — so callers (e.g. the reconnect poll) can decide
// whether to render the real state or keep a synthetic transition state.
function fetchStatus() {
  return new Promise((resolve) => {
    chrome.runtime.sendMessage({ type: 'GET_STATUS' }, async (status) => {
      if (chrome.runtime.lastError || !status) {
        status = { nmConnected: false, healthStatus: null };
      }
      // Supplement: fetch /health directly (background cache may be empty after SW restart)
      if (!status.transportType) {
        try {
          const resp = await fetch(P.HEALTH_URL);
          const health = await resp.json();
          if (health.internal_transport && health.internal_transport.type) {
            status.transportType = health.internal_transport.type;
            status.transportAddress = health.internal_transport.address;
            status.healthStatus = health.status;
            status.mcpPort = health.port;
            status.transports = health.transports;
          }
        } catch (e) { /* bridge not running — ignore */ }
      }
      // Always fetch bridge version (git describe, e.g. v0.3.11-3-gc4a063c-dirty)
      // for runtime dev-build confirmation. Fetched separately because the
      // transportType supplement above only runs on cache miss — version must
      // refresh every popup open regardless of cache state.
      if (!status.bridgeVersion) {
        try {
          const resp = await fetch(P.HEALTH_URL);
          const health = await resp.json();
          if (health.version) status.bridgeVersion = health.version;
        } catch (e) { /* bridge not running — ignore */ }
      }
      resolve(status);
    });
  });
}

async function queryStatus() {
  const status = await fetchStatus();
  render(status);
  renderStaleBanner(); // version-mismatch check (independent of connection status)
}

// renderStaleBanner compares the running bridge's version (from /api/status)
// against THIS extension's manifest version. When the bridge self-updates
// (e.g. 0.3.5 → 0.3.7) the on-disk extension is refreshed, but Chrome still
// has the OLD extension loaded in memory — reload needed. The banner makes
// that visible in the popup itself (not just the desktop dashboard). Hidden
// when versions match or the bridge is unreachable. Best-effort: a fetch
// failure just leaves the banner hidden.
function renderStaleBanner() {
  const banner = document.getElementById('ext-stale-banner');
  if (!banner) return;
  fetch(STATUS_URL)
    .then(r => r.json())
    .then(s => {
      const extVer = chrome.runtime.getManifest().version;
      const bridgeVer = String(s.bridge_version || '').replace(/^v+/, '');
      const extBare = String(extVer || '').replace(/^v+/, '');
      if (bridgeVer && extBare && bridgeVer !== extBare) {
        document.getElementById('stale-bridge-ver').textContent = fmtVer(bridgeVer);
        document.getElementById('stale-ext-ver').textContent = fmtVer(extBare);
        banner.style.display = '';
      } else {
        banner.style.display = 'none';
      }
    })
    .catch(() => { banner.style.display = 'none'; }); // bridge unreachable → hide
}

// fmtVer normalizes a version string to a leading-v form for display:
// "0.3.7" → "v0.3.7", "v0.3.7" → "v0.3.7". Matches the desktop dashboard's
// fmtVer so the popup + dashboard show the same label.
function fmtVer(v) {
  return 'v' + String(v).replace(/^v+/, '');
}

// Render MCP config immediately (always visible, not gated on connection).
renderConfig();
queryStatus();
