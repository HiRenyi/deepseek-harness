// Package profile holds the build-time-baked identity values that differ
// between a TEST build and a PROD build of nm-host, so test and prod can
// coexist on the same machine (different data dir → different named-pipe hash
// → nm-host connects to the right bridge). See profiles/<env>.env +
// scripts/build.ps1 -Profile.
//
// All vars default to the TEST profile so a bare `go build` (no ldflags)
// reproduces the historical test-only behavior. build.ps1 -Profile prod bakes
// prod values via `-X github.com/browser-mcp/nm-host/profile.<Var>=<val>`.
//
// Why a per-module package (not shared across bridge/desktop/nm-host): the
// three are independent Go modules (separate go.mod) and cannot share an
// import path. scripts/profile.ps1 reads ONE profiles/<env>.env and bakes the
// same logical value into each module's profile package via its own -X target.
//
// nm-host is identity-agnostic on the wire (it only speaks stdio + the inner
// transport), but it must LOCATE bridge: the data dir basename drives the
// named-pipe hash and the addr.json/token file paths. A prod nm-host pointing
// at the test data dir would compute the wrong pipe name and fail to connect.
// Only DataDirName is needed here — Port/UpdateBase/McpServerName are
// bridge-only concerns.
package profile

// DataDirName is the data directory basename under the user home. Test
// ".browser-mcp", prod ".browser-mcp-prod". BROWSER_MCP_DATA_DIR env still
// overrides the resolved path entirely (isolation/testing). The dir name
// drives the named-pipe hash (transport.DeterministicPipeName) and the
// addr.json/lock/log/token locations, so a different name separates the whole
// on-disk footprint and keeps nm-host paired with its matching bridge.
var DataDirName = ".browser-mcp"

// BinSuffix is the binary filename suffix (test "", prod "-prod") so nm-host's
// launchBridge (nm-host/main.go) locates the matching bridge binary next to
// itself: test "bridge.exe", prod "bridge-prod.exe". bridge-boot-autostart
// (2026-07-08) fix: previously launchBridge hardcoded "bridge.exe" and could
// not find "bridge-prod.exe" on prod → nm-host's crash-recovery relaunch of
// bridge failed. Baked via build.ps1 -X ...nm-host/profile.BinSuffix=<val>.
var BinSuffix = ""
