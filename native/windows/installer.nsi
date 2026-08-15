; DeepSeek Harness Windows Installer (NSIS)
; Requires: NSIS 3.x

Unicode True

!define PRODUCT_NAME "DeepSeek Harness"
!define PRODUCT_PUBLISHER "HiRenyi"
!define PRODUCT_VERSION "0.1.0-rc.5"

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

; Pages
!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_LICENSE "LICENSE"
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES

; Languages
!insertmacro MUI_LANGUAGE "English"
!insertmacro MUI_LANGUAGE "SimpChinese"

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

  ; Write uninstaller
  WriteUninstaller "$INSTDIR\Uninstall.exe"

  ; Start menu shortcut
  CreateDirectory "$SMPROGRAMS\${PRODUCT_NAME}"
  CreateShortCut "$SMPROGRAMS\${PRODUCT_NAME}\${PRODUCT_NAME}.lnk" "$INSTDIR\DeepSeek Harness.exe"
  CreateShortCut "$SMPROGRAMS\${PRODUCT_NAME}\Uninstall.lnk" "$INSTDIR\Uninstall.exe"

  ; Desktop shortcut
  CreateShortCut "$DESKTOP\${PRODUCT_NAME}.lnk" "$INSTDIR\DeepSeek Harness.exe"

  ; Registry for auto-start and uninstall
  WriteRegStr HKCU "Software\${PRODUCT_NAME}" "" "$INSTDIR"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" \
    "DisplayName" "${PRODUCT_NAME}"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" \
    "UninstallString" "$INSTDIR\Uninstall.exe"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" \
    "DisplayVersion" "${Version}"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}" \
    "Publisher" "${PRODUCT_PUBLISHER}"
SectionEnd

Section "Uninstall"
  ; Stop bridge if running
  ExecWait '"$INSTDIR\bridge.exe" --shutdown'

  ; Remove files
  RMDir /r "$INSTDIR"

  ; Remove shortcuts
  RMDir /r "$SMPROGRAMS\${PRODUCT_NAME}"
  Delete "$DESKTOP\${PRODUCT_NAME}.lnk"

  ; Remove registry
  DeleteRegKey HKCU "Software\${PRODUCT_NAME}"
  DeleteRegKey HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${PRODUCT_NAME}"
  DeleteRegValue HKCU "Software\Microsoft\Windows\CurrentVersion\Run" "${PRODUCT_NAME}Bridge"
SectionEnd