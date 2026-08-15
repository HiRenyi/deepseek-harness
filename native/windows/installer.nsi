; DeepSeek Harness Windows Installer (NSIS)
; Requires: NSIS 3.x

Unicode True

!define PRODUCT_NAME "DeepSeek Harness"
!define PRODUCT_PUBLISHER "HiRenyi"
!define PRODUCT_VERSION "0.1.0-rc.5"
!define NODEJS_URL "https://nodejs.org/dist/v24.15.0/node-v24.15.0-x64.msi"
!define CHROME_URL "https://dl.google.com/chrome/install/latest/chrome_installer.exe"

!ifndef SourceDir
  !define SourceDir "dist\windows-amd64"
!endif

!ifndef Version
  !define Version "${PRODUCT_VERSION}"
!endif

Name "${PRODUCT_NAME} ${Version}"
OutFile "dist\DeepSeek-Harness-Setup-${Version}.exe"
InstallDir "$PROGRAMFILES64\${PRODUCT_NAME}"
InstallDirRegKey HKCU "Software\${PRODUCT_NAME}" ""
RequestExecutionLevel admin

; Modern UI
!include "MUI2.nsh"
!include "FileFunc.nsh"
!include "nsDialogs.nsh"
!include "WinVer.nsh"

; Custom page for dependency check
Var NodeJsFound
Var ChromeFound
Var Dialog
Var NodeJsLabel
Var ChromeLabel
Var NodeJsInstallBtn
Var ChromeInstallBtn
Var SkipNodeJsBtn
Var SkipChromeBtn

; Pages
!insertmacro MUI_PAGE_WELCOME
Page custom DependencyCheck DependencyCheckLeave
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES

; Languages
!insertmacro MUI_LANGUAGE "English"
!insertmacro MUI_LANGUAGE "SimpChinese"

; Language strings
LangString NodeJsNotFound ${LANG_ENGLISH} "Node.js is not installed. DeepSeek Harness requires Node.js 22+ to run."
LangString NodeJsNotFound ${LANG_SIMPCHINESE} "未检测到 Node.js。DeepSeek Harness 需要 Node.js 22+ 才能运行。"
LangString ChromeNotFound ${LANG_ENGLISH} "Chrome is not installed. Browser automation requires Chrome."
LangString ChromeNotFound ${LANG_SIMPCHINESE} "未检测到 Chrome。浏览器自动化功能需要 Chrome。"
LangString NodeJsFound_ ${LANG_ENGLISH} "Node.js detected ✓"
LangString NodeJsFound_ ${LANG_SIMPCHINESE} "Node.js 已安装 ✓"
LangString ChromeFound_ ${LANG_ENGLISH} "Chrome detected ✓"
LangString ChromeFound_ ${LANG_SIMPCHINESE} "Chrome 已安装 ✓"

; Check for Node.js
Function CheckNodeJs
  nsExec::ExecToStack '"node" --version'
  Pop $0
  StrCmp $0 "0" NodeJsFound_Label
  StrCpy $NodeJsFound "no"
  Goto NodeJsDone
NodeJsFound_Label:
  StrCpy $NodeJsFound "yes"
NodeJsDone:
FunctionEnd

; Check for Chrome
Function CheckChrome
  ; Check common install locations
  IfFileExists "$PROGRAMFILES64\Google\Chrome\Application\chrome.exe" ChromeFound_Label
  IfFileExists "$PROGRAMFILES32\Google\Chrome\Application\chrome.exe" ChromeFound_Label
  IfFileExists "$LOCALAPPDATA\Google\Chrome\Application\chrome.exe" ChromeFound_Label
  StrCpy $ChromeFound "no"
  Goto ChromeDone
ChromeFound_Label:
  StrCpy $ChromeFound "yes"
ChromeDone:
FunctionEnd

; Download and install Node.js
Function DownloadNodeJs
  DetailPrint "Downloading Node.js..."
  NSISdl::download "${NODEJS_URL}" "$TEMP\node-installer.msi"
  Pop $0
  StrCmp $0 "success" NodeDownloadOk
  DetailPrint "Node.js download failed: $0"
  MessageBox MB_ICONSTOP "Node.js download failed. Please install manually from https://nodejs.org"
  Goto NodeDownloadEnd
NodeDownloadOk:
  DetailPrint "Installing Node.js..."
  ExecWait 'msiexec /i "$TEMP\node-installer.msi" /qn ADDLOCAL=ALL'
  DetailPrint "Node.js installation complete"
  Delete "$TEMP\node-installer.msi"
NodeDownloadEnd:
FunctionEnd

; Download and install Chrome
Function DownloadChrome
  DetailPrint "Downloading Chrome..."
  NSISdl::download "${CHROME_URL}" "$TEMP\chrome_installer.exe"
  Pop $0
  StrCmp $0 "success" ChromeDownloadOk
  DetailPrint "Chrome download failed: $0"
  MessageBox MB_ICONSTOP "Chrome download failed. Please install manually from https://google.com/chrome"
  Goto ChromeDownloadEnd
ChromeDownloadOk:
  DetailPrint "Installing Chrome..."
  ExecWait '"$TEMP\chrome_installer.exe" /silent /install'
  DetailPrint "Chrome installation complete"
  Delete "$TEMP\chrome_installer.exe"
ChromeDownloadEnd:
FunctionEnd

; Custom dependency check page
Function DependencyCheck
  Call CheckNodeJs
  Call CheckChrome

  !insertmacro MUI_HEADER_TEXT "Dependency Check" "Checking required software..."

  nsDialogs::Create 1018
  Pop $Dialog

  ${If} $Dialog == error
    Abort
  ${EndIf}

  ; Node.js status
  ${NSD_CreateLabel} 0 0 100% 20 ""
  Pop $NodeJsLabel
  ${If} $NodeJsFound == "yes"
    ${NSD_SetText} $NodeJsLabel "$(NodeJsFound_)"
  ${Else}
    ${NSD_SetText} $NodeJsLabel "$(NodeJsNotFound)"
  ${EndIf}

  ; Chrome status
  ${NSD_CreateLabel} 0 30 100% 20 ""
  Pop $ChromeLabel
  ${If} $ChromeFound == "yes"
    ${NSD_SetText} $ChromeLabel "$(ChromeFound_)"
  ${Else}
    ${NSD_SetText} $ChromeLabel "$(ChromeNotFound)"
  ${EndIf}

  nsDialogs::Show
FunctionEnd

Function DependencyCheckLeave
  ${If} $NodeJsFound == "no"
    MessageBox MB_YESNO "Node.js is not installed. Install now?$\n$\n(Required for dsh CLI to run)" IDYES InstallNodeJs IDNO SkipNodeJs
InstallNodeJs:
    Call DownloadNodeJs
SkipNodeJs:
  ${EndIf}

  ${If} $ChromeFound == "no"
    MessageBox MB_YESNO "Chrome is not installed. Install now?$\n$\n(Required for browser automation)" IDYES InstallChrome IDNO SkipChrome
InstallChrome:
    Call DownloadChrome
SkipChrome:
  ${EndIf}
FunctionEnd

; Create bridge startup batch script
Function CreateBridgeScript
  FileOpen $0 "$INSTDIR\start-bridge.cmd" w
  FileWrite $0 '@echo off$\r$\n'
  FileWrite $0 'echo Starting easybrowser bridge...$\r$\n'
  FileWrite $0 '"%~dp0bridge.exe" --boot$\r$\n'
  FileClose $0
FunctionEnd

Section "Install"
  SetOutPath "$INSTDIR"

  ; Core dsh
  File /r "${SourceDir}\dsh\*"

  ; Desktop app
  File "${SourceDir}\DeepSeek Harness.exe"

  ; easybrowser bridge
  File /nonfatal "${SourceDir}\bridge.exe"
  File /nonfatal "${SourceDir}\nm-host.exe"

  ; Chrome extension
  File /nonfatal /r "${SourceDir}\extension"

  ; Create startup scripts
  Call CreateBridgeScript

  ; Register auto-start for bridge
  WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Run" "${PRODUCT_NAME}Bridge" "$INSTDIR\bridge.exe --boot"

  ; Write uninstaller
  WriteUninstaller "$INSTDIR\Uninstall.exe"

  ; Start menu shortcut
  CreateDirectory "$SMPROGRAMS\${PRODUCT_NAME}"
  CreateShortCut "$SMPROGRAMS\${PRODUCT_NAME}\${PRODUCT_NAME}.lnk" "$INSTDIR\DeepSeek Harness.exe"
  CreateShortCut "$SMPROGRAMS\${PRODUCT_NAME}\Uninstall.lnk" "$INSTDIR\Uninstall.exe"

  ; Desktop shortcut
  CreateShortCut "$DESKTOP\${PRODUCT_NAME}.lnk" "$INSTDIR\DeepSeek Harness.exe"

  ; Registry for uninstall
  WriteRegStr HKCU "Software\${PRODUCT_NAME}" "" "$INSTDIR"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" \
    "DisplayName" "${PRODUCT_NAME}"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" \
    "UninstallString" "$INSTDIR\Uninstall.exe"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" \
    "DisplayVersion" "${Version}"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" \
    "Publisher" "${PRODUCT_PUBLISHER}"

  ; Start bridge after install
  Exec "$\"$INSTDIR\bridge.exe$\" --boot"
SectionEnd

Section "Uninstall"
  ; Stop bridge if running
  ExecWait '"$INSTDIR\bridge.exe" --shutdown'

  ; Remove auto-start
  DeleteRegValue HKCU "Software\Microsoft\Windows\CurrentVersion\Run" "${PRODUCT_NAME}Bridge"

  ; Remove files
  RMDir /r "$INSTDIR"

  ; Remove shortcuts
  RMDir /r "$SMPROGRAMS\${PRODUCT_NAME}"
  Delete "$DESKTOP\${PRODUCT_NAME}.lnk"

  ; Remove registry
  DeleteRegKey HKCU "Software\${PRODUCT_NAME}"
  DeleteRegKey HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}"
SectionEnd