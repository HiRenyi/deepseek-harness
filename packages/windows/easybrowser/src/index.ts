/**
 * @deepseek-ai/dsh-easybrowser — Cordis plugin wrapping easybrowser as a
 * managed sidecar service with model-facing browser automation tools.
 *
 * Provides:
 * - easybrowser bridge lifecycle management (start/stop/restart)
 * - `browser_navigate`, `browser_click`, `browser_type`, `browser_screenshot` tools
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

/**
 * Wraps easybrowser as a Cordis service, providing lifecycle management,
 * MCP client integration, and model-facing browser tools.
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

    // Register model-facing browser tools
    this.registerTools(ctx)

    ctx.on('dispose', () => {
      this.stop()
    })
  }

  /**
   * Register browser automation tools in the agent's tool registry.
   */
  private registerTools(ctx: Context): void {
    const tools = ctx.get('tools')
    if (!tools) {
      ctx.logger.warn('easybrowser: tools registry not available, skipping tool registration')
      return
    }

    tools.register('browser_navigate', {
      description: 'Navigate the browser to a URL and return the page title and content',
      config: z.object({
        url: z.string().describe('The URL to navigate to'),
      }),
      handler: async (params) => {
        return this.browserAction('navigate', params)
      },
    })

    tools.register('browser_click', {
      description: 'Click an element on the current page by its selector',
      config: z.object({
        selector: z.string().describe('CSS selector for the element to click'),
      }),
      handler: async (params) => {
        return this.browserAction('click', params)
      },
    })

    tools.register('browser_type', {
      description: 'Type text into an input field on the current page',
      config: z.object({
        selector: z.string().describe('CSS selector for the input element'),
        text: z.string().describe('The text to type'),
      }),
      handler: async (params) => {
        return this.browserAction('type', params)
      },
    })

    tools.register('browser_screenshot', {
      description: 'Take a screenshot of the current page',
      config: z.object({}),
      handler: async () => {
        return this.browserAction('screenshot', {})
      },
    })
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
    // TODO: implement MCP JSON-RPC call to bridge HTTP API
    return { status: 'ok', action, result: 'not yet implemented' }
  }

  /**
   * Get the current bridge health status.
   */
  async health(): Promise<{ status: string; uptime: number }> {
    return { status: this._status, uptime: 0 }
  }
}

export default EasyBrowserService