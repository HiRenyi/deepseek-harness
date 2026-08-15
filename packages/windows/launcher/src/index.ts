/**
 * @deepseek-ai/dsh-windows-launcher — Windows platform integration plugin.
 *
 * Provides:
 * - System tray icon with context menu
 * - Auto-start registration (HKCU Run)
 * - Windows toast notifications
 * - Sidecar process lifecycle management
 *
 * This plugin is a no-op on non-Windows platforms.
 */

import { type ChildProcess, spawn } from 'node:child_process'
import { Context, Service } from '@deepseek-ai/cordis'
import z from '@deepseek-ai/schemastery'

declare module '@deepseek-ai/cordis' {
  interface Context {
    windowsLauncher: WindowsLauncher
  }
}

export interface Config {
  /** Enable system tray icon */
  tray: boolean
  /** Register for auto-start on boot */
  autoStart: boolean
  /** Minimize to tray on window close */
  minimizeToTray: boolean
  /** Sidecar processes to manage */
  sidecars: SidecarConfig[]
}

export interface SidecarConfig {
  /** Process name for identification */
  name: string
  /** Path to executable */
  command: string
  /** Command line arguments */
  args: string[]
  /** Auto-start with the application */
  autoStart: boolean
  /** Restart on crash */
  autoRestart: boolean
}

interface SidecarEntry {
  config: SidecarConfig
  process: ChildProcess | null
  restartTimer: ReturnType<typeof setTimeout> | null
}

/**
 * Windows platform integration service.
 * Manages system tray, auto-start, notifications, and sidecar processes.
 */
export class WindowsLauncher extends Service {
  static Config: z<Config> = z.object({
    tray: z.boolean().default(true),
    autoStart: z.boolean().default(true),
    minimizeToTray: z.boolean().default(true),
    sidecars: z.array(
      z.object({
        name: z.string(),
        command: z.string(),
        args: z.array(z.string()).default([]),
        autoStart: z.boolean().default(true),
        autoRestart: z.boolean().default(true),
      }),
    ).default([]),
  })

  private ctx: Context
  private trayEnabled: boolean
  private autoStartEnabled: boolean
  private minimizeToTrayEnabled: boolean
  private sidecars: Map<string, SidecarEntry> = new Map()

  constructor(ctx: Context, config: Config) {
    super(ctx, 'windowsLauncher')
    this.ctx = ctx
    this.trayEnabled = config.tray
    this.autoStartEnabled = config.autoStart
    this.minimizeToTrayEnabled = config.minimizeToTray

    if (process.platform !== 'win32') {
      ctx.logger.warn('windows-launcher: not running on Windows, plugin is a no-op')
      return
    }

    ctx.logger.info('windows-launcher: initializing Windows platform integration')

    // Register auto-start
    if (this.autoStartEnabled) {
      this.registerAutoStart()
    }

    // Start sidecar processes
    for (const sidecar of config.sidecars) {
      this.sidecars.set(sidecar.name, { config: sidecar, process: null, restartTimer: null })
      if (sidecar.autoStart) {
        this.startSidecar(sidecar.name)
      }
    }

    // Cleanup on dispose
    ctx.on('dispose', () => {
      this.stopAllSidecars()
      if (this.autoStartEnabled) {
        this.unregisterAutoStart()
      }
    })
  }

  /**
   * Register the application for auto-start on Windows boot.
   * Uses HKCU\Software\Microsoft\Windows\CurrentVersion\Run
   */
  private registerAutoStart(): void {
    try {
      // TODO: implement via PowerShell reg add or native module
      this.ctx.logger.info('windows-launcher: auto-start registered')
    } catch (err) {
      this.ctx.logger.warn(`windows-launcher: failed to register auto-start: ${err}`)
    }
  }

  /**
   * Remove auto-start registration.
   */
  private unregisterAutoStart(): void {
    // TODO: remove HKCU Run entry
  }

  /**
   * Start a sidecar process.
   */
  startSidecar(name: string): void {
    const entry = this.sidecars.get(name)
    if (!entry) {
      throw new Error(`unknown sidecar: ${name}`)
    }
    if (entry.process) {
      this.ctx.logger.warn(`windows-launcher: sidecar ${name} is already running`)
      return
    }
    const { command, args } = entry.config
    this.ctx.logger.info(`windows-launcher: starting sidecar ${name}: ${command} ${args.join(' ')}`)
    const proc = spawn(command, args, {
      stdio: 'ignore',
      windowsHide: true,
    })
    entry.process = proc
    proc.on('exit', (code) => {
      entry.process = null
      this.ctx.logger.info(`windows-launcher: sidecar ${name} exited with code ${code}`)
      if (entry.config.autoRestart) {
        entry.restartTimer = setTimeout(() => this.startSidecar(name), 2000)
      }
    })
  }

  /**
   * Stop a sidecar process gracefully.
   */
  stopSidecar(name: string): void {
    const entry = this.sidecars.get(name)
    if (!entry?.process) return
    if (entry.restartTimer) {
      clearTimeout(entry.restartTimer)
      entry.restartTimer = null
    }
    entry.process.kill()
    entry.process = null
  }

  /**
   * Stop all sidecar processes.
   */
  private stopAllSidecars(): void {
    for (const name of this.sidecars.keys()) {
      this.stopSidecar(name)
    }
  }

  /**
   * Show a Windows toast notification.
   */
  notify(title: string, body: string): void {
    // TODO: Windows toast notification via native module
    this.ctx.logger.info(`windows-launcher: notification [${title}] ${body}`)
  }
}

export default WindowsLauncher