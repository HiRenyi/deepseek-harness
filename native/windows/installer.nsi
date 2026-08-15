; DeepSeek Harness Windows Installer (NSIS)
; Full offline installer — bundles Node.js, Chromium, and all dependencies.
Unicode True

!define PRODUCT_NAME "DeepSeek Harness"
!define PRODUCT_PUBLISHER "HiRenyi"
!define PRODUCT_VERSION "0.1.0-rc.5"
!define NPM_REGISTRY "https://registry.npmjs.org/"

!ifndef SourceDir
  !define SourceDir "dist\windows-amd64"
!endif

!ifndef Version
  !define Version "${PRODUCT_VERSION}"
!endif

Name "${PRODUCT_NAME} ${Version}"
OutFile "dist\DeepSeek-Harness-Full-Setup-${Version}.exe"
InstallDir "$PROGRAMFILES64\${PRODUCT_NAME}"
InstallDirRegKey HKCU "Software\${PRODUCT_NAME}" ""
RequestExecutionLevel admin

!include "MUI2.nsh"
!include "FileFunc.nsh"

; Pages
!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES

!insertmacro MUI_LANGUAGE "English"
!insertmacro MUI_LANGUAGE "SimpChinese"

; ── Helper: create dsh launcher batch ──
Function CreateDshLauncher
  FileOpen $0 "$INSTDIR\dsh.cmd" w
  FileWrite $0 '@echo off$\r$\n'
  FileWrite $0 'set "NODE_PATH=%~dp0node-v24.15.0-win-x64"$\r$\n'
  FileWrite $0 'set "PATH=%NODE_PATH%;%PATH%"$\r$\n'
  FileWrite $0 '"%NODE_PATH%\node.exe" "%~dp0node_modules\@deepseek-ai\dsh\lib\bin.js" %*$\r$\n'
  FileClose $0
FunctionEnd

; ── Helper: create bridge launcher ──
Function CreateBridgeLauncher
  FileOpen $0 "$INSTDIR\start-bridge.cmd" w
  FileWrite $0 '@echo off$\r$\n'
  FileWrite $0 'set "CHROMIUM_PATH=%~dp0chrome-win64\chrome.exe"$\r$\n'
  FileWrite $0 'set "BROWSER_MCP_CHROME_PATH=%CHROMIUM_PATH%"$\r$\n'
  FileWrite $0 '"%~dp0bridge.exe" --boot$\r$\n'
  FileClose $0
FunctionEnd

; ── Helper: create PowerShell env setup ──
Function CreateEnvSetup
  FileOpen $0 "$INSTDIR\env.ps1" w
  FileWrite $0 '$$nodePath = Join-Path $$PSScriptRoot "node-v24.15.0-win-x64"$\r$\n'
  FileWrite $0 '$$chromiumPath = Join-Path $$PSScriptRoot "chrome-win64\chrome.exe"$\r$\n'
  FileWrite $0 '$$env:Path = "$$nodePath;$$env:Path"$\r$\n'
  FileWrite $0 '$$env:BROWSER_MCP_CHROME_PATH = $$chromiumPath$\r$\n'
  FileWrite $0 'Write-Host "DeepSeek Harness environment ready"$\r$\n'
  FileClose $0
FunctionEnd

Section "Install"
  SetOutPath "$INSTDIR"

  ; ── Bundled Node.js (portable) ──
  File /r "${SourceDir}\node-v24.15.0-win-x64"

  ; ── Bundled Chromium (Playwright) ──
  File /r "${SourceDir}\chrome-win64"

  ; ── Desktop app ──
  File "${SourceDir}\DeepSeek Harness.exe"

  ; ── easybrowser native binaries ──
  File "${SourceDir}\bridge.exe"
  File "${SourceDir}\nm-host.exe"

  ; ── Chrome extension ──
  File /r "${SourceDir}\extension"

  ; ── Launcher scripts ──
  Call CreateDshLauncher
  Call CreateBridgeLauncher
  Call CreateEnvSetup

  ; ── Register bridge auto-start ──
  WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Run" \
    "${PRODUCT_NAME}Bridge" '"$INSTDIR\bridge.exe" --boot'

  ; ── Uninstaller ──
  WriteUninstaller "$INSTDIR\Uninstall.exe"

  ; ── Shortcuts ──
  CreateDirectory "$SMPROGRAMS\${PRODUCT_NAME}"
  CreateShortCut "$SMPROGRAMS\${PRODUCT_NAME}\${PRODUCT_NAME}.lnk" \
    "$INSTDIR\DeepSeek Harness.exe"
  CreateShortCut "$SMPROGRAMS\${PRODUCT_NAME}\dsh CLI.lnk" \
    "$INSTDIR\node-v24.15.0-win-x64\node.exe" \
    '"$INSTDIR\node_modules\@deepseek-ai\dsh\lib\bin.js" --profile web'
  CreateShortCut "$SMPROGRAMS\${PRODUCT_NAME}\Uninstall.lnk" \
    "$INSTDIR\Uninstall.exe"
  CreateShortCut "$DESKTOP\${PRODUCT_NAME}.lnk" \
    "$INSTDIR\DeepSeek Harness.exe"

  ; ── Registry ──
  WriteRegStr HKCU "Software\${PRODUCT_NAME}" "" "$INSTDIR"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" \
    "DisplayName" "${PRODUCT_NAME}"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" \
    "UninstallString" "$INSTDIR\Uninstall.exe"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" \
    "DisplayVersion" "${Version}"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" \
    "Publisher" "${PRODUCT_PUBLISHER}"

  ; ── Start bridge ──
  Exec "$\"$INSTDIR\bridge.exe$\" --boot"

  ; ── Install dsh CLI from npm ──
  DetailPrint "Installing dsh CLI from npm..."
  nsExec::ExecToStack '"$INSTDIR\node-v24.15.0-win-x64\node.exe" "$INSTDIR\node-v24.15.0-win-x64\node_modules\npm\bin\npm-cli.js" install @deepseek-ai/dsh --prefix "$INSTDIR" --registry "${NPM_REGISTRY}"'
  Pop $0
  DetailPrint "npm install completed (exit code: $0)"
SectionEnd

Section "Uninstall"
  ExecWait '"$INSTDIR\bridge.exe" --shutdown'
  DeleteRegValue HKCU "Software\Microsoft\Windows\CurrentVersion\Run" \
    "${PRODUCT_NAME}Bridge"
  RMDir /r "$INSTDIR"
  RMDir /r "$SMPROGRAMS\${PRODUCT_NAME}"
  Delete "$DESKTOP\${PRODUCT_NAME}.lnk"
  DeleteRegKey HKCU "Software\${PRODUCT_NAME}"
  DeleteRegKey HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}"
SectionEnd