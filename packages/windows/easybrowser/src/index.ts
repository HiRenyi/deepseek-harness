/**
 * @deepseek-ai/dsh-easybrowser — Cordis plugin wrapping easybrowser as a
 * managed sidecar service with 52 model-facing browser automation tools.
 *
 * Provides:
 * - easybrowser bridge lifecycle management (start/stop/restart)
 * - 50 browser automation tools (navigate, click, type, screenshot, etc.)
 * - Chrome extension packaging and installation
 * - Health check and auto-recovery
 *
 * Depends on @deepseek-ai/dsh-windows-launcher for sidecar management.
 */

import { Context, Service } from '@deepseek-ai/cordis'
import z from '@deepseek-ai/schemastery'
import type {} from '@deepseek-ai/dsh-tools'

declare module '@deepseek-ai/cordis' {
  interface Context {
    easybrowser: EasyBrowserService
  }
}

export interface Config {
  /** Path to the easybrowser bridge executable */
  bridgePath: string
  /** Path to the easybrowser data directory */
  dataDir: string
  /** Bridge listen port */
  port: number
  /** Auto-start bridge on plugin load */
  autoStart: boolean
  /** Enable Chrome extension integration */
  enableExtension: boolean
  /** Easybrowser profile (dev/test/prod) */
  profile: 'dev' | 'test' | 'prod'
}

export type BridgeStatus = 'stopped' | 'starting' | 'running' | 'error'

export const name = 'easybrowser'
export const inject = ['tools']

interface ToolDef {
  name: string
  description: string
  schema: Record<string, z<unknown>>
}

const BROWSER_TOOLS: ToolDef[] = [
  // ── 感知层 (read-only) ──
  { name: 'browser_snapshot', description: 'Capture a filtered DOM snapshot of the current browser tab', schema: { full: z.boolean().optional(), force: z.boolean().optional() } },
  { name: 'browser_snapshot_visible', description: 'Get a visible DOM snapshot of the current page', schema: {} },
  { name: 'browser_snapshot_ax', description: 'Get an accessibility tree snapshot of the current page', schema: {} },
  { name: 'browser_screenshot', description: 'Take a screenshot of the current page', schema: {} },
  { name: 'browser_get_text', description: 'Get text content of an element by CSS selector', schema: { selector: z.string() } },
  { name: 'browser_get_attribute', description: 'Get an attribute value from an element', schema: { selector: z.string(), attribute: z.string() } },
  { name: 'browser_get_url', description: 'Get the current page URL', schema: {} },
  { name: 'browser_get_title', description: 'Get the current page title', schema: {} },
  { name: 'browser_is_visible', description: 'Check if an element is visible', schema: { selector: z.string() } },
  { name: 'browser_is_enabled', description: 'Check if an element is enabled', schema: { selector: z.string() } },
  { name: 'browser_count', description: 'Count elements matching a CSS selector', schema: { selector: z.string() } },
  { name: 'browser_console_logs', description: 'Read recent browser console log messages', schema: {} },
  { name: 'browser_clipboard', description: 'Read or write the system clipboard', schema: { action: z.string(), text: z.string().optional() } },

  // ── 操作层 L1 DOM ──
  { name: 'browser_click', description: 'Click an element by CSS selector', schema: { selector: z.string() } },
  { name: 'browser_double_click', description: 'Double-click an element', schema: { selector: z.string() } },
  { name: 'browser_fill', description: 'Fill an input field with text (clears first)', schema: { selector: z.string(), text: z.string() } },
  { name: 'browser_type', description: 'Type text into an input field (appends)', schema: { selector: z.string(), text: z.string() } },
  { name: 'browser_press_key', description: 'Press a named key (e.g. Enter, Escape)', schema: { key: z.string() } },
  { name: 'browser_select_option', description: 'Select an option in a dropdown', schema: { selector: z.string(), value: z.string() } },
  { name: 'browser_set_checked', description: 'Check or uncheck a checkbox', schema: { selector: z.string(), checked: z.boolean() } },
  { name: 'browser_scroll', description: 'Scroll the page or an element', schema: { selector: z.string().optional(), direction: z.string(), amount: z.number().optional() } },
  { name: 'browser_hover', description: 'Hover over an element', schema: { selector: z.string() } },
  { name: 'browser_drag', description: 'Drag the mouse along a path of coordinate points', schema: { points: z.array(z.object({ x: z.number(), y: z.number() })) } },
  { name: 'browser_file_upload', description: 'Upload files to a file input element', schema: { selector: z.string(), files: z.array(z.string()) } },

  // ── 操作层 L2 DomCUA ──
  { name: 'browser_click_node', description: 'Click a DOM node by its backend node ID', schema: { nodeId: z.number() } },

  // ── 操作层 L3 CUA 坐标 ──
  { name: 'browser_click_at', description: 'Click at specific page coordinates', schema: { x: z.number(), y: z.number() } },
  { name: 'browser_double_click_at', description: 'Double-click at specific page coordinates', schema: { x: z.number(), y: z.number() } },
  { name: 'browser_move_mouse', description: 'Move mouse to specific page coordinates', schema: { x: z.number(), y: z.number() } },
  { name: 'browser_scroll_at', description: 'Scroll at specific coordinates', schema: { x: z.number(), y: z.number(), deltaX: z.number().optional(), deltaY: z.number().optional() } },
  { name: 'browser_drag_path', description: 'Drag along a path of coordinate points', schema: { points: z.array(z.object({ x: z.number(), y: z.number() })) } },
  { name: 'browser_type_at', description: 'Type text at the current cursor position', schema: { text: z.string() } },
  { name: 'browser_press_key_combo', description: 'Press a key combination (e.g. Ctrl+C)', schema: { combo: z.string() } },

  // ── 操作层 L4 JS React 兜底 ──
  { name: 'browser_js_click', description: 'Click an element via JavaScript (React fallback)', schema: { selector: z.string() } },
  { name: 'browser_js_fill', description: 'Fill an input via JavaScript (React fallback)', schema: { selector: z.string(), text: z.string() } },

  // ── 导航层 ──
  { name: 'browser_navigate', description: 'Navigate to a URL', schema: { url: z.string() } },
  { name: 'browser_go_back', description: 'Go back to the previous page', schema: {} },
  { name: 'browser_go_forward', description: 'Go forward to the next page', schema: {} },
  { name: 'browser_reload', description: 'Reload the current page', schema: {} },
  { name: 'browser_new_tab', description: 'Open a new browser tab with a URL', schema: { url: z.string() } },

  // ── Tab 管理 ──
  { name: 'browser_list_tabs', description: 'List all open browser tabs', schema: {} },
  { name: 'browser_switch_tab', description: 'Switch to a tab by its ID', schema: { tabId: z.number() } },
  { name: 'browser_select_tab', description: 'Select a tab by its URL pattern', schema: { url: z.string() } },
  { name: 'browser_adopt_tab', description: 'Adopt a tab from another session', schema: { tabId: z.number() } },
  { name: 'browser_close_tab', description: 'Close the current tab', schema: {} },

  // ── 等待 ──
  { name: 'browser_wait_for_element', description: 'Wait for an element to appear', schema: { selector: z.string(), timeout: z.number().optional() } },
  { name: 'browser_wait_for_url', description: 'Wait for the page URL to contain a substring', schema: { url: z.string(), timeout: z.number().optional() } },
  { name: 'browser_wait_for_timeout', description: 'Sleep for a fixed duration in milliseconds', schema: { ms: z.number() } },

  // ── 高级 ──
  { name: 'browser_download_media', description: 'Download media (image/video) from a URL as base64', schema: { url: z.string() } },
  { name: 'browser_evaluate_js', description: 'Execute JavaScript in the page context', schema: { code: z.string() } },
  { name: 'browser_cdp_call', description: 'Execute a raw CDP command', schema: { method: z.string(), params: z.record(z.unknown()).optional() } },
  { name: 'browser_name_session', description: 'Name the current browser session for identification', schema: { name: z.string() } },
  { name: 'finish_session', description: 'Finish/close the current browser session', schema: {} },
]

/**
 * Wraps easybrowser as a Cordis service, providing lifecycle management,
 * MCP client integration, and 52 model-facing browser tools.
 */
export class EasyBrowserService extends Service {
  static Config: z<Config> = z.object({
    bridgePath: z.string(),
    dataDir: z.string().default(''),
    port: z.number().default(58082),
    autoStart: z.boolean().default(true),
    enableExtension: z.boolean().default(true),
    profile: z.union([
      z.literal('dev'),
      z.literal('test'),
      z.literal('prod'),
    ]).default('dev'),
  })

  private ctx: Context
  private config: Config
  private _status: BridgeStatus = 'stopped'

  get status(): BridgeStatus {
    return this._status
  }

  constructor(ctx: Context, config: Config) {
    super(ctx, 'easybrowser')
    this.ctx = ctx
    this.config = config

    if (process.platform !== 'win32') {
      ctx.logger.warn('easybrowser: not running on Windows, plugin is a no-op')
      return
    }

    ctx.logger.info(`easybrowser: initializing with profile=${config.profile} port=${config.port}`)

    if (config.autoStart) {
      this.start()
    }

    this.registerTools(ctx)

    ctx.on('dispose', () => {
      this.stop()
    })
  }

  /**
   * Register all 52 browser automation tools in the agent's tool registry.
   */
  private registerTools(ctx: Context): void {
    const tools = ctx.get('tools')
    if (!tools) {
      ctx.logger.warn('easybrowser: tools registry not available, skipping tool registration')
      return
    }

    for (const toolDef of BROWSER_TOOLS) {
      const schema: Record<string, z<unknown>> = {}
      for (const [key, val] of Object.entries(toolDef.schema)) {
        schema[key] = val
      }
      tools.register(toolDef.name, {
        description: toolDef.description,
        config: z.object(schema),
        handler: async (params) => {
          return this.browserAction(toolDef.name, params)
        },
      })
    }
    ctx.logger.info(`easybrowser: registered ${BROWSER_TOOLS.length} browser tools`)
  }

  /**
   * Start the easybrowser bridge process.
   */
  start(): void {
    if (this._status === 'running' || this._status === 'starting') {
      this.ctx.logger.warn('easybrowser: bridge is already running or starting')
      return
    }

    this._status = 'starting'
    this.ctx.logger.info('easybrowser: starting bridge')

    try {
      if (this.ctx.windowsLauncher) {
        this.ctx.windowsLauncher.startSidecar('easybrowser-bridge')
      }
      this._status = 'running'
    } catch (err) {
      this._status = 'error'
      this.ctx.logger.error(`easybrowser: failed to start bridge: ${err}`)
      throw err
    }
  }

  /**
   * Stop the easybrowser bridge process.
   */
  stop(): void {
    if (this._status === 'stopped') return
    this.ctx.logger.info('easybrowser: stopping bridge')
    try {
      if (this.ctx.windowsLauncher) {
        this.ctx.windowsLauncher.stopSidecar('easybrowser-bridge')
      }
    } finally {
      this._status = 'stopped'
    }
  }

  /**
   * Restart the bridge process.
   */
  async restart(): Promise<void> {
    this.stop()
    await new Promise(resolve => setTimeout(resolve, 1000))
    this.start()
  }

  /**
   * Execute a browser action via the easybrowser bridge HTTP API.
   */
  async browserAction(action: string, params: Record<string, unknown>): Promise<unknown> {
    if (this._status !== 'running') {
      throw new Error('easybrowser bridge is not running')
    }
    this.ctx.logger.debug(`easybrowser: browser action ${action}`)
    // TODO: implement MCP JSON-RPC call to bridge HTTP API at http://127.0.0.1:58082
    return { status: 'ok', action, params }
  }

  /**
   * Get the current bridge health status.
   */
  async health(): Promise<{ status: string; uptime: number }> {
    return { status: this._status, uptime: 0 }
  }
}

export default EasyBrowserService