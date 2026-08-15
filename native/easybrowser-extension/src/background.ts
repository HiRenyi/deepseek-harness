/**
 * Browser MCP — Chrome Extension Service Worker
 *
 * Receives JSON-RPC over Native Messaging and dispatches to:
 *   - CDP commands via chrome.debugger
 *   - Tab management via chrome.tabs
 *   - CDP event subscriptions for real-time forwarding
 *
 * Popup channel (chrome.runtime.onMessage) handles:
 *   - GET_STATUS, DISCONNECT, RECONNECT
 *   - TOGGLE_EDITOR, TOGGLE_ANNOTATION
 *   - SET_CDP_PERMISSION
 *   - Tab group info
 */

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

import { PROFILE } from './profile';
import { computeReconcileTargets } from "./reconcile";

interface JsonRpcRequest {
  jsonrpc: "2.0";
  id: number | string;
  method: string;
  params?: unknown;
}

interface JsonRpcResponse {
  jsonrpc: "2.0";
  id: number | string;
  result?: unknown;
  error?: {
    code: number;
    message: string;
    data?: unknown;
  };
}

interface CdpExecuteParams {
  tabId?: number;
  cdpMethod: string;
  cdpParams?: Record<string, unknown>;
}

interface CdpSubscribeParams {
  tabId?: number;
  cdpEvent: string;
}

interface CdpUnsubscribeParams {
  cdpEvent: string;
}

interface PopupMessage {
  type: string;
  key?: string;
  value?: boolean | number;
}

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

const NM_HOST_NAME = PROFILE.NM_HOST_NAME;
const CDP_VERSION = "1.3";
const HEARTBEAT_ALARM = "browser-mcp-heartbeat";
const HEARTBEAT_INTERVAL_MIN = 0.5;
const HEALTH_ALARM = "browser-mcp-health";
const HEALTH_INTERVAL_MIN = 1 / 6; // 10 seconds
const HEALTH_URL = PROFILE.HEALTH_URL;
const EXT_INFO_URL = PROFILE.EXT_INFO_URL;

// reportVersion pushes the extension's manifest version to the bridge so the
// dashboard can display the loaded extension's version. Fire-and-forget; best
// effort (no throw on failure — bridge may be briefly down). Called on each
// health poll so the bridge sees the version within ~10s of the SW starting.
function reportVersion(): void {
  const v = chrome.runtime.getManifest().version;
  fetch(EXT_INFO_URL, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ version: v, id: chrome.runtime.id }),
  }).catch(() => { /* bridge down — health poll will retry next tick */ });
}

// Reconnect (aligned with Codex Bn: scheduleReconnect + kt=5e3 + ensureReconnectAlarm)
const RECONNECT_ALARM = "browser-mcp-reconnect";
const RECONNECT_FAST_MS = 5000;        // kt=5e3 — fast-path setTimeout
const RECONNECT_PERIOD_MIN = 5;        // Nn — alarm backstop (survives SW teardown)

// --- Graceful disconnect: idle-timeout fallback (chrome.alarms, SW-suspend-safe) ---
const IDLE_ALARM = "browser-mcp-idle";
const IDLE_TIMEOUT_DEFAULT_MS = 120000;  // 120s default; chrome.alarms production floor is 60s
const IDLE_TIMEOUT_MIN_MS = 60000;       // chrome.alarms delays < 60s are clamped in production
let idleTimeoutMs: number = IDLE_TIMEOUT_DEFAULT_MS;

let port: chrome.runtime.Port | null = null;
let connecting = false;                 // covers connectNative() return → handshake-complete pending window
let reconnectPending = false;
let reconnectAttempt = 0;
let reconnectTimeoutId: ReturnType<typeof setTimeout> | null = null;
let attachedTabId: number | null = null;
let idleArmed = false;  // tracks whether IDLE_ALARM is currently scheduled
// disconnect-sticky-stop: user-initiated DISCONNECT sets this; while true the
// extension does NOT auto-reconnect (connectNative top guard, scheduleReconnect,
// onDisconnect all skip). Distinguishes user-initiated stop from an unexpected
// drop (which still auto-reconnects — transport-resilience unchanged). Cleared
// by RECONNECT.
// disconnect-sticky-guard: persisted to chrome.storage.session so a MV3 SW
// suspension/restart mid-session doesn't lose the sticky state (the heartbeat's
// 30s !port probe + init connectNative would otherwise reconnect). session
// storage survives SW suspension but is cleared on browser restart — matching
// "temporary disconnect" semantics (a fresh browser session resumes auto-connect).
let userDisconnected = false;
const DISCONNECT_FLAG_KEY = "userDisconnected";

// setUserDisconnected sets the in-memory flag AND persists it to
// chrome.storage.session (best-effort; a storage failure doesn't block the
// in-memory flag, which still guards within this SW lifetime).
function setUserDisconnected(v: boolean): void {
  userDisconnected = v;
  try {
    void chrome.storage.session.set({ [DISCONNECT_FLAG_KEY]: v });
  } catch (e) {
    console.warn("[BrowserMCP] persist userDisconnected failed:", e);
  }
}

// initUserDisconnected restores the sticky flag from chrome.storage.session on
// SW startup. Called before the init connectNative so the top guard respects a
// sticky disconnect that survived a SW suspension. Async; the init connectNative
// awaits it. session storage is empty on a fresh browser start → false (resume
// auto-connect).
async function initUserDisconnected(): Promise<void> {
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

type CdpEventHandler = (
  source: chrome.debugger.Debuggee,
  method: string,
  params?: unknown,
) => void;

let cdpEventHandler: CdpEventHandler | null = null;
const subscribedEvents = new Set<string>();

// Quick tools state
let editorActive = false;
let annotationActive = false;

// Tab group state
let mcpTabGroupId: number | null = null;
let mcpGroupTitle: string = PROFILE.TAB_GROUP_TITLE;
// managedColor is the exclusive color paired with mcpGroupTitle. The orphan
// sweep (D4) matches title+color to find same-named leftover groups; this
// combo is ours alone — user-created groups with this exact title+color are
// ungrouped (data-preserving, never closed).
const managedColor: chrome.tabGroups.ColorEnum = "blue";

// Group-creation mutex (D3): serializes the group-operation sections in
// handleTabsCreate / handleTabsAdopt / chrome.tabs.onCreated so concurrent
// tabs.create calls cannot each see mcpTabGroupId===null and build separate
// duplicate groups (TOCTOU root cause of #2). Inside the critical section the
// double-check on mcpTabGroupId reuses an existing group when a concurrent
// caller already populated it.
let groupChain: Promise<void> = Promise.resolve();
function withGroupLock<T>(fn: () => Promise<T>): Promise<T> {
  const run = groupChain.then(fn);
  // Keep the chain alive regardless of fn's outcome so one failure does not
  // break subsequent critical sections.
  groupChain = run.then(
    () => undefined,
    () => undefined,
  );
  return run;
}

// ensureMcpGroupForTab is the shared critical-section body (D3 + 4.4): if the
// current mcpTabGroupId is alive, add the tab to it; otherwise create a fresh
// group titled/color'ed to the MCP contract and remember its id. Caller MUST
// hold the group lock (via withGroupLock) so the double-check is race-free.
async function ensureMcpGroupForTab(tabId: number): Promise<void> {
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
    collapsed: false,
  });
  mcpTabGroupId = groupId;
  saveTabGroupState();
  console.log("[BrowserMCP] New tab group created:", groupId, "title:", mcpGroupTitle);
}

// sweepOrphanMcpGroups (D4): ungroups ALL tab groups whose title+color match
// the MCP contract, not just the current mcpTabGroupId pointer. This catches
// leftover groups from prior sessions / SW restarts (mcpTabGroupId is in-memory
// and lost on SW teardown). Idempotent — ungrouping an already-ungrouped group
// is a no-op. Only ungroups (cancels grouping); never closes tabs, so even a
// false-positive match is data-preserving and re-groupable.
async function sweepOrphanMcpGroups(): Promise<void> {
  try {
    // Query all groups then filter client-side by title+color. The MV3
    // tabGroups.query filter exists but support for title/color filtering
    // varies across Chrome versions; client-side filtering is the robust path.
    const all = await chrome.tabGroups.query({});
    const orphans = all.filter(
      (g) => g.title === mcpGroupTitle && g.color === managedColor,
    );
    if (orphans.length === 0) return;
    for (const g of orphans) {
      try {
        const tabs = await chrome.tabs.query({ groupId: g.id });
        if (tabs.length) {
          const ids = tabs
            .map((t) => t.id)
            .filter((id): id is number => id !== undefined);
          if (ids.length) await chrome.tabs.ungroup(ids);
        }
      } catch (e) {
        console.warn("[BrowserMCP] orphan sweep ungroup failed for group", g.id, e);
      }
    }
    console.log("[BrowserMCP] orphan sweep cleaned", orphans.length, "group(s)");
  } catch (e: unknown) {
    // SW cold-start / onStartup / onInstalled may fire before any browser
    // window exists → tabGroups.query throws "No current window". That's a
    // normal startup race, not a real error; the next sweep on a windowed
    // alarm (heartbeat/health) will clean up orphans. Silence it so the SW
    // console stays clean for real diagnostics.
    const message = e instanceof Error ? e.message : String(e);
    if (!message.includes("No current window")) {
      console.warn("[BrowserMCP] orphan sweep query failed:", e);
    }
  }
}

// Health check cache
let cachedHealth: Record<string, unknown> | null = null;

// ---------------------------------------------------------------------------
// Native Messaging
// ---------------------------------------------------------------------------

function connectNative(): void {
  // disconnect-sticky-guard: the single chokepoint for all auto-reconnect
  // paths (heartbeat !port/port-dead, init onInstalled/onStartup/top-level,
  // scheduleReconnect→runReconnectAttempt). If the user explicitly disconnected,
  // NEVER auto-(re)connect — the heartbeat's 30s !port probe used to bypass
  // userDisconnected + relaunch bridge via nm-host launchBridge. RECONNECT
  // clears userDisconnected=false before calling this, so the user's explicit
  // reconnect still goes through.
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
    // Codex Bn: connectNative() returning port (no throw) = connected immediately.
    // No handshake middle state — nm-host does not send a ready/hello message,
    // and we MUST NOT wait for one (doing so leaves `connecting` stuck true
    // and locks the popup on "连接中…"). `connecting` is only the brief
    // in-call flag for dedup; clear it the moment connectNative returns.
    connecting = false;
    reconnectPending = false;
    reconnectAttempt = 0;
    clearReconnectTimers();
    console.info("[BrowserMCP] connectNative() returned port — connected:", NM_HOST_NAME);
    broadcastStatusChanged();

    activePort.onMessage.addListener((msg: unknown) => {
      // Codex Bn: if(this.port===e) — only process current port's messages
      if (port !== activePort) return;
      // Reaffirm connected on each message (mirrors Codex onMessage updateStatus).
      reconnectAttempt = 0;
      handleExtensionMessage(msg as JsonRpcRequest);
    });

    activePort.onDisconnect.addListener(() => {
      const err = chrome.runtime.lastError;
      // Codex Bn: if(this.port!==e) return — ignore stale/old port's lagged disconnect
      if (port !== activePort) {
        console.info("[BrowserMCP] NM stale port disconnected (ignored):", err?.message ?? "unknown");
        return;
      }
      // disconnect-sticky-stop: user-initiated DISCONNECT already set the flag +
      // cleared timers; this onDisconnect is the expected consequence of the
      // user's port.disconnect(), not an unexpected drop → skip scheduleReconnect.
      if (userDisconnected) {
        console.info("[BrowserMCP] NM port disconnected (user-initiated, sticky — no reconnect)");
        return;
      }
      // connectNative returning port meant connected, so any onDisconnect here is
      // an unexpected drop (not a handshake failure — there is no handshake).
      console.warn("[BrowserMCP] NM port disconnected:", err?.message ?? "unknown error");
      port = null;
      connecting = false;
      dissolveActiveControl().catch((e) => console.warn("[BrowserMCP] dissolve on disconnect failed:", e));
      subscribedEvents.clear();
      broadcastStatusChanged();
      // Any onDisconnect where port===activePort schedules reconnect.
      scheduleReconnect();
    });
  } catch (e) {
    console.warn("[BrowserMCP] connectNative() threw:", e);
    port = null;
    connecting = false;
    scheduleReconnect();
  }
}

// ---------------------------------------------------------------------------
// Reconnect scheduling (aligned with Codex Bn.scheduleReconnect + ensureReconnectAlarm)
// Dual-layer: setTimeout(5s) fast path (SW alive) + chrome.alarms(5min) backstop (survives SW teardown)
// ---------------------------------------------------------------------------

function clearReconnectTimers(): void {
  if (reconnectTimeoutId !== null) {
    clearTimeout(reconnectTimeoutId);
    reconnectTimeoutId = null;
  }
  chrome.alarms.clear(RECONNECT_ALARM);
}

function scheduleReconnect(): void {
  // disconnect-sticky-stop: user explicitly disconnected → never auto-reconnect.
  if (userDisconnected) return;
  if (port || reconnectPending) return; // already connected or already scheduled
  reconnectPending = true;
  reconnectAttempt += 1;
  console.info(`[BrowserMCP] scheduleReconnect: attempt=${reconnectAttempt}, fast retry in ${RECONNECT_FAST_MS}ms + alarm backstop ${RECONNECT_PERIOD_MIN}min`);
  // Fast path — fires only if SW stays alive
  reconnectTimeoutId = setTimeout(runReconnectAttempt, RECONNECT_FAST_MS);
  // Backstop — survives SW teardown (chrome.alarms is persisted by the browser)
  chrome.alarms.create(RECONNECT_ALARM, { periodInMinutes: RECONNECT_PERIOD_MIN });
}

async function runReconnectAttempt(): Promise<void> {
  // Called from both setTimeout (fast path) and onAlarm (backstop)
  if (port || connecting) {
    // Already reconnected; ensure alarm is cleared
    if (port) { clearReconnectTimers(); reconnectPending = false; }
    return;
  }
  console.info(`[BrowserMCP] runReconnectAttempt: reconnecting (attempt=${reconnectAttempt})`);
  // Force-clear connecting in case a prior attempt left it stuck (prevents connectNative no-op deadlock)
  connecting = false;
  reconnectPending = false;
  connectNative();
}

// ---------------------------------------------------------------------------
// Message handling — NM JSON-RPC channel (existing)
// ---------------------------------------------------------------------------

async function handleExtensionMessage(msg: JsonRpcRequest): Promise<void> {
  if (!msg || msg.jsonrpc !== "2.0" || !msg.id) {
    console.warn("[BrowserMCP] Ignoring invalid JSON-RPC message", msg);
    return;
  }
  try {
    let result: unknown;
    switch (msg.method) {
      case "cdp.execute": result = await handleCdpExecute(msg); break;
      case "cdp.subscribe": result = await handleCdpSubscribe(msg); break;
      case "cdp.unsubscribe": result = await handleCdpUnsubscribe(msg); break;
      case "tabs.list": result = await handleTabsList(); break;
      case "tabs.create": result = await handleTabsCreate(msg); break;
      case "tabs.close": result = await handleTabsClose(msg); break;
      case "tabs.switch": result = await handleTabsSwitch(msg); break;
      case "tabs.select_tab": result = await handleSelectTab(msg); break;
      case "tabs.adopt": result = await handleTabsAdopt(msg); break;
      case "tabs.name_session": result = await handleNameSession(msg); break;
      case "config.set_cdp_permission": result = await handleSetCdpPermission(msg); break;
      case "session.start": result = await handleSessionStart(); break;
      case "session.end": result = await handleSessionEnd(); break;
      default:
        sendResponse({ jsonrpc: "2.0", id: msg.id, error: { code: -32601, message: `Method not found: ${msg.method}` } });
        return;
    }
    // Any real MCP/CDP activity (except session.end, which IS the收尾) resets
    // the idle alarm. Heartbeat ping and health poll do NOT go through this
    // dispatcher, so they don't reset — desired "heartbeat keeps SW alive but
    // doesn't defeat idle收尾" semantics.
    if (msg.method !== "session.end") {
      resetIdleAlarm();
    }
    sendResponse({ jsonrpc: "2.0", id: msg.id, result });
  } catch (e: unknown) {
    const message = e instanceof Error ? e.message : String(e);
    sendResponse({ jsonrpc: "2.0", id: msg.id, error: { code: -32000, message } });
  }
}

// ---------------------------------------------------------------------------
// Message handling — Popup channel (new)
// ---------------------------------------------------------------------------

chrome.runtime.onMessage.addListener(
  (msg: PopupMessage, _sender: chrome.runtime.MessageSender, sendResponse: (response?: unknown) => void) => {
    switch (msg.type) {
      case "GET_STATUS": {
        // Real connection state = bridge /health heartbeat-based value
        // (srv.HasActiveConnection lied when nm-host silently exited).
        // (transport-resilience: honest connection state + transport visibility.)
        const it = (cachedHealth?.internal_transport ?? {}) as Record<string, string>;
        const realNm = typeof cachedHealth?.nm_connected === "boolean"
          ? (cachedHealth.nm_connected as boolean)
          : (port !== null);
        // disconnect-sticky-stop: when the user explicitly disconnected, report
        // not-connected immediately (don't wait for the bridge /health heartbeat
        // to time out ~10s later) so the popup LED turns red right away.
        const nmConnected = userDisconnected ? false : realNm;
        sendResponse({
          nmConnected,
          userDisconnected,                             // popup may show "已断开（用户）"
          connecting,                              // local pending-handshake flag for popup "连接中…"
          healthStatus: cachedHealth?.status ?? null,
          mcpPort: cachedHealth?.port ?? PROFILE.MCP_PORT,
          transports: cachedHealth?.transports ?? [],
          transportType: it.type ?? null,        // "pipe" | "unix" | "tcp" | null
          transportAddress: it.address ?? null,
          cdpPermissions: cachedHealth?.cdp_permissions ?? null,
          idleTimeoutMs,
          tabGroup: { id: mcpTabGroupId, title: mcpGroupTitle }
        });
        return false; // synchronous
      }

      case "DISCONNECT": {
        // disconnect-sticky-stop: user explicitly disconnected → sticky stop.
        // Mark + clear any pending reconnect timers BEFORE dropping the port so
        // neither a queued setTimeout nor the RECONNECT_ALARM backstop can pull
        // bridge back up (the "kill 被拉起" root cause).
        setUserDisconnected(true);
        clearReconnectTimers();
        if (port) {
          port.disconnect();
          port = null;
          detachDebugger();
          subscribedEvents.clear();
        }
        sendResponse({ success: true, userDisconnected });
        broadcastStatusChanged();
        return false;
      }

      case "RECONNECT": {
        // disconnect-sticky-stop: user rescinds the sticky stop → re-arm
        // auto-reconnect semantics, then connect now.
        setUserDisconnected(false);
        // Force-reset any pending/stale port, then reconnect
        if (port) {
          try { port.disconnect(); } catch { /* already gone */ }
          port = null;
        }
        connecting = false;
        reconnectPending = false;
        clearReconnectTimers();
        connectNative();
        sendResponse({ success: true, connecting });
        return false;
      }

      case "TOGGLE_EDITOR": {
        toggleEditor().then((active) => {
          sendResponse({ success: true, active });
        });
        return true; // async
      }

      case "TOGGLE_ANNOTATION": {
        toggleAnnotation().then((active) => {
          sendResponse({ success: true, active });
        });
        return true; // async
      }

      case "SET_CDP_PERMISSION": {
        // Forward to bridge via NM JSON-RPC
        if (port && msg.key !== undefined && msg.value !== undefined) {
          const id = Date.now();
          port.postMessage({
            jsonrpc: "2.0",
            id,
            method: "config.set_cdp_permission",
            params: { key: msg.key, value: msg.value }
          });
          sendResponse({ success: true });
        } else {
          sendResponse({ success: false, error: "NM not connected or missing params" });
        }
        return false;
      }

      case "SET_IDLE_TIMEOUT": {
        const ms = typeof msg.value === "number" ? msg.value : Number(msg.value);
        if (!Number.isFinite(ms) || ms < IDLE_TIMEOUT_MIN_MS) {
          sendResponse({ success: false, error: `idle timeout must be a number >= ${IDLE_TIMEOUT_MIN_MS}ms (chrome.alarms floor)` });
          return false;
        }
        idleTimeoutMs = ms;
        persistIdleTimeout(ms).catch(() => {});
        if (attachedTabId !== null) resetIdleAlarm();
        sendResponse({ success: true, idleTimeoutMs: ms });
        return false;
      }
    }
    return false;
  }
);

// ---------------------------------------------------------------------------
// Status broadcast
// ---------------------------------------------------------------------------

function broadcastStatusChanged(): void {
  chrome.runtime.sendMessage({ type: "STATUS_CHANGED" }).catch(() => {
    // No listeners (popup not open) — ignore
  });
}

// ---------------------------------------------------------------------------
// CDP execution
// ---------------------------------------------------------------------------

async function handleCdpExecute(msg: JsonRpcRequest): Promise<unknown> {
  const params = msg.params as CdpExecuteParams | undefined;
  if (!params?.cdpMethod) throw new Error("Missing cdpMethod in params");
  return executeCDP(params);
}

// resolveTargetTabId picks the tabId a CDP call should target:
//   1. an explicit tabId on the params (agent passed one), else
//   2. the pinned attachedTabId (Codex ensureAttachedTab alignment — once
//      attached, all subsequent CDP ops target that tab regardless of focus), else
//   3. SECURITY: throw — do NOT fall back to chrome.tabs.query({active,currentWindow}).
//
// The old active-tab fallback (#15 tab-targeting-fix遗留) hijacked the user's
// current tab on first control — a browser_navigate would redirect it and lose
// unsaved work (control-safety-boundary: No Active-Tab Hijack on First Control).
// The agent must explicitly enroll a tab via tabs.create (new group tab) or
// tabs.adopt (existing user tab).
async function resolveTargetTabId(paramsTabId: number | undefined | null): Promise<number> {
  if (paramsTabId !== undefined && paramsTabId !== null) return paramsTabId;
  if (attachedTabId !== null) return attachedTabId;
  throw new Error("no tab attached; call tabs.create or tabs.adopt first");
}

// assertControllable enforces the group-as-sandbox boundary
// (control-safety-boundary: Group-Only Operability Guard). A tab is
// controllable iff it is the pinned attachedTabId OR currently a member of
// the MCP tab group. Group-out (user) tabs are rejected so the agent cannot
// close/focus/inject/CDP-operate tabs the user hasn't explicitly adopted.
async function assertControllable(tabId: number): Promise<void> {
  if (tabId === attachedTabId) return;
  if (mcpTabGroupId !== null) {
    try {
      const tab = await chrome.tabs.get(tabId);
      if (tab.groupId !== undefined && tab.groupId === mcpTabGroupId) return;
    } catch {
      // tab no longer exists — fall through to reject
    }
  }
  throw new Error(`tab ${tabId} not in MCP control group; call tabs.adopt to enroll it first`);
}

async function executeCDP(params: CdpExecuteParams): Promise<unknown> {
  const tabId = await resolveTargetTabId(params.tabId);
  await assertControllable(tabId);
  await attachDebugger(tabId);
  return await chrome.debugger.sendCommand({ tabId }, params.cdpMethod, params.cdpParams ?? {});
}

// ---------------------------------------------------------------------------
// CDP event subscriptions
// ---------------------------------------------------------------------------

function ensureCdpEventHandler(): void {
  if (cdpEventHandler !== null) return;
  cdpEventHandler = (source: chrome.debugger.Debuggee, method: string, params?: unknown) => {
    resetIdleAlarm();  // CDP event = active page under control
    if (subscribedEvents.has(method)) {
      sendResponse({ jsonrpc: "2.0", id: `event:${method}`, result: { method, params, source } });
    }
  };
  chrome.debugger.onEvent.addListener(cdpEventHandler);
}

async function handleCdpSubscribe(msg: JsonRpcRequest): Promise<{ subscribed: string }> {
  const params = msg.params as CdpSubscribeParams | undefined;
  if (!params?.cdpEvent) throw new Error("Missing cdpEvent in params");
  const tabId = await resolveTargetTabId(params.tabId);
  await assertControllable(tabId);
  await attachDebugger(tabId);
  subscribedEvents.add(params.cdpEvent);
  ensureCdpEventHandler();
  const domain = params.cdpEvent.split(".")[0];
  try { await chrome.debugger.sendCommand({ tabId }, `${domain}.enable`, {}); } catch {}
  return { subscribed: params.cdpEvent };
}

async function handleCdpUnsubscribe(msg: JsonRpcRequest): Promise<{ unsubscribed: string }> {
  const params = msg.params as CdpUnsubscribeParams | undefined;
  if (!params?.cdpEvent) throw new Error("Missing cdpEvent in params");
  subscribedEvents.delete(params.cdpEvent);
  return { unsubscribed: params.cdpEvent };
}

// ---------------------------------------------------------------------------
// Tab management
// ---------------------------------------------------------------------------

interface TabInfo {
  id: number; index: number; windowId: number; url: string; title: string; active: boolean; status: string; enrolled: boolean;
}

// isControlledTab mirrors assertControllable (:573) — a tab is controlled iff
// it is the pinned attachedTabId OR a member of the MCP tab group. Used by
// serializeTab to populate the enrolled field surfaced to the AI via list_tabs.
function isControlledTab(tab: chrome.tabs.Tab): boolean {
  if (tab.id !== undefined && tab.id === attachedTabId) return true;
  if (mcpTabGroupId !== null && tab.groupId !== undefined && tab.groupId === mcpTabGroupId) return true;
  return false;
}

function serializeTab(tab: chrome.tabs.Tab): TabInfo {
  return { id: tab.id!, index: tab.index, windowId: tab.windowId, url: tab.url ?? "", title: tab.title ?? "", active: tab.active, status: tab.status ?? "unknown", enrolled: isControlledTab(tab) };
}

async function handleTabsList(): Promise<TabInfo[]> {
  // SW may have torn down + restarted since the last call (MV3 30s idle);
  // top-level restoreTabGroupState (:1306) is fire-and-forget, so mcpTabGroupId
  // may still be null when list_tabs arrives. Await restore here so the
  // controlled set is populated before computing enrolled/reconcile targets.
  await restoreTabGroupState();

  const tabs = await chrome.tabs.query({});
  const filtered = tabs.filter((t) => t.id !== undefined);

  // Determine controlled set: attachedTabId + members of mcpTabGroupId.
  const controlledIds = new Set<number>();
  if (attachedTabId !== null) controlledIds.add(attachedTabId);
  if (mcpTabGroupId !== null) {
    try {
      const groupTabs = await chrome.tabs.query({ groupId: mcpTabGroupId });
      for (const t of groupTabs) if (t.id !== undefined) controlledIds.add(t.id);
    } catch {
      // stale group — ensureMcpGroupForTab will rebuild on reconcile
    }
  }

  // DIAG (commented after v1.0.12 real-machine confirm — fix生效: mcpTabGroupId
  // restored, window.open'd tab shows [controlled]. Uncomment to root-cause a
  // future list_tabs [unmanaged]-for-all regression):
  // console.log("[BrowserMCP] list_tabs state:", {
  //   attachedTabId,
  //   mcpTabGroupId,
  //   controlledCount: controlledIds.size,
  //   controlledIds: [...controlledIds],
  //   tabs: filtered.map((t) => ({ id: t.id, groupId: t.groupId, openerTabId: t.openerTabId })),
  // });

  // Lazy reconcile (Drift-G fallback): fold window.open'd tabs whose opener is
  // controlled into the MCP group. Mirrors chrome.tabs.onCreated listener but
  // runs at list_tabs time so the AI sees them as [controlled].
  const targets = computeReconcileTargets(
    filtered.map((t) => ({ id: t.id!, openerTabId: t.openerTabId })),
    controlledIds,
  );
  // DIAG (commented): console.log("[BrowserMCP] list_tabs reconcile targets:", targets);
  for (const tid of targets) {
    try {
      await withGroupLock(() => ensureMcpGroupForTab(tid));
    } catch (e) {
      console.warn("[BrowserMCP] list_tabs reconcile failed for", tid, e);
    }
  }

  // If we grouped anything, re-query so serializeTab sees updated groupId.
  if (targets.length > 0) {
    const refreshed = await chrome.tabs.query({});
    return refreshed.filter((t) => t.id !== undefined).map(serializeTab);
  }
  return filtered.map(serializeTab);
}

async function handleTabsCreate(msg: JsonRpcRequest): Promise<TabInfo> {
  const params = msg.params as { url?: string } | undefined;
  const tab = await chrome.tabs.create({ url: params?.url });
  if (!tab || tab.id === undefined) throw new Error("Failed to create tab");

  // Auto-group into MCP tab group (with runtime self-healing, per Codex approach).
  // D3: the group operation is serialized via withGroupLock + ensureMcpGroupForTab
  // so concurrent tabs.create calls cannot each build a separate duplicate group.
  try {
    await withGroupLock(() => ensureMcpGroupForTab(tab.id!));
  } catch (e) {
    console.warn("[BrowserMCP] Tab group failed:", e);
  }

  // Attach CDP to the newly created tab so subsequent tools (navigate/snapshot/
  // click) target it. tabs.create is an enrollment primitive — resolveTargetTabId
  // throws "no tab attached; call tabs.create or tabs.adopt first", implying
  // tabs.create establishes the attached pin. Without this call attachedTabId
  // stayed null and the next tool errored "no tab attached" right after a
  // new_tab. Mirrors handleTabsAdopt (:711) which also attaches after enrolling.
  await attachDebugger(tab.id!);

  return serializeTab(tab);
}

async function handleTabsClose(msg: JsonRpcRequest): Promise<{ success: true }> {
  const params = msg.params as { tabId?: number } | undefined;
  if (params?.tabId === undefined) throw new Error("Missing tabId in params");
  await assertControllable(params.tabId);
  await chrome.tabs.remove(params.tabId);
  if (attachedTabId === params.tabId) attachedTabId = null;
  return { success: true };
}

async function handleTabsSwitch(msg: JsonRpcRequest): Promise<TabInfo> {
  const params = msg.params as { tabId?: number } | undefined;
  if (params?.tabId === undefined) throw new Error("Missing tabId in params");
  await assertControllable(params.tabId);
  const tab = await chrome.tabs.update(params.tabId, { active: true });
  // Re-target CDP control to the focused tab (D5). Without this, D1's pin
  // would leave CDP on the old tab after a switch — a footgun. switch=focus+select.
  await attachDebugger(params.tabId);
  if (!tab || tab.id === undefined) throw new Error("Failed to switch to tab");
  return serializeTab(tab);
}

// handleSelectTab selects a tab as the CDP control target WITHOUT focusing
// it or stealing mouse focus (Codex browser.user.claimTab equivalent).
// CDP works on background tabs, so the user can keep operating their current
// tab while the agent controls the selected one. Pairs with the bridge's
// browser_select_tab MCP tool.
async function handleSelectTab(msg: JsonRpcRequest): Promise<{ tabId: number }> {
  const params = msg.params as { tabId?: number } | undefined;
  if (params?.tabId === undefined) throw new Error("Missing tabId in params");
  await assertControllable(params.tabId);
  await attachDebugger(params.tabId);  // D2: detach old → attach new, set pin
  return { tabId: params.tabId };
}

// handleTabsAdopt enrolls a user-opened tab into the MCP tab group and makes
// it the CDP control target (control-safety-boundary: Tab Adoption Primitive).
// This is the safe path to "operate the OA form the user already has open" —
// the agent never auto-grabs the active tab; it explicitly adopts a tabId
// (obtained via tabs.list). Consent is provided by the caller's tool-approval
// gate (Kairos "Requesting user approval" per tool call) — no extra consent UI
// is built on the extension side. Group self-healing mirrors handleTabsCreate:
// validate the existing group is alive, else create a new one.
async function handleTabsAdopt(msg: JsonRpcRequest): Promise<{ tabId: number }> {
  const params = msg.params as { tabId?: number } | undefined;
  if (params?.tabId === undefined) throw new Error("Missing tabId in params");
  const tabId = params.tabId;
  try {
    // D3: serialize group creation across concurrent adopt/create/onCreated.
    await withGroupLock(() => ensureMcpGroupForTab(tabId));
  } catch (e) {
    console.warn("[BrowserMCP] Adopt grouping failed:", e);
  }
  await attachDebugger(tabId);  // attach + glow + set attachedTabId
  return { tabId };
}

// ---------------------------------------------------------------------------
// Tab name session (AI-controlled tab group title)
// ---------------------------------------------------------------------------

async function handleNameSession(msg: JsonRpcRequest): Promise<{ success: true }> {
  const params = msg.params as { title?: string } | undefined;
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

// ---------------------------------------------------------------------------
// CDP permission sync (from bridge)
// ---------------------------------------------------------------------------

async function handleSetCdpPermission(msg: JsonRpcRequest): Promise<{ success: true }> {
  // This is a no-op on the extension side — permissions are enforced in Go.
  // The bridge updates its own config; we just acknowledge receipt.
  return { success: true };
}

// ---------------------------------------------------------------------------
// Quick tools
// ---------------------------------------------------------------------------

async function toggleEditor(): Promise<boolean> {
  if (editorActive) {
    // Remove editor — reload active tab to clear injected scripts
    const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
    if (tabs.length && tabs[0].id) {
      try { await chrome.scripting.executeScript({
        target: { tabId: tabs[0].id },
        func: () => {
          document.querySelectorAll('[data-mcp-editor]').forEach(el => el.remove());
          document.querySelectorAll('[contenteditable][data-mcp-editable]').forEach(el => {
            el.removeAttribute('contenteditable');
            el.removeAttribute('data-mcp-editable');
          });
        }
      }); } catch {}
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

async function toggleAnnotation(): Promise<boolean> {
  if (annotationActive) {
    const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
    if (tabs.length && tabs[0].id) {
      try { await chrome.scripting.executeScript({
        target: { tabId: tabs[0].id },
        func: () => {
          document.querySelectorAll('[data-mcp-annotation]').forEach(el => el.remove());
        }
      }); } catch {}
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

// ---------------------------------------------------------------------------
// Tab group lifecycle
// ---------------------------------------------------------------------------

chrome.tabGroups.onRemoved.addListener(() => {
  mcpTabGroupId = null;
  saveTabGroupState();
  broadcastStatusChanged();
});

chrome.tabs.onRemoved.addListener((tabId: number) => {
  // If the closed tab was in our group, check if group is now empty
  if (mcpTabGroupId !== null) {
    // Use setTimeout to let Chrome update tab state
    setTimeout(async () => {
      try {
        const tabs = await chrome.tabs.query({ groupId: mcpTabGroupId! });
        if (tabs.length === 0) {
          mcpTabGroupId = null;
          saveTabGroupState();
          broadcastStatusChanged();
        }
      } catch {
        // Group may have been auto-removed
        mcpTabGroupId = null;
        saveTabGroupState();
      }
    }, 100);
  }
});

// Auto-group tabs opened FROM the MCP-controlled page (Drift-G).
//
// handleTabsCreate only groups tabs the AI opens via browser_new_tab (the
// tabs.create JSON-RPC path). But clicking a page element that window.opens /
// target=_blanks a new tab (Tencent Docs "空白文档" template card → editor tab
// at docs.qq.com/doc/<id>?...&is_blank_or_template=blank) goes through Chrome's
// own tab creation, NOT handleTabsCreate — so the new tab was left ungrouped in
// the user's tab strip (real-machine Drift-G: "标签组丢了，直接在浏览器标签页上
// 操作了"). chrome.tabs.onCreated fires for ALL new tabs, including window.open;
// when the opener is our attached tab (or any tab already in the MCP group),
// fold the new tab into the group. This mirrors handleTabsCreate's self-healing
// (re-validate groupId, recreate if stale).
chrome.tabs.onCreated.addListener((tab: chrome.tabs.Tab) => {
  if (tab.id === undefined) return;
  // Only group tabs opened from a tab we control. openerTabId is set by Chrome
  // when the tab was opened via window.open / target=_blank / link click from
  // another tab. Ctrl+T / opening-from-another-window have no opener → leave
  // them alone (user-initiated, not AI-driven).
  const opener = tab.openerTabId;
  const fromMcpTab = opener !== undefined && (opener === attachedTabId || opener === tab.id);
  if (!fromMcpTab) return;

  (async () => {
    try {
      // D3: serialize group creation. The double-check inside
      // ensureMcpGroupForTab reuses an existing group if a concurrent
      // tabs.create/onCreated already populated mcpTabGroupId.
      await withGroupLock(() => ensureMcpGroupForTab(tab.id!));
    } catch (e) {
      console.warn("[BrowserMCP] onCreated grouping failed:", e);
    }
  })();
});

// Re-inject the AI-active breathing glow after the attached tab navigates.
// A full navigation destroys the page-scoped overlay <div>/<style> that
// injectGlow created, so without re-injection the glow vanishes the moment
// the AI drives a browser_navigate (real-machine Drift-D: glow "时有时无").
// `tabs` permission is already granted; no new permission needed.
chrome.tabs.onUpdated.addListener((tabId, changeInfo, tab) => {
  if (attachedTabId !== tabId) return;
  if (changeInfo.status !== "complete") return;
  // Only re-inject for http(s)/file pages the debugger can script; skip
  // chrome://, devtools://, etc. where Runtime.evaluate would throw.
  const url = tab?.url ?? "";
  if (!url.startsWith("http") && !url.startsWith("file:")) return;
  injectGlow(tabId).catch(() => { /* page may have detached; ignore */ });
});

async function saveTabGroupState(): Promise<void> {
  try {
    await chrome.storage.local.set({
      TAB_GROUP: { mcpTabGroupId, mcpGroupTitle }
    });
  } catch {}
}

async function restoreTabGroupState(): Promise<void> {
  try {
    const result = await chrome.storage.local.get("TAB_GROUP");
    if (result.TAB_GROUP) {
      mcpGroupTitle = PROFILE.TAB_GROUP_TITLE;  // always use code default (ignore stale storage)
      // mcpTabGroupId may be stale after service worker restart —
      // verify the group still exists
      if (result.TAB_GROUP.mcpTabGroupId != null) {
        try {
          await chrome.tabGroups.get(result.TAB_GROUP.mcpTabGroupId);
          mcpTabGroupId = result.TAB_GROUP.mcpTabGroupId;
        } catch {
          mcpTabGroupId = null;
        }
      }
    }
  } catch {}
}

// Idle-timeout persistence (chrome.storage.local — MV3 extension storage,
// not subject to the "no %TEMP%" contract which covers host-side files).
// Restored on SW revival so the configured value survives restarts.
async function restoreIdleTimeout(): Promise<void> {
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

async function persistIdleTimeout(ms: number): Promise<void> {
  try {
    await chrome.storage.local.set({ IDLE_TIMEOUT_MS: ms });
  } catch {}
}

// ---------------------------------------------------------------------------
// Debugger attach / detach
// ---------------------------------------------------------------------------
// AI-active breathing glow border (content script injection)
// ---------------------------------------------------------------------------

async function injectGlow(tabId: number): Promise<void> {
  try {
    // Use CDP Runtime.evaluate (debugger already attached, no extra host_permissions needed)
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
      `,
    });
  } catch (e) {
    console.warn('[BrowserMCP] Glow inject failed:', e);
  }
}

async function removeGlow(tabId: number): Promise<void> {
  try {
    await chrome.debugger.sendCommand({ tabId }, "Runtime.evaluate", {
      expression: `
        var el = document.getElementById('mcp-glow-overlay');
        if (el) el.remove();
      `,
    });
  } catch {}
}

// ---------------------------------------------------------------------------

async function attachDebugger(tabId: number): Promise<void> {
  if (attachedTabId === tabId) return;
  if (attachedTabId !== null) await detachDebugger();
  try {
    await chrome.debugger.attach({ tabId }, CDP_VERSION);
    attachedTabId = tabId;
    console.info("[BrowserMCP] Debugger attached to tab", tabId);
    // Inject AI-active breathing glow border (awaited so the overlay is present
    // before attach returns — fixes Drift-D timing race where a fast navigate
    // fired before the fire-and-forget injection landed).
    await injectGlow(tabId);
    resetIdleAlarm();  // arm idle收尾 deadline for this control session
  } catch (e: unknown) {
    const message = e instanceof Error ? e.message : String(e);
    if (message.includes("Already attached")) { attachedTabId = tabId; await injectGlow(tabId); } else { throw e; }
  }
}

async function detachDebugger(): Promise<void> {
  if (attachedTabId === null) return;
  const tabId = attachedTabId;
  attachedTabId = null;
  clearIdleAlarm();
  // Remove AI-active glow border
  removeGlow(tabId);
  try {
    await chrome.debugger.detach({ tabId });
    console.info("[BrowserMCP] Debugger detached from tab", tabId);
  } catch (e: unknown) {
    const message = e instanceof Error ? e.message : String(e);
    // "Not attached" = already detached (idempotent path). "No tab with id"
    // (older Chrome) / "No tab with given id" (current Chrome) = tab was
    // closed before detach ran. Both are expected (external detach / tab
    // gone) — silence so the SW console stays clean for real errors.
    if (
      !message.includes("Not attached") &&
      !message.includes("No tab with id") &&
      !message.includes("No tab with given id")
    ) {
      console.warn("[BrowserMCP] Error detaching debugger", message);
    }
  }
}

chrome.debugger.onDetach.addListener((source: chrome.debugger.Debuggee) => {
  if (source.tabId === attachedTabId) {
    console.info("[BrowserMCP] Debugger detached externally", attachedTabId);
    // External detach (user opened DevTools / tab closed) — dissolve active
    // control state consistently with the other disconnect paths.
    dissolveActiveControl().catch(() => { /* already cleared */ });
  }
});

// ---------------------------------------------------------------------------
// Graceful disconnect (graceful-disconnect change)
// ---------------------------------------------------------------------------

// dissolveActiveControl is the canonical disconnect dissolve
// (control-safety-boundary: Disconnect Dissolves Active Control And Releases
// The Group). Clears active control state (attachedTabId + glow + idle alarm)
// AND releases the tab group (aligned with Codex releaseTabsFromManagedGroups
// + tabLeases.releaseTabs): ungroups managed-group tabs and clears
// mcpTabGroupId. Tabs STAY OPEN (ungroup does not close them) so the user can
// view results / resume next session — the group is a "currently controlled"
// indicator, not an "ever-controlled" marker, so disconnect exits the group.
// Next control (tabs.create / tabs.adopt) re-creates the group. Only ungroups
// the MCP-managed group; user-owned groups are untouched.
// Idempotent: removeGlow/detachDebugger/ungroup are try-catch safe.
// Does NOT disconnect the NM port — connection preserved for reconnect.
async function dissolveActiveControl(): Promise<void> {
  if (attachedTabId !== null) {
    const tabId = attachedTabId;
    await removeGlow(tabId).catch(() => { /* page may be gone */ });
    await detachDebugger().catch(() => { /* already detached */ });
  }
  clearIdleAlarm();
  // Release the managed tab group (Codex releaseTabsFromManagedGroups).
  if (mcpTabGroupId !== null) {
    const groupId = mcpTabGroupId;
    mcpTabGroupId = null;
    try {
      const tabs = await chrome.tabs.query({ groupId });
      if (tabs.length) {
        const ids = tabs.map((t) => t.id).filter((id): id is number => id !== undefined);
        if (ids.length) await chrome.tabs.ungroup(ids);
      }
    } catch (e) {
      console.warn("[BrowserMCP] ungroup on dissolve failed:", e);
    }
    saveTabGroupState();
  }
  // D4 / E item: sweep ALL same-named leftover groups, not just the current
  // pointer. Catches cross-session / SW-restart orphans (G1/G2) that
  // mcpTabGroupId never pointed at. Idempotent.
  await sweepOrphanMcpGroups();
}

// releaseActiveTab is the light-release counterpart to dissolveActiveControl.
// session.start calls this on a new MCP session: removes glow + detaches
// debugger + clears attachedTabId, but KEEPS mcpTabGroupId (the group is
// preserved so the new session's new_tab joins the existing group — matches
// the "one group" UX verified in tab-group-lifecycle-hygiene scenario B).
// Distinct from dissolveActiveControl (full clear: ungroup + sweep) used by
// session.end / idle / disconnect. Idempotent + try-catch safe.
// control-safety-boundary: only releases the pinned attached tab; does NOT
// ungroup or close tabs (user tabs stay open).
async function releaseActiveTab(): Promise<void> {
  if (attachedTabId === null) return;
  const tabId = attachedTabId;
  await removeGlow(tabId).catch(() => { /* page may be gone */ });
  await detachDebugger().catch(() => { /* already detached */ });
  attachedTabId = null;
  // mcpTabGroupId intentionally preserved — no ungroup, no sweep.
}

// handleSessionStart is the entry for the bridge "session.start" notification.
// Releases the prior session's attached tab so the new session must open its
// own tab (per-session isolation), preventing navigate-hijack of the prior tab.
async function handleSessionStart(): Promise<{ ok: true }> {
  await releaseActiveTab();
  return { ok: true };
}

// handleSessionEnd is the shared收尾 for both the explicit session.end RPC
// (finish_session tool) and the idle-timeout fallback. Routes through
// dissolveActiveControl so all disconnect paths share one clear-state routine.
async function handleSessionEnd(): Promise<{ ok: true }> {
  await dissolveActiveControl();
  return { ok: true };
}

// resetIdleAlarm arms (or re-arms) the idle alarm: on any CDP/MCP activity,
// push the收尾 deadline out by idleTimeoutMs. chrome.alarms survives SW
// teardown, so this fires even if the service worker was suspended (unlike
// setTimeout). Production chrome clamps delayInMinutes to >= 1 minute, so
// values < 60s effectively become 60s — documented, acceptable.
function resetIdleAlarm(): void {
  if (attachedTabId === null) { clearIdleAlarm(); return; }
  const effectiveMs = Math.max(idleTimeoutMs, IDLE_TIMEOUT_MIN_MS);
  const delayInMinutes = effectiveMs / 60000;
  chrome.alarms.create(IDLE_ALARM, { delayInMinutes });
  idleArmed = true;
}

function clearIdleAlarm(): void {
  if (!idleArmed) return;
  chrome.alarms.clear(IDLE_ALARM);
  idleArmed = false;
}

// ---------------------------------------------------------------------------
// Response sending
// ---------------------------------------------------------------------------

function sendResponse(resp: JsonRpcResponse): void {
  if (!port) { console.warn("[BrowserMCP] Cannot send response — NM port is null"); return; }
  try { port.postMessage(resp); } catch (e) { console.warn("[BrowserMCP] Failed to post response (port dead):", e); }
}

// ---------------------------------------------------------------------------
// Health check alarm
// ---------------------------------------------------------------------------

// Track the last-known nm_connected so we only flip the glow overlay on
// transitions, not every health poll (Drift-F: glow must reflect real bridge
// connection health — "still breathing while bridge is erroring" was a false
// "alive" signal. removeGlow was only ever called on detach, so a bridge that
// errored without the NM port dying left the overlay breathing indefinitely.)
let lastNmConnected: boolean | null = null;

async function checkHealth(): Promise<void> {
  try {
    const resp = await fetch(HEALTH_URL);
    cachedHealth = await resp.json();
  } catch {
    cachedHealth = null;
  }
  reportVersion(); // piggyback: keep bridge's extension_version fresh each poll
  syncGlowWithHealth();
  broadcastStatusChanged();
}

// Bind the AI-active glow to the real bridge connection state. On a
// false→true transition (re)inject the overlay; on true→false extinguish it.
// No-op when no tab is attached (nothing to glow on).
function syncGlowWithHealth(): void {
  const connected = cachedHealth?.nm_connected === true;
  if (connected === lastNmConnected) return; // no transition
  lastNmConnected = connected;
  if (attachedTabId === null) return;
  if (connected) {
    injectGlow(attachedTabId).catch(() => { /* page may not be scriptable yet */ });
  } else {
    removeGlow(attachedTabId).catch(() => { /* debugger may be gone */ });
  }
}

function setupHealthCheck(): void {
  chrome.alarms.create(HEALTH_ALARM, { periodInMinutes: HEALTH_INTERVAL_MIN });
  checkHealth(); // immediate first check, don't wait for first alarm
  console.info("[BrowserMCP] Health check alarm created");
}

chrome.alarms.onAlarm.addListener(async (alarm: chrome.alarms.Alarm) => {
  if (alarm.name === HEARTBEAT_ALARM) {
    if (!port) { connecting = false; detachDebugger(); connectNative(); return; }
    try { port.postMessage({ jsonrpc: "2.0", method: "ping" }); }
    catch (e) { console.warn("[BrowserMCP] Heartbeat: port dead, reconnecting"); port = null; connecting = false; detachDebugger(); connectNative(); }
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
    idleArmed = false;  // alarm consumed
    if (attachedTabId !== null) {
      handleSessionEnd().catch((e) => console.warn("[BrowserMCP] idle收尾 failed:", e));
    }
    return;
  }
});

// ---------------------------------------------------------------------------
// Heartbeat (existing)
// ---------------------------------------------------------------------------

function setupHeartbeat(): void {
  chrome.alarms.create(HEARTBEAT_ALARM, { periodInMinutes: HEARTBEAT_INTERVAL_MIN });
  console.info("[BrowserMCP] Heartbeat alarm created");
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// initConnect restores the persisted userDisconnected flag (so a sticky
// disconnect that survived a SW suspension is honored) BEFORE the first
// connectNative — otherwise the init connectNative would bypass the sticky
// state + relaunch bridge. Async; safe to fire-and-forget at init.
async function initConnect(): Promise<void> {
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
  // D5 / G item: clean up same-named orphan groups left from a prior SW
  // lifecycle (mcpTabGroupId is in-memory and lost on SW teardown). Idempotent.
  sweepOrphanMcpGroups().catch((e) => console.warn("[BrowserMCP] startup orphan sweep failed:", e));
});

chrome.runtime.onStartup.addListener(() => {
  console.info("[BrowserMCP] Browser started");
  setupHeartbeat();
  setupHealthCheck();
  void initConnect();
  restoreTabGroupState();
  restoreIdleTimeout();
  // D5 / G item: same sweep on browser startup.
  sweepOrphanMcpGroups().catch((e) => console.warn("[BrowserMCP] startup orphan sweep failed:", e));
});

setupHeartbeat();
setupHealthCheck();
void initConnect();
restoreTabGroupState();
restoreIdleTimeout();
// D5 / G item: top-level SW cold-start activation path (runs on every SW
// wakeup that is not an onInstalled/onStartup event). Idempotent.
sweepOrphanMcpGroups().catch((e) => console.warn("[BrowserMCP] cold-start orphan sweep failed:", e));
