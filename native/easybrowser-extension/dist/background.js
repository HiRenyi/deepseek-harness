// src/profile.ts
var PROFILE = {
  NM_HOST_NAME: "com.browser.mcp",
  MCP_PORT: 58080,
  HEALTH_URL: "http://127.0.0.1:58080/health",
  EXT_INFO_URL: "http://127.0.0.1:58080/api/extension-info",
  MCP_SERVER_NAME: "browser-mcp",
  TAB_GROUP_TITLE: "🤖 Browser MCP"
};

// src/reconcile.ts
function computeReconcileTargets(tabs, controlledIds) {
  const result = [];
  for (const t of tabs) {
    if (controlledIds.has(t.id)) continue;
    if (t.openerTabId !== void 0 && controlledIds.has(t.openerTabId)) {
      result.push(t.id);
    }
  }
  return result;
}

// src/background.ts
var NM_HOST_NAME = PROFILE.NM_HOST_NAME;
var CDP_VERSION = "1.3";
var HEARTBEAT_ALARM = "browser-mcp-heartbeat";
var HEARTBEAT_INTERVAL_MIN = 0.5;
var HEALTH_ALARM = "browser-mcp-health";
var HEALTH_INTERVAL_MIN = 1 / 6;
var HEALTH_URL = PROFILE.HEALTH_URL;
var EXT_INFO_URL = PROFILE.EXT_INFO_URL;
function reportVersion() {
  const v = chrome.runtime.getManifest().version;
  fetch(EXT_INFO_URL, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ version: v, id: chrome.runtime.id })
  }).catch(() => {
  });
}
var RECONNECT_ALARM = "browser-mcp-reconnect";
var RECONNECT_FAST_MS = 5e3;
var RECONNECT_PERIOD_MIN = 5;
var IDLE_ALARM = "browser-mcp-idle";
var IDLE_TIMEOUT_DEFAULT_MS = 12e4;
var IDLE_TIMEOUT_MIN_MS = 6e4;
var idleTimeoutMs = IDLE_TIMEOUT_DEFAULT_MS;
var port = null;
var connecting = false;
var reconnectPending = false;
var reconnectAttempt = 0;
var reconnectTimeoutId = null;
var attachedTabId = null;
var idleArmed = false;
var userDisconnected = false;
var DISCONNECT_FLAG_KEY = "userDisconnected";
function setUserDisconnected(v) {
  userDisconnected = v;
  try {
    void chrome.storage.session.set({ [DISCONNECT_FLAG_KEY]: v });
  } catch (e) {
    console.warn("[BrowserMCP] persist userDisconnected failed:", e);
  }
}
async function initUserDisconnected() {
  try {
    const got = await chrome.storage.session.get(DISCONNECT_FLAG_KEY);
    if (got[DISCONNECT_FLAG_KEY] === true) {
      userDisconnected = true;
      console.info("[BrowserMCP] restored userDisconnected=true from session storage (sticky survived SW restart)");
    }
  } catch (e) {
    console.warn("[BrowserMCP] restore userDisconnected failed:", e);
  }
}
var cdpEventHandler = null;
var subscribedEvents = /* @__PURE__ */ new Set();
var editorActive = false;
var annotationActive = false;
var mcpTabGroupId = null;
var mcpGroupTitle = PROFILE.TAB_GROUP_TITLE;
var managedColor = "blue";
var groupChain = Promise.resolve();
function withGroupLock(fn) {
  const run = groupChain.then(fn);
  groupChain = run.then(
    () => void 0,
    () => void 0
  );
  return run;
}
async function ensureMcpGroupForTab(tabId) {
  if (mcpTabGroupId !== null) {
    try {
      await chrome.tabGroups.get(mcpTabGroupId);
      await chrome.tabs.group({ groupId: mcpTabGroupId, tabIds: [tabId] });
      console.log("[BrowserMCP] Tab added to existing group:", mcpTabGroupId);
      return;
    } catch {
      console.log("[BrowserMCP] Stale group", mcpTabGroupId, "→ creating new");
      mcpTabGroupId = null;
    }
  }
  const groupId = await chrome.tabs.group({ tabIds: [tabId] });
  await chrome.tabGroups.update(groupId, {
    title: mcpGroupTitle,
    color: managedColor,
    collapsed: false
  });
  mcpTabGroupId = groupId;
  saveTabGroupState();
  console.log("[BrowserMCP] New tab group created:", groupId, "title:", mcpGroupTitle);
}
async function sweepOrphanMcpGroups() {
  try {
    const all = await chrome.tabGroups.query({});
    const orphans = all.filter(
      (g) => g.title === mcpGroupTitle && g.color === managedColor
    );
    if (orphans.length === 0) return;
    for (const g of orphans) {
      try {
        const tabs = await chrome.tabs.query({ groupId: g.id });
        if (tabs.length) {
          const ids = tabs.map((t) => t.id).filter((id) => id !== void 0);
          if (ids.length) await chrome.tabs.ungroup(ids);
        }
      } catch (e) {
        console.warn("[BrowserMCP] orphan sweep ungroup failed for group", g.id, e);
      }
    }
    console.log("[BrowserMCP] orphan sweep cleaned", orphans.length, "group(s)");
  } catch (e) {
    const message = e instanceof Error ? e.message : String(e);
    if (!message.includes("No current window")) {
      console.warn("[BrowserMCP] orphan sweep query failed:", e);
    }
  }
}
var cachedHealth = null;
function connectNative() {
  if (userDisconnected) {
    console.info("[BrowserMCP] connectNative skipped — user disconnected (sticky)");
    return;
  }
  if (port || connecting) {
    console.info("[BrowserMCP] connectNative skipped —", port ? "port exists" : "connecting");
    return;
  }
  connecting = true;
  try {
    console.info("[BrowserMCP] Attempting connectNative to:", NM_HOST_NAME);
    const activePort = chrome.runtime.connectNative(NM_HOST_NAME);
    port = activePort;
    connecting = false;
    reconnectPending = false;
    reconnectAttempt = 0;
    clearReconnectTimers();
    console.info("[BrowserMCP] connectNative() returned port — connected:", NM_HOST_NAME);
    broadcastStatusChanged();
    activePort.onMessage.addListener((msg) => {
      if (port !== activePort) return;
      reconnectAttempt = 0;
      handleExtensionMessage(msg);
    });
    activePort.onDisconnect.addListener(() => {
      const err = chrome.runtime.lastError;
      if (port !== activePort) {
        console.info("[BrowserMCP] NM stale port disconnected (ignored):", err?.message ?? "unknown");
        return;
      }
      if (userDisconnected) {
        console.info("[BrowserMCP] NM port disconnected (user-initiated, sticky — no reconnect)");
        return;
      }
      console.warn("[BrowserMCP] NM port disconnected:", err?.message ?? "unknown error");
      port = null;
      connecting = false;
      dissolveActiveControl().catch((e) => console.warn("[BrowserMCP] dissolve on disconnect failed:", e));
      subscribedEvents.clear();
      broadcastStatusChanged();
      scheduleReconnect();
    });
  } catch (e) {
    console.warn("[BrowserMCP] connectNative() threw:", e);
    port = null;
    connecting = false;
    scheduleReconnect();
  }
}
function clearReconnectTimers() {
  if (reconnectTimeoutId !== null) {
    clearTimeout(reconnectTimeoutId);
    reconnectTimeoutId = null;
  }
  chrome.alarms.clear(RECONNECT_ALARM);
}
function scheduleReconnect() {
  if (userDisconnected) return;
  if (port || reconnectPending) return;
  reconnectPending = true;
  reconnectAttempt += 1;
  console.info(`[BrowserMCP] scheduleReconnect: attempt=${reconnectAttempt}, fast retry in ${RECONNECT_FAST_MS}ms + alarm backstop ${RECONNECT_PERIOD_MIN}min`);
  reconnectTimeoutId = setTimeout(runReconnectAttempt, RECONNECT_FAST_MS);
  chrome.alarms.create(RECONNECT_ALARM, { periodInMinutes: RECONNECT_PERIOD_MIN });
}
async function runReconnectAttempt() {
  if (port || connecting) {
    if (port) {
      clearReconnectTimers();
      reconnectPending = false;
    }
    return;
  }
  console.info(`[BrowserMCP] runReconnectAttempt: reconnecting (attempt=${reconnectAttempt})`);
  connecting = false;
  reconnectPending = false;
  connectNative();
}
async function handleExtensionMessage(msg) {
  if (!msg || msg.jsonrpc !== "2.0" || !msg.id) {
    console.warn("[BrowserMCP] Ignoring invalid JSON-RPC message", msg);
    return;
  }
  try {
    let result;
    switch (msg.method) {
      case "cdp.execute":
        result = await handleCdpExecute(msg);
        break;
      case "cdp.subscribe":
        result = await handleCdpSubscribe(msg);
        break;
      case "cdp.unsubscribe":
        result = await handleCdpUnsubscribe(msg);
        break;
      case "tabs.list":
        result = await handleTabsList();
        break;
      case "tabs.create":
        result = await handleTabsCreate(msg);
        break;
      case "tabs.close":
        result = await handleTabsClose(msg);
        break;
      case "tabs.switch":
        result = await handleTabsSwitch(msg);
        break;
      case "tabs.select_tab":
        result = await handleSelectTab(msg);
        break;
      case "tabs.adopt":
        result = await handleTabsAdopt(msg);
        break;
      case "tabs.name_session":
        result = await handleNameSession(msg);
        break;
      case "config.set_cdp_permission":
        result = await handleSetCdpPermission(msg);
        break;
      case "session.start":
        result = await handleSessionStart();
        break;
      case "session.end":
        result = await handleSessionEnd();
        break;
      default:
        sendResponse({ jsonrpc: "2.0", id: msg.id, error: { code: -32601, message: `Method not found: ${msg.method}` } });
        return;
    }
    if (msg.method !== "session.end") {
      resetIdleAlarm();
    }
    sendResponse({ jsonrpc: "2.0", id: msg.id, result });
  } catch (e) {
    const message = e instanceof Error ? e.message : String(e);
    sendResponse({ jsonrpc: "2.0", id: msg.id, error: { code: -32e3, message } });
  }
}
chrome.runtime.onMessage.addListener(
  (msg, _sender, sendResponse2) => {
    switch (msg.type) {
      case "GET_STATUS": {
        const it = cachedHealth?.internal_transport ?? {};
        const realNm = typeof cachedHealth?.nm_connected === "boolean" ? cachedHealth.nm_connected : port !== null;
        const nmConnected = userDisconnected ? false : realNm;
        sendResponse2({
          nmConnected,
          userDisconnected,
          // popup may show "已断开（用户）"
          connecting,
          // local pending-handshake flag for popup "连接中…"
          healthStatus: cachedHealth?.status ?? null,
          mcpPort: cachedHealth?.port ?? PROFILE.MCP_PORT,
          transports: cachedHealth?.transports ?? [],
          transportType: it.type ?? null,
          // "pipe" | "unix" | "tcp" | null
          transportAddress: it.address ?? null,
          cdpPermissions: cachedHealth?.cdp_permissions ?? null,
          idleTimeoutMs,
          tabGroup: { id: mcpTabGroupId, title: mcpGroupTitle }
        });
        return false;
      }
      case "DISCONNECT": {
        setUserDisconnected(true);
        clearReconnectTimers();
        if (port) {
          port.disconnect();
          port = null;
          detachDebugger();
          subscribedEvents.clear();
        }
        sendResponse2({ success: true, userDisconnected });
        broadcastStatusChanged();
        return false;
      }
      case "RECONNECT": {
        setUserDisconnected(false);
        if (port) {
          try {
            port.disconnect();
          } catch {
          }
          port = null;
        }
        connecting = false;
        reconnectPending = false;
        clearReconnectTimers();
        connectNative();
        sendResponse2({ success: true, connecting });
        return false;
      }
      case "TOGGLE_EDITOR": {
        toggleEditor().then((active) => {
          sendResponse2({ success: true, active });
        });
        return true;
      }
      case "TOGGLE_ANNOTATION": {
        toggleAnnotation().then((active) => {
          sendResponse2({ success: true, active });
        });
        return true;
      }
      case "SET_CDP_PERMISSION": {
        if (port && msg.key !== void 0 && msg.value !== void 0) {
          const id = Date.now();
          port.postMessage({
            jsonrpc: "2.0",
            id,
            method: "config.set_cdp_permission",
            params: { key: msg.key, value: msg.value }
          });
          sendResponse2({ success: true });
        } else {
          sendResponse2({ success: false, error: "NM not connected or missing params" });
        }
        return false;
      }
      case "SET_IDLE_TIMEOUT": {
        const ms = typeof msg.value === "number" ? msg.value : Number(msg.value);
        if (!Number.isFinite(ms) || ms < IDLE_TIMEOUT_MIN_MS) {
          sendResponse2({ success: false, error: `idle timeout must be a number >= ${IDLE_TIMEOUT_MIN_MS}ms (chrome.alarms floor)` });
          return false;
        }
        idleTimeoutMs = ms;
        persistIdleTimeout(ms).catch(() => {
        });
        if (attachedTabId !== null) resetIdleAlarm();
        sendResponse2({ success: true, idleTimeoutMs: ms });
        return false;
      }
    }
    return false;
  }
);
function broadcastStatusChanged() {
  chrome.runtime.sendMessage({ type: "STATUS_CHANGED" }).catch(() => {
  });
}
async function handleCdpExecute(msg) {
  const params = msg.params;
  if (!params?.cdpMethod) throw new Error("Missing cdpMethod in params");
  return executeCDP(params);
}
async function resolveTargetTabId(paramsTabId) {
  if (paramsTabId !== void 0 && paramsTabId !== null) return paramsTabId;
  if (attachedTabId !== null) return attachedTabId;
  throw new Error("no tab attached; call tabs.create or tabs.adopt first");
}
async function assertControllable(tabId) {
  if (tabId === attachedTabId) return;
  if (mcpTabGroupId !== null) {
    try {
      const tab = await chrome.tabs.get(tabId);
      if (tab.groupId !== void 0 && tab.groupId === mcpTabGroupId) return;
    } catch {
    }
  }
  throw new Error(`tab ${tabId} not in MCP control group; call tabs.adopt to enroll it first`);
}
async function executeCDP(params) {
  const tabId = await resolveTargetTabId(params.tabId);
  await assertControllable(tabId);
  await attachDebugger(tabId);
  return await chrome.debugger.sendCommand({ tabId }, params.cdpMethod, params.cdpParams ?? {});
}
function ensureCdpEventHandler() {
  if (cdpEventHandler !== null) return;
  cdpEventHandler = (source, method, params) => {
    resetIdleAlarm();
    if (subscribedEvents.has(method)) {
      sendResponse({ jsonrpc: "2.0", id: `event:${method}`, result: { method, params, source } });
    }
  };
  chrome.debugger.onEvent.addListener(cdpEventHandler);
}
async function handleCdpSubscribe(msg) {
  const params = msg.params;
  if (!params?.cdpEvent) throw new Error("Missing cdpEvent in params");
  const tabId = await resolveTargetTabId(params.tabId);
  await assertControllable(tabId);
  await attachDebugger(tabId);
  subscribedEvents.add(params.cdpEvent);
  ensureCdpEventHandler();
  const domain = params.cdpEvent.split(".")[0];
  try {
    await chrome.debugger.sendCommand({ tabId }, `${domain}.enable`, {});
  } catch {
  }
  return { subscribed: params.cdpEvent };
}
async function handleCdpUnsubscribe(msg) {
  const params = msg.params;
  if (!params?.cdpEvent) throw new Error("Missing cdpEvent in params");
  subscribedEvents.delete(params.cdpEvent);
  return { unsubscribed: params.cdpEvent };
}
function isControlledTab(tab) {
  if (tab.id !== void 0 && tab.id === attachedTabId) return true;
  if (mcpTabGroupId !== null && tab.groupId !== void 0 && tab.groupId === mcpTabGroupId) return true;
  return false;
}
function serializeTab(tab) {
  return { id: tab.id, index: tab.index, windowId: tab.windowId, url: tab.url ?? "", title: tab.title ?? "", active: tab.active, status: tab.status ?? "unknown", enrolled: isControlledTab(tab) };
}
async function handleTabsList() {
  await restoreTabGroupState();
  const tabs = await chrome.tabs.query({});
  const filtered = tabs.filter((t) => t.id !== void 0);
  const controlledIds = /* @__PURE__ */ new Set();
  if (attachedTabId !== null) controlledIds.add(attachedTabId);
  if (mcpTabGroupId !== null) {
    try {
      const groupTabs = await chrome.tabs.query({ groupId: mcpTabGroupId });
      for (const t of groupTabs) if (t.id !== void 0) controlledIds.add(t.id);
    } catch {
    }
  }
  const targets = computeReconcileTargets(
    filtered.map((t) => ({ id: t.id, openerTabId: t.openerTabId })),
    controlledIds
  );
  for (const tid of targets) {
    try {
      await withGroupLock(() => ensureMcpGroupForTab(tid));
    } catch (e) {
      console.warn("[BrowserMCP] list_tabs reconcile failed for", tid, e);
    }
  }
  if (targets.length > 0) {
    const refreshed = await chrome.tabs.query({});
    return refreshed.filter((t) => t.id !== void 0).map(serializeTab);
  }
  return filtered.map(serializeTab);
}
async function handleTabsCreate(msg) {
  const params = msg.params;
  const tab = await chrome.tabs.create({ url: params?.url });
  if (!tab || tab.id === void 0) throw new Error("Failed to create tab");
  try {
    await withGroupLock(() => ensureMcpGroupForTab(tab.id));
  } catch (e) {
    console.warn("[BrowserMCP] Tab group failed:", e);
  }
  await attachDebugger(tab.id);
  return serializeTab(tab);
}
async function handleTabsClose(msg) {
  const params = msg.params;
  if (params?.tabId === void 0) throw new Error("Missing tabId in params");
  await assertControllable(params.tabId);
  await chrome.tabs.remove(params.tabId);
  if (attachedTabId === params.tabId) attachedTabId = null;
  return { success: true };
}
async function handleTabsSwitch(msg) {
  const params = msg.params;
  if (params?.tabId === void 0) throw new Error("Missing tabId in params");
  await assertControllable(params.tabId);
  const tab = await chrome.tabs.update(params.tabId, { active: true });
  await attachDebugger(params.tabId);
  if (!tab || tab.id === void 0) throw new Error("Failed to switch to tab");
  return serializeTab(tab);
}
async function handleSelectTab(msg) {
  const params = msg.params;
  if (params?.tabId === void 0) throw new Error("Missing tabId in params");
  await assertControllable(params.tabId);
  await attachDebugger(params.tabId);
  return { tabId: params.tabId };
}
async function handleTabsAdopt(msg) {
  const params = msg.params;
  if (params?.tabId === void 0) throw new Error("Missing tabId in params");
  const tabId = params.tabId;
  try {
    await withGroupLock(() => ensureMcpGroupForTab(tabId));
  } catch (e) {
    console.warn("[BrowserMCP] Adopt grouping failed:", e);
  }
  await attachDebugger(tabId);
  return { tabId };
}
async function handleNameSession(msg) {
  const params = msg.params;
  if (!params?.title) throw new Error("Missing title in params");
  mcpGroupTitle = params.title;
  if (mcpTabGroupId !== null) {
    try {
      await chrome.tabGroups.update(mcpTabGroupId, { title: mcpGroupTitle });
    } catch (e) {
      console.warn("[BrowserMCP] Failed to update tab group title:", e);
      mcpTabGroupId = null;
    }
  }
  saveTabGroupState();
  return { success: true };
}
async function handleSetCdpPermission(msg) {
  return { success: true };
}
async function toggleEditor() {
  if (editorActive) {
    const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
    if (tabs.length && tabs[0].id) {
      try {
        await chrome.scripting.executeScript({
          target: { tabId: tabs[0].id },
          func: () => {
            document.querySelectorAll("[data-mcp-editor]").forEach((el) => el.remove());
            document.querySelectorAll("[contenteditable][data-mcp-editable]").forEach((el) => {
              el.removeAttribute("contenteditable");
              el.removeAttribute("data-mcp-editable");
            });
          }
        });
      } catch {
      }
    }
    editorActive = false;
  } else {
    const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
    if (tabs.length && tabs[0].id) {
      try {
        await chrome.scripting.executeScript({
          target: { tabId: tabs[0].id },
          files: ["dist/content/editor.js"]
        });
      } catch (e) {
        console.warn("[BrowserMCP] Editor inject failed:", e);
      }
    }
    editorActive = true;
  }
  return editorActive;
}
async function toggleAnnotation() {
  if (annotationActive) {
    const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
    if (tabs.length && tabs[0].id) {
      try {
        await chrome.scripting.executeScript({
          target: { tabId: tabs[0].id },
          func: () => {
            document.querySelectorAll("[data-mcp-annotation]").forEach((el) => el.remove());
          }
        });
      } catch {
      }
    }
    annotationActive = false;
  } else {
    const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
    if (tabs.length && tabs[0].id) {
      try {
        await chrome.scripting.executeScript({
          target: { tabId: tabs[0].id },
          files: ["dist/content/annotation.js"]
        });
      } catch (e) {
        console.warn("[BrowserMCP] Annotation inject failed:", e);
      }
    }
    annotationActive = true;
  }
  return annotationActive;
}
chrome.tabGroups.onRemoved.addListener(() => {
  mcpTabGroupId = null;
  saveTabGroupState();
  broadcastStatusChanged();
});
chrome.tabs.onRemoved.addListener((tabId) => {
  if (mcpTabGroupId !== null) {
    setTimeout(async () => {
      try {
        const tabs = await chrome.tabs.query({ groupId: mcpTabGroupId });
        if (tabs.length === 0) {
          mcpTabGroupId = null;
          saveTabGroupState();
          broadcastStatusChanged();
        }
      } catch {
        mcpTabGroupId = null;
        saveTabGroupState();
      }
    }, 100);
  }
});
chrome.tabs.onCreated.addListener((tab) => {
  if (tab.id === void 0) return;
  const opener = tab.openerTabId;
  const fromMcpTab = opener !== void 0 && (opener === attachedTabId || opener === tab.id);
  if (!fromMcpTab) return;
  (async () => {
    try {
      await withGroupLock(() => ensureMcpGroupForTab(tab.id));
    } catch (e) {
      console.warn("[BrowserMCP] onCreated grouping failed:", e);
    }
  })();
});
chrome.tabs.onUpdated.addListener((tabId, changeInfo, tab) => {
  if (attachedTabId !== tabId) return;
  if (changeInfo.status !== "complete") return;
  const url = tab?.url ?? "";
  if (!url.startsWith("http") && !url.startsWith("file:")) return;
  injectGlow(tabId).catch(() => {
  });
});
async function saveTabGroupState() {
  try {
    await chrome.storage.local.set({
      TAB_GROUP: { mcpTabGroupId, mcpGroupTitle }
    });
  } catch {
  }
}
async function restoreTabGroupState() {
  try {
    const result = await chrome.storage.local.get("TAB_GROUP");
    if (result.TAB_GROUP) {
      mcpGroupTitle = PROFILE.TAB_GROUP_TITLE;
      if (result.TAB_GROUP.mcpTabGroupId != null) {
        try {
          await chrome.tabGroups.get(result.TAB_GROUP.mcpTabGroupId);
          mcpTabGroupId = result.TAB_GROUP.mcpTabGroupId;
        } catch {
          mcpTabGroupId = null;
        }
      }
    }
  } catch {
  }
}
async function restoreIdleTimeout() {
  try {
    const result = await chrome.storage.local.get("IDLE_TIMEOUT_MS");
    if (typeof result.IDLE_TIMEOUT_MS === "number" && result.IDLE_TIMEOUT_MS > 0) {
      idleTimeoutMs = result.IDLE_TIMEOUT_MS;
    } else {
      idleTimeoutMs = IDLE_TIMEOUT_DEFAULT_MS;
    }
  } catch {
    idleTimeoutMs = IDLE_TIMEOUT_DEFAULT_MS;
  }
}
async function persistIdleTimeout(ms) {
  try {
    await chrome.storage.local.set({ IDLE_TIMEOUT_MS: ms });
  } catch {
  }
}
async function injectGlow(tabId) {
  try {
    await chrome.debugger.sendCommand({ tabId }, "Runtime.evaluate", {
      expression: `
        (function(){
          if (document.getElementById('mcp-glow-overlay')) return;
          var s = document.createElement('style');
          s.textContent = '@keyframes mcp-glow-breathe{0%,100%{opacity:.55}50%{opacity:.9}}#mcp-glow-overlay{position:fixed;inset:0;pointer-events:none;z-index:2147483647;box-shadow:inset 0 0 30px 4px rgba(99,102,241,.5),inset 0 0 60px 8px rgba(139,92,246,.3),0 0 0 2px rgba(99,102,241,.55),0 0 40px 6px rgba(99,102,241,.35);border:2px solid rgba(99,102,241,.6);outline:2px solid rgba(139,92,246,.25);animation:mcp-glow-breathe 2s ease-in-out infinite}';
          document.head.appendChild(s);
          var d = document.createElement('div');
          d.id = 'mcp-glow-overlay';
          (document.body||document.documentElement).appendChild(d);
        })();
      `
    });
  } catch (e) {
    console.warn("[BrowserMCP] Glow inject failed:", e);
  }
}
async function removeGlow(tabId) {
  try {
    await chrome.debugger.sendCommand({ tabId }, "Runtime.evaluate", {
      expression: `
        var el = document.getElementById('mcp-glow-overlay');
        if (el) el.remove();
      `
    });
  } catch {
  }
}
async function attachDebugger(tabId) {
  if (attachedTabId === tabId) return;
  if (attachedTabId !== null) await detachDebugger();
  try {
    await chrome.debugger.attach({ tabId }, CDP_VERSION);
    attachedTabId = tabId;
    console.info("[BrowserMCP] Debugger attached to tab", tabId);
    await injectGlow(tabId);
    resetIdleAlarm();
  } catch (e) {
    const message = e instanceof Error ? e.message : String(e);
    if (message.includes("Already attached")) {
      attachedTabId = tabId;
      await injectGlow(tabId);
    } else {
      throw e;
    }
  }
}
async function detachDebugger() {
  if (attachedTabId === null) return;
  const tabId = attachedTabId;
  attachedTabId = null;
  clearIdleAlarm();
  removeGlow(tabId);
  try {
    await chrome.debugger.detach({ tabId });
    console.info("[BrowserMCP] Debugger detached from tab", tabId);
  } catch (e) {
    const message = e instanceof Error ? e.message : String(e);
    if (!message.includes("Not attached") && !message.includes("No tab with id") && !message.includes("No tab with given id")) {
      console.warn("[BrowserMCP] Error detaching debugger", message);
    }
  }
}
chrome.debugger.onDetach.addListener((source) => {
  if (source.tabId === attachedTabId) {
    console.info("[BrowserMCP] Debugger detached externally", attachedTabId);
    dissolveActiveControl().catch(() => {
    });
  }
});
async function dissolveActiveControl() {
  if (attachedTabId !== null) {
    const tabId = attachedTabId;
    await removeGlow(tabId).catch(() => {
    });
    await detachDebugger().catch(() => {
    });
  }
  clearIdleAlarm();
  if (mcpTabGroupId !== null) {
    const groupId = mcpTabGroupId;
    mcpTabGroupId = null;
    try {
      const tabs = await chrome.tabs.query({ groupId });
      if (tabs.length) {
        const ids = tabs.map((t) => t.id).filter((id) => id !== void 0);
        if (ids.length) await chrome.tabs.ungroup(ids);
      }
    } catch (e) {
      console.warn("[BrowserMCP] ungroup on dissolve failed:", e);
    }
    saveTabGroupState();
  }
  await sweepOrphanMcpGroups();
}
async function releaseActiveTab() {
  if (attachedTabId === null) return;
  const tabId = attachedTabId;
  await removeGlow(tabId).catch(() => {
  });
  await detachDebugger().catch(() => {
  });
  attachedTabId = null;
}
async function handleSessionStart() {
  await releaseActiveTab();
  return { ok: true };
}
async function handleSessionEnd() {
  await dissolveActiveControl();
  return { ok: true };
}
function resetIdleAlarm() {
  if (attachedTabId === null) {
    clearIdleAlarm();
    return;
  }
  const effectiveMs = Math.max(idleTimeoutMs, IDLE_TIMEOUT_MIN_MS);
  const delayInMinutes = effectiveMs / 6e4;
  chrome.alarms.create(IDLE_ALARM, { delayInMinutes });
  idleArmed = true;
}
function clearIdleAlarm() {
  if (!idleArmed) return;
  chrome.alarms.clear(IDLE_ALARM);
  idleArmed = false;
}
function sendResponse(resp) {
  if (!port) {
    console.warn("[BrowserMCP] Cannot send response — NM port is null");
    return;
  }
  try {
    port.postMessage(resp);
  } catch (e) {
    console.warn("[BrowserMCP] Failed to post response (port dead):", e);
  }
}
var lastNmConnected = null;
async function checkHealth() {
  try {
    const resp = await fetch(HEALTH_URL);
    cachedHealth = await resp.json();
  } catch {
    cachedHealth = null;
  }
  reportVersion();
  syncGlowWithHealth();
  broadcastStatusChanged();
}
function syncGlowWithHealth() {
  const connected = cachedHealth?.nm_connected === true;
  if (connected === lastNmConnected) return;
  lastNmConnected = connected;
  if (attachedTabId === null) return;
  if (connected) {
    injectGlow(attachedTabId).catch(() => {
    });
  } else {
    removeGlow(attachedTabId).catch(() => {
    });
  }
}
function setupHealthCheck() {
  chrome.alarms.create(HEALTH_ALARM, { periodInMinutes: HEALTH_INTERVAL_MIN });
  checkHealth();
  console.info("[BrowserMCP] Health check alarm created");
}
chrome.alarms.onAlarm.addListener(async (alarm) => {
  if (alarm.name === HEARTBEAT_ALARM) {
    if (!port) {
      connecting = false;
      detachDebugger();
      connectNative();
      return;
    }
    try {
      port.postMessage({ jsonrpc: "2.0", method: "ping" });
    } catch (e) {
      console.warn("[BrowserMCP] Heartbeat: port dead, reconnecting");
      port = null;
      connecting = false;
      detachDebugger();
      connectNative();
    }
    return;
  }
  if (alarm.name === HEALTH_ALARM) {
    await checkHealth();
    return;
  }
  if (alarm.name === RECONNECT_ALARM) {
    await runReconnectAttempt();
    return;
  }
  if (alarm.name === IDLE_ALARM) {
    idleArmed = false;
    if (attachedTabId !== null) {
      handleSessionEnd().catch((e) => console.warn("[BrowserMCP] idle收尾 failed:", e));
    }
    return;
  }
});
function setupHeartbeat() {
  chrome.alarms.create(HEARTBEAT_ALARM, { periodInMinutes: HEARTBEAT_INTERVAL_MIN });
  console.info("[BrowserMCP] Heartbeat alarm created");
}
async function initConnect() {
  await initUserDisconnected();
  connectNative();
}
chrome.runtime.onInstalled.addListener(() => {
  console.info("[BrowserMCP] Installed");
  setupHeartbeat();
  setupHealthCheck();
  void initConnect();
  restoreTabGroupState();
  restoreIdleTimeout();
  sweepOrphanMcpGroups().catch((e) => console.warn("[BrowserMCP] startup orphan sweep failed:", e));
});
chrome.runtime.onStartup.addListener(() => {
  console.info("[BrowserMCP] Browser started");
  setupHeartbeat();
  setupHealthCheck();
  void initConnect();
  restoreTabGroupState();
  restoreIdleTimeout();
  sweepOrphanMcpGroups().catch((e) => console.warn("[BrowserMCP] startup orphan sweep failed:", e));
});
setupHeartbeat();
setupHealthCheck();
void initConnect();
restoreTabGroupState();
restoreIdleTimeout();
sweepOrphanMcpGroups().catch((e) => console.warn("[BrowserMCP] cold-start orphan sweep failed:", e));
