; Jasqet installer (per-user: no administrator rights needed)
; 64-bit by default; the NSIS download for Windows only has 32-bit stubs, so
; build there with -DTARGET=x86-unicode (it installs the same 64-bit Jasqet).
;
; Jasqet was called TurboGet up to 1.6.0. Installed over TurboGet, it goes into
; TurboGet's folder, so the browser extension loaded from there keeps its ID
; (Jasqet still knows it); TurboGet.exe, its shortcuts, uninstall entry and
; registrations make way for Jasqet's. Settings and the download list move to
; Jasqet's folder when it first starts (platform.go).
!ifndef TARGET
  !define TARGET "amd64-unicode"
!endif
Target ${TARGET}
!include "MUI2.nsh"
!include "FileFunc.nsh"

!ifndef VERSION
  !define VERSION "1.6.2"
!endif
!ifndef SRCDIR
  !define SRCDIR "payload"
!endif
!ifndef OUTFILE
  !define OUTFILE "Jasqet-Setup.exe"
!endif
; Fill these once the extension is published, so the browsers offer it automatically.
!ifndef CHROME_STORE_ID
  !define CHROME_STORE_ID ""
!endif
!ifndef EDGE_STORE_ID
  !define EDGE_STORE_ID ""
!endif

Name "Jasqet"
OutFile "${OUTFILE}"
InstallDir "$LOCALAPPDATA\Programs\Jasqet"
InstallDirRegKey HKCU "Software\Jasqet" "InstallDir"
RequestExecutionLevel user
SetCompressor /SOLID lzma
BrandingText "Jasqet ${VERSION}"
VIProductVersion "${VERSION}.0"
VIAddVersionKey "ProductName" "Jasqet"
VIAddVersionKey "FileDescription" "Jasqet download manager installer"
VIAddVersionKey "FileVersion" "${VERSION}"
VIAddVersionKey "ProductVersion" "${VERSION}"
VIAddVersionKey "LegalCopyright" "Free and open source"

!define MUI_ICON "${SRCDIR}\icon.ico"
!define MUI_UNICON "${SRCDIR}\icon.ico"
!define MUI_ABORTWARNING
!define MUI_WELCOMEPAGE_TITLE "Install Jasqet ${VERSION}"
!define MUI_WELCOMEPAGE_TEXT "Jasqet is a free download manager: many connections per file, pause and resume, YouTube and video saving, and a scheduler for cheap night data.$\r$\n$\r$\nUsed TurboGet before? Jasqet is its new name: it installs over it and keeps your downloads and settings.$\r$\n$\r$\nNo administrator rights are needed, because it installs just for you.$\r$\n$\r$\nClick Next to continue."
!define MUI_FINISHPAGE_RUN "$INSTDIR\Jasqet.exe"
!define MUI_FINISHPAGE_RUN_TEXT "Open Jasqet"
!define MUI_FINISHPAGE_SHOWREADME ""
!define MUI_FINISHPAGE_SHOWREADME_TEXT "Connect my browser (2-minute guide)"
!define MUI_FINISHPAGE_SHOWREADME_FUNCTION ConnectBrowser

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_COMPONENTS
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

; Installing over TurboGet (Jasqet's old name): use its folder, so the
; extension loaded unpacked from it keeps working.
Function .onInit
  ReadRegStr $0 HKCU "Software\Jasqet" "InstallDir"
  StrCmp $0 "" 0 init_done
  ReadRegStr $0 HKCU "Software\TurboGet" "InstallDir"
  StrCmp $0 "" 0 +2
    StrCpy $0 "$LOCALAPPDATA\Programs\TurboGet"
  IfFileExists "$0\TurboGet.exe" 0 init_done
    StrCpy $INSTDIR $0
  init_done:
FunctionEnd

; Close a running copy so its files can be replaced (downloads resume
; later). Windows won't let a running program's file be overwritten, but it
; can be renamed, so the old file is moved aside and the new one written in
; its place, even if the old copy is slow to close. EXE is Jasqet, or TurboGet
; when installing over Jasqet's old name.
!macro CloseApp UN EXE
Function ${UN}Close${EXE}
  IfFileExists "$INSTDIR\${EXE}.exe" 0 tg_done
  ExecWait '"$INSTDIR\${EXE}.exe" -quit'
  ; give it up to 10 s to save its list and close by itself before forcing it
  StrCpy $R0 0
  tg_wait:
    nsExec::Exec 'cmd /c tasklist /FI "IMAGENAME eq ${EXE}.exe" | find /I "${EXE}.exe"'
    Pop $R1
    StrCmp $R1 "0" 0 tg_gone
    Sleep 500
    IntOp $R0 $R0 + 1
    IntCmp $R0 20 tg_gone tg_wait tg_gone
  tg_gone:
  StrCpy $R0 0
  tg_loop:
    ClearErrors
    Delete "$INSTDIR\${EXE}.exe.old"
    Rename "$INSTDIR\${EXE}.exe" "$INSTDIR\${EXE}.exe.old"
    IfErrors 0 tg_moved
    nsExec::Exec 'taskkill /F /T /IM ${EXE}.exe'
    Pop $R1
    Sleep 700
    IntOp $R0 $R0 + 1
    IntCmp $R0 20 tg_moved tg_loop tg_moved
  tg_moved:
  ; make sure no old copy keeps running (it would hold the app's port)
  nsExec::Exec 'taskkill /F /T /IM ${EXE}.exe'
  Pop $R1
  Sleep 800
  ClearErrors
  Delete "$INSTDIR\${EXE}.exe.old"
  IfErrors 0 tg_done
    Delete /REBOOTOK "$INSTDIR\${EXE}.exe.old"
  tg_done:
FunctionEnd
!macroend
!insertmacro CloseApp "" Jasqet
!insertmacro CloseApp "" TurboGet
!insertmacro CloseApp "un." Jasqet

Function ConnectBrowser
  Exec '"$INSTDIR\Jasqet.exe" -connect-browser'
FunctionEnd

Section "Jasqet (required)" SecMain
  SectionIn RO
  SetOutPath "$INSTDIR"
  ; TurboGet (Jasqet's old name) installed here?
  StrCpy $R9 ""
  IfFileExists "$INSTDIR\TurboGet.exe" 0 +2
    StrCpy $R9 "TurboGet"
  Call CloseJasqet
  Call CloseTurboGet
  File "${SRCDIR}\Jasqet.exe"
  ; a copy of Jasqet (or TurboGet) running from another folder: ask it to close too
  ExecWait '"$INSTDIR\Jasqet.exe" -quit'
  File "${SRCDIR}\icon.ico"
  File "${SRCDIR}\README.md"
  File "${SRCDIR}\LICENSE.txt"
  SetOutPath "$INSTDIR\extension"
  File /r "${SRCDIR}\extension\*.*"
  SetOutPath "$INSTDIR\extension-firefox"
  File /r "${SRCDIR}\extension-firefox\*.*"
  SetOutPath "$INSTDIR"

  ; TurboGet's shortcuts, uninstall entry and registrations make way for
  ; Jasqet's (your settings move when Jasqet first starts)
  StrCmp $R9 "TurboGet" 0 no_turboget
    Delete "$SMPROGRAMS\TurboGet\TurboGet.lnk"
    Delete "$SMPROGRAMS\TurboGet\Uninstall TurboGet.lnk"
    RMDir "$SMPROGRAMS\TurboGet"
    Delete "$DESKTOP\TurboGet.lnk"
    DeleteRegValue HKCU "Software\Microsoft\Windows\CurrentVersion\Run" "TurboGet"
    DeleteRegKey HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\TurboGet"
    DeleteRegKey HKCU "Software\TurboGet"
    DeleteRegKey HKCU "Software\Classes\turboget"
    DeleteRegKey HKCU "Software\Classes\AppUserModelId\TurboGet"
    RMDir /r "$TEMP\TurboGet-viewer"
  no_turboget:

  WriteUninstaller "$INSTDIR\Uninstall.exe"
  WriteRegStr HKCU "Software\Jasqet" "InstallDir" "$INSTDIR"
  !define UNINST "Software\Microsoft\Windows\CurrentVersion\Uninstall\Jasqet"
  WriteRegStr HKCU "${UNINST}" "DisplayName" "Jasqet"
  WriteRegStr HKCU "${UNINST}" "DisplayVersion" "${VERSION}"
  WriteRegStr HKCU "${UNINST}" "Publisher" "Jasqet"
  WriteRegStr HKCU "${UNINST}" "DisplayIcon" "$INSTDIR\Jasqet.exe"
  WriteRegStr HKCU "${UNINST}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKCU "${UNINST}" "UninstallString" '"$INSTDIR\Uninstall.exe"'
  WriteRegStr HKCU "${UNINST}" "QuietUninstallString" '"$INSTDIR\Uninstall.exe" /S'
  WriteRegDWORD HKCU "${UNINST}" "NoModify" 1
  WriteRegDWORD HKCU "${UNINST}" "NoRepair" 1
  ${GetSize} "$INSTDIR" "/S=0K" $0 $1 $2
  WriteRegDWORD HKCU "${UNINST}" "EstimatedSize" $0

  CreateDirectory "$SMPROGRAMS\Jasqet"
  CreateShortcut "$SMPROGRAMS\Jasqet\Jasqet.lnk" "$INSTDIR\Jasqet.exe" "" "$INSTDIR\Jasqet.exe" 0
  CreateShortcut "$SMPROGRAMS\Jasqet\Uninstall Jasqet.lnk" "$INSTDIR\Uninstall.exe"

  ; Browsers offer the extension automatically once it's in their stores.
  StrCmp "${CHROME_STORE_ID}" "" +2
    WriteRegStr HKCU "Software\Google\Chrome\Extensions\${CHROME_STORE_ID}" "update_url" "https://clients2.google.com/service/update2/crx"
  StrCmp "${EDGE_STORE_ID}" "" +2
    WriteRegStr HKCU "Software\Microsoft\Edge\Extensions\${EDGE_STORE_ID}" "update_url" "https://edge.microsoft.com/extensionwebstorebase/v1/crx"
SectionEnd

Section "Start with Windows (needed for the scheduler)" SecStartup
  WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Run" "Jasqet" '"$INSTDIR\Jasqet.exe" -no-browser'
SectionEnd

Section "Desktop shortcut" SecDesktop
  CreateShortcut "$DESKTOP\Jasqet.lnk" "$INSTDIR\Jasqet.exe" "" "$INSTDIR\Jasqet.exe" 0
SectionEnd

LangString DESC_Main ${LANG_ENGLISH} "The Jasqet app and its browser extension files."
LangString DESC_Startup ${LANG_ENGLISH} "Starts Jasqet quietly in the tray when you sign in, so scheduled downloads run at night."
LangString DESC_Desktop ${LANG_ENGLISH} "A Jasqet icon on your desktop."
!insertmacro MUI_FUNCTION_DESCRIPTION_BEGIN
  !insertmacro MUI_DESCRIPTION_TEXT ${SecMain} $(DESC_Main)
  !insertmacro MUI_DESCRIPTION_TEXT ${SecStartup} $(DESC_Startup)
  !insertmacro MUI_DESCRIPTION_TEXT ${SecDesktop} $(DESC_Desktop)
!insertmacro MUI_FUNCTION_DESCRIPTION_END

Section "Uninstall"
  Call un.CloseJasqet
  Delete "$INSTDIR\Jasqet.exe"
  Delete /REBOOTOK "$INSTDIR\Jasqet.exe.old"
  Delete "$INSTDIR\icon.ico"
  Delete "$INSTDIR\README.md"
  Delete "$INSTDIR\LICENSE.txt"
  Delete "$INSTDIR\Uninstall.exe"
  RMDir /r "$INSTDIR\extension"
  RMDir /r "$INSTDIR\extension-firefox"
  RMDir "$INSTDIR"
  Delete "$SMPROGRAMS\Jasqet\Jasqet.lnk"
  Delete "$SMPROGRAMS\Jasqet\Uninstall Jasqet.lnk"
  RMDir "$SMPROGRAMS\Jasqet"
  Delete "$DESKTOP\Jasqet.lnk"
  DeleteRegValue HKCU "Software\Microsoft\Windows\CurrentVersion\Run" "Jasqet"
  DeleteRegKey HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\Jasqet"
  DeleteRegKey HKCU "Software\Jasqet"
  ; the jasqet:// links behind the notification buttons, and the notifications' name and icon
  DeleteRegKey HKCU "Software\Classes\jasqet"
  DeleteRegKey HKCU "Software\Classes\AppUserModelId\Jasqet"
  StrCmp "${CHROME_STORE_ID}" "" +2
    DeleteRegKey HKCU "Software\Google\Chrome\Extensions\${CHROME_STORE_ID}"
  StrCmp "${EDGE_STORE_ID}" "" +2
    DeleteRegKey HKCU "Software\Microsoft\Edge\Extensions\${EDGE_STORE_ID}"
  ; the viewer's re-wrapped TS/FLV copies
  RMDir /r "$TEMP\Jasqet-viewer"
  ; Your downloads and settings (Downloads\Jasqet, %AppData%\Jasqet) are kept.
SectionEnd
