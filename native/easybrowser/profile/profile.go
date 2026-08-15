// Package profile holds the build-time-baked identity values that differ
// between a TEST build and a PROD build of the bridge, so the two can coexist
// on the same machine (different port, data dir, update channel, client-config
// key, binary name). See profiles/<env>.env + scripts/build.ps1 -Profile.
//
// All vars default to the TEST profile so a bare `go build` (no ldflags)
// reproduces the historical test-only behavior. build.ps1 -Profile prod bakes
// prod values via `-X github.com/browser-mcp/bridge/profile.<Var>=<val>`.
// ldflag -X only supports STRING values, so Port/BinSuffix are strings parsed
// at the call site.
//
// Why a per-module package (not shared across bridge/desktop/nm-host): the
// three are independent Go modules (separate go.mod) and cannot share an
// import path. scripts/profile.ps1 reads ONE profiles/<env>.env and bakes the
// same logical value into each module's profile package via its own -X target.
package profile

// Port is the MCP server listen port (string form, parsed to int at the call
// site). Test "58080", prod "58081" — two bridges bind different ports so they
// run concurrently without addr-in-use.
var Port = "58080"

// DataDirName is the data directory basename under the user home. Test
// ".browser-mcp", prod ".browser-mcp-prod". BROWSER_MCP_DATA_DIR env still
// overrides the resolved path entirely (isolation/testing). The dir name
// drives the named-pipe hash (transport) and the config/lock/log locations,
// so a different name separates the whole on-disk footprint.
var DataDirName = ".browser-mcp"

// UpdateBase is the http update-source base URL (version.json lives at
// <UpdateBase>/version.json, assets at <UpdateBase>/assets/...). Test
// tool-hub (10.141.103.6:7443), prod tool-hub (10.249.19.32). The bridge's
// self-update + telemetry host both derive from this, so baking it retargets
// the whole update+telemetry channel.
//
// HARDCODED (hardcode-update-base-url change): newUpdateSource() always reads
// this var for the http backend — config file update_base field and env
// BROWSER_MCP_UPDATE_BASE / BROWSER_MCP_DIST_URL are no longer honored (field
// removed from config schema, env constants deleted). This is build-time
// baked via ldflag from profiles/{test,prod}.env; runtime has no override
// path. See CLAUDE.md 更新通道 contract.
var UpdateBase = "http://10.141.103.6:7443/tool-hub/browser-mcp/stable"

// McpServerName is the mcpServers entry key injected into AI-client configs
// (.claude.json mcpServers.<name>, Codex [mcp_servers.<name>], OpenCode
// mcp.<name>). Test "browser-mcp", prod "browser-mcp-prod" so two coexisting
// installs hold separate slots in the same client config file.
var McpServerName = "browser-mcp"

// BackupSuffix is the suffix for the client-config backup file written before
// injection. Test ".browser-mcp.bak", prod ".browser-mcp-prod.bak".
var BackupSuffix = ".browser-mcp.bak"

// BinSuffix is the bridge binary name suffix ("", or "-prod" for prod). Used
// so isBridgeProcess recognizes the profile's own binary image name
// (bridge.exe vs bridge-prod.exe) and the desktop installer's cleanupStale
// kills only the profile's processes (no cross-profile kill).
var BinSuffix = ""

// AutostartKey is the per-user autostart identifier (test "BrowserMCP", prod
// "BrowserMCPProd"). Used by bridge's --boot self-heal registration
// (harden-bridge-autostart, 2026-07-08): the HKCU Run entry name is
// AutostartKey+"Bridge" (= desktop/autostart's bridgeRunValueName, kept in
// sync). Baked via build.ps1 -X ...bridge/profile.AutostartKey=<val> so bridge
// (independent module, can't import desktop/autostart) computes the same entry
// name the installer registered.
var AutostartKey = "BrowserMCP"

// Channel is the telemetry channel tag sent in the install report body
// (POST /api/telemetry/install {client_version, channel}). Test "sit", prod
// "stable" — lets the stats panel group installs by channel so prod/sit
// coexisting on the same IP are still distinguishable. config.UpdateChannel
// (env BROWSER_MCP_UPDATE_CHANNEL) overrides at runtime; this is the baked
// default when neither env nor config file sets it. Empty = server records
// channel=null.
var Channel = ""
