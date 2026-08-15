/**
 * @deepseek-ai/dsh-easybrowser — Cordis plugin wrapping easybrowser as a
 * managed sidecar service. Provides:
 *
 * - easybrowser bridge lifecycle management (start/stop/restart)
 * - MCP client integration for AI agent browser control
 * - Chrome extension packaging and installation
 * - Health check and auto-recovery
 *
 * Depends on @deepseek-ai/dsh-windows-launcher for sidecar management.
 */

import { Context, Service } from '@deepseek-ai/cordis'
import z from '@deepseek-ai/schemastery'

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

/**
 * easybrowser bridge service status.
 */
export type BridgeStatus = 'stopped' | 'starting' | 'running' | 'error'

/**
 * Wraps easybrowser as a Cordis service, providing lifecycle management
 * and MCP client integration for the AI agent.
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

    ctx.on('dispose', () => {
      this.stop()
    })
  }

  /**
   * Start the easybrowser bridge process.
   * Delegates to windowsLauncher sidecar management.
   */
  start(): void {
    if (this._status === 'running' || this._status === 'starting') {
      this.ctx.logger.warn('easybrowser: bridge is already running or starting')
      return
    }

    this._status = 'starting'
    this.ctx.logger.info('easybrowser: starting bridge')

    try {
      // Use windowsLauncher to manage sidecar if available
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
    // Brief delay to ensure clean shutdown
    await new Promise(resolve => setTimeout(resolve, 1000))
    this.start()
  }

  /**
   * MCP tool: control browser via easybrowser.
   * This is the primary interface for AI agents to interact with the browser.
   */
  async browserAction(action: string, params: Record<string, unknown>): Promise<unknown> {
    if (this._status !== 'running') {
      throw new Error('easybrowser bridge is not running')
    }
    this.ctx.logger.debug(`easybrowser: browser action ${action}`)
    // TODO: implement MCP JSON-RPC call to bridge
    return {}
  }

  /**
   * Get the current bridge health status.
   */
  async health(): Promise<{ status: string; uptime: number }> {
    // TODO: implement health check via bridge HTTP API
    return { status: this._status, uptime: 0 }
  }
}

export default EasyBrowserService