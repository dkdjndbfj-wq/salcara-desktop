; Wrap the existing complete Electron directory. The updater owns only app\;
; the Inno uninstall program and log stay outside the directory it swaps.
#ifndef AppVersion
  #error AppVersion must be supplied by scripts/installer.cjs
#endif
#ifndef PayloadDir
  #error PayloadDir must be supplied by scripts/installer.cjs
#endif
#ifndef ReleaseDir
  #error ReleaseDir must be supplied by scripts/installer.cjs
#endif

[Setup]
AppId=top.salcara.desktop.peruser
AppName=Salcara Desktop
AppVersion={#AppVersion}
AppPublisher=Salcara
AppPublisherURL=https://github.com/dkdjndbfj-wq/salcara-desktop
AppSupportURL=https://github.com/dkdjndbfj-wq/salcara-desktop/issues
AppUpdatesURL=https://github.com/dkdjndbfj-wq/salcara-desktop/releases
DefaultDirName={localappdata}\Programs\Salcara Desktop
DisableDirPage=yes
UsePreviousAppDir=no
DisableProgramGroupPage=yes
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0
UninstallFilesDir={app}
UninstallDisplayIcon={app}\app\Salcara Bridge.exe
UninstallDisplayName=Salcara Desktop
CloseApplications=no
RestartApplications=no
SetupIconFile=..\icons\app.ico
OutputDir={#ReleaseDir}
OutputBaseFilename=Salcara-Desktop-{#AppVersion}-win32-x64-setup
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
DisableWelcomePage=no
LicenseFile=..\..\..\..\LICENSE
SetupLogging=yes
SetupMutex=SalcaraDesktopPerUserInstaller

[Tasks]
Name: desktopicon; Description: "Create a desktop shortcut"; Flags: unchecked

[Files]
Source: "{#PayloadDir}\*"; DestDir: "{app}\app"; Flags: ignoreversion recursesubdirs createallsubdirs
Source: "root.marker"; DestDir: "{app}"; DestName: ".salcara-installer"; Flags: ignoreversion

[Icons]
Name: "{userprograms}\Salcara Desktop"; Filename: "{app}\app\Salcara Bridge.exe"; WorkingDir: "{app}\app"
Name: "{userdesktop}\Salcara Desktop"; Filename: "{app}\app\Salcara Bridge.exe"; WorkingDir: "{app}\app"; Tasks: desktopicon

[Run]
Filename: "{app}\app\Salcara Bridge.exe"; Description: "Launch Salcara Desktop"; Flags: nowait postinstall skipifsilent unchecked

[Code]
const
  RootIdentity = 'salcara-desktop-per-user-v1';
  MarkerPrefix = '{"product":"salcara-desktop","version":"';
  MarkerSuffix = '"}';
  FileAttributeDirectory = $10;
  FileAttributeReparsePoint = $400;
  InvalidAttributes = $FFFFFFFF;

var
  PayloadMoved: Boolean;
  InstallationCommitted: Boolean;

function FileAttributes(Name: String): Cardinal;
  external 'GetFileAttributesW@kernel32.dll stdcall';
function OpenFileForDelete(Name: String; Access, Sharing: Cardinal;
  Security: LongWord; Creation, Flags: Cardinal; Template: THandle): THandle;
  external 'CreateFileW@kernel32.dll stdcall';
function CloseFile(Handle: THandle): Boolean;
  external 'CloseHandle@kernel32.dll stdcall';

function FixedRoot(): String;
begin
  Result := ExpandConstant('{localappdata}\Programs\Salcara Desktop');
end;

function IsFixedRoot(Root: String): Boolean;
begin
  Result := CompareText(RemoveBackslashUnlessRoot(ExpandFileName(Root)),
    RemoveBackslashUnlessRoot(ExpandFileName(FixedRoot()))) = 0;
end;

function SafeParents(Name: String): Boolean;
var
  At, Parent: String;
  Attributes: Cardinal;
begin
  Result := False;
  At := ExpandFileName(Name);
  repeat
    Attributes := FileAttributes(At);
    if Attributes <> InvalidAttributes then
      if ((Attributes and FileAttributeReparsePoint) <> 0) or
        ((Attributes and FileAttributeDirectory) = 0) then Exit;
    Parent := ExtractFileDir(At);
    if (Parent = '') or (CompareText(Parent, At) = 0) then Break;
    At := Parent;
  until False;
  Result := True;
end;

function RootOwned(Root: String): Boolean;
var
  Text: AnsiString;
  Attributes: Cardinal;
begin
  Attributes := FileAttributes(Root + '\.salcara-installer');
  Result := False;
  if (Attributes = InvalidAttributes) or
    ((Attributes and (FileAttributeReparsePoint or FileAttributeDirectory)) <> 0) then Exit;
  Result := LoadStringFromFile(Root + '\.salcara-installer', Text) and
    (Trim(String(Text)) = RootIdentity);
end;

function IsEmptyDirectory(Name: String): Boolean;
var
  Entry: TFindRec;
begin
  Result := True;
  if FindFirst(Name + '\*', Entry) then
    try
      repeat
        if (Entry.Name <> '.') and (Entry.Name <> '..') then begin
          Result := False;
          Exit;
        end;
      until not FindNext(Entry);
    finally
      FindClose(Entry);
    end;
end;

function ParseVersion(Value: String; var Parts: TArrayOfString): Boolean;
var
  I, J: Integer;
begin
  Result := False;
  Parts := StringSplit(Value, ['.'], stAll);
  if GetArrayLength(Parts) <> 3 then Exit;
  for I := 0 to 2 do begin
    if (Length(Parts[I]) = 0) or (Length(Parts[I]) > 9) then Exit;
    for J := 1 to Length(Parts[I]) do
      if (Parts[I][J] < '0') or (Parts[I][J] > '9') then Exit;
    if StrToIntDef(Parts[I], -1) < 0 then Exit;
  end;
  Result := True;
end;

function PayloadVersion(Name: String; var Version: String): Boolean;
var
  Bytes: AnsiString;
  Text: String;
  Parts: TArrayOfString;
begin
  Result := False;
  if not LoadStringFromFile(Name + '\.salcara-install.json', Bytes) then Exit;
  // The packager emits exactly this two-field JSON document. Reject other
  // formats rather than guessing ownership of a directory we would delete.
  Text := Trim(String(Bytes));
  if (Copy(Text, 1, Length(MarkerPrefix)) <> MarkerPrefix) or
    (Copy(Text, Length(Text) - Length(MarkerSuffix) + 1,
      Length(MarkerSuffix)) <> MarkerSuffix) then Exit;
  Version := Copy(Text, Length(MarkerPrefix) + 1,
    Length(Text) - Length(MarkerPrefix) - Length(MarkerSuffix));
  Result := ParseVersion(Version, Parts);
end;

function NewerThanInstaller(Version: String): Boolean;
var
  Existing, Offered: TArrayOfString;
  I, A, B: Integer;
begin
  Result := True;
  if not ParseVersion(Version, Existing) or
    not ParseVersion('{#AppVersion}', Offered) then Exit;
  for I := 0 to 2 do begin
    A := StrToInt(Existing[I]); B := StrToInt(Offered[I]);
    if A > B then Exit;
    if A < B then begin Result := False; Exit; end;
  end;
  Result := False;
end;

function SafeTree(Name: String): Boolean;
var
  Entry: TFindRec;
  Child: String;
  Handle: THandle;
  Attributes: Cardinal;
begin
  Result := False;
  Attributes := FileAttributes(Name);
  if (Attributes = InvalidAttributes) or
    ((Attributes and FileAttributeReparsePoint) <> 0) or
    ((Attributes and FileAttributeDirectory) = 0) then Exit;
  if FindFirst(Name + '\*', Entry) then
    try
      repeat
        if (Entry.Name <> '.') and (Entry.Name <> '..') then begin
          Child := Name + '\' + Entry.Name;
          if (Entry.Attributes and FileAttributeReparsePoint) <> 0 then Exit;
          if (Entry.Attributes and FileAttributeDirectory) <> 0 then begin
            if not SafeTree(Child) then Exit;
          end else begin
            // DELETE access with no sharing detects in-use files, including
            // mapped executables, without sending any process a close request.
            Handle := OpenFileForDelete(Child, $10000, 0, 0, 3, $200000, 0);
            if Handle = THandle(-1) then Exit;
            CloseFile(Handle);
          end;
        end;
      until not FindNext(Entry);
    finally
      FindClose(Entry);
    end;
  Result := True;
end;

function CheckInstallation(Root: String; Uninstalling: Boolean): String;
var
  Payload, Existing: String;
begin
  Result := 'The installation path is not the fixed current-user location.';
  if not IsFixedRoot(Root) then Exit;
  Result := 'The installation path contains a link or a non-directory.';
  if not SafeParents(Root) then Exit;
  Result := '';
  if not DirExists(Root) then begin
    if Uninstalling then Result := 'The installation directory is missing.';
    Exit;
  end;
  if not RootOwned(Root) then begin
    if Uninstalling or not IsEmptyDirectory(Root) then
      Result := 'This directory is not owned by the Salcara installer.';
    Exit;
  end;
  if not Uninstalling then
    if FileExists(Root + '\.salcara-setup-recovery') or
      DirExists(Root + '\.salcara-setup-recovery') then begin
      Result := 'A previous installer recovery copy exists. Keep it for recovery before reinstalling.';
      Exit;
    end;
  Payload := Root + '\app';
  if FileExists(Payload) then begin
    Result := 'The application directory is not a directory.'; Exit;
  end;
  if DirExists(Payload) then begin
    if not SafeParents(Payload) or not SafeTree(Payload) then begin
      Result := 'Close Salcara Desktop yourself before continuing. Linked or locked application files cannot be modified.';
      Exit;
    end;
    if not PayloadVersion(Payload, Existing) then begin
      Result := 'The application ownership/version marker is invalid.'; Exit;
    end;
    if not Uninstalling and NewerThanInstaller(Existing) then
      Result := 'A newer Salcara Desktop is already installed. This installer cannot downgrade it.';
  end;
end;

function RemovePayload(Name: String): Boolean;
var
  Entry: TFindRec;
  Child: String;
  Attributes: Cardinal;
begin
  Result := False;
  // Check each directory again immediately before descent. Never use DelTree,
  // and never sweep app.old-* recovery copies or a user-specified parent.
  Attributes := FileAttributes(Name);
  if (Attributes = InvalidAttributes) or
    ((Attributes and FileAttributeReparsePoint) <> 0) or
    ((Attributes and FileAttributeDirectory) = 0) then Exit;
  if FindFirst(Name + '\*', Entry) then
    try
      repeat
        if (Entry.Name <> '.') and (Entry.Name <> '..') then begin
          Child := Name + '\' + Entry.Name;
          Attributes := FileAttributes(Child);
          if (Attributes = InvalidAttributes) or
            ((Attributes and FileAttributeReparsePoint) <> 0) then Exit;
          if (Attributes and FileAttributeDirectory) <> 0 then begin
            if not RemovePayload(Child) then Exit;
          end else if not DeleteFile(Child) then Exit;
        end;
      until not FindNext(Entry);
    finally
      FindClose(Entry);
    end;
  Result := RemoveDir(Name);
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  Result := CheckInstallation(WizardDirValue(), False);
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  Root, Error, Recovery, Version: String;
begin
  Root := WizardDirValue(); Recovery := Root + '\.salcara-setup-recovery';
  if CurStep = ssInstall then begin
    Error := CheckInstallation(Root, False);
    if Error <> '' then RaiseException(Error);
    if DirExists(Root + '\app') then begin
      if not RenameFile(Root + '\app', Recovery) then
        RaiseException('The previous application could not be saved for recovery. No process was stopped.');
      PayloadMoved := True;
    end;
  end;
  if CurStep = ssPostInstall then begin
    // Inno has finished copying and registering the new installation. A file
    // copy failure before this point leaves the previous app available below.
    InstallationCommitted := True;
    if PayloadMoved and SafeParents(Recovery) and
      PayloadVersion(Recovery, Version) and SafeTree(Recovery) then
      if not RemovePayload(Recovery) then
        Log('The previous application copy was retained for recovery.');
  end;
end;

procedure DeinitializeSetup();
var
  Root, Recovery, Version: String;
begin
  if not PayloadMoved or InstallationCommitted then Exit;
  Root := WizardDirValue(); Recovery := Root + '\.salcara-setup-recovery';
  if not IsFixedRoot(Root) or not SafeParents(Root) or
    not SafeParents(Recovery) or not PayloadVersion(Recovery, Version) or
    not SafeTree(Recovery) then begin
    Log('Installer recovery copy requires manual recovery.'); Exit;
  end;
  // Inno's own rollback normally removes the incomplete new directory. Only
  // remove a surviving new tree if its exact marker confirms our version.
  if DirExists(Root + '\app') then begin
    if not SafeParents(Root + '\app') or
      not PayloadVersion(Root + '\app', Version) or
      (Version <> '{#AppVersion}') or not SafeTree(Root + '\app') then begin
      Log('Incomplete installation retained; previous copy remains in recovery.'); Exit;
    end;
    if not RemovePayload(Root + '\app') then begin
      Log('Incomplete installation could not be removed; previous copy retained.'); Exit;
    end;
  end;
  if RenameFile(Recovery, Root + '\app') then
    Log('The previous application was restored after an incomplete installation.')
  else Log('The previous application copy requires manual recovery.');
end;

function InitializeUninstall(): Boolean;
var
  Error: String;
begin
  Error := CheckInstallation(ExpandConstant('{app}'), True);
  Result := Error = '';
  if not Result then SuppressibleMsgBox(Error, mbError, MB_OK, IDOK);
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  Root, Error, Value, Expected: String;
begin
  if CurUninstallStep = usUninstall then begin
    Root := ExpandConstant('{app}'); Error := CheckInstallation(Root, True);
    if Error <> '' then RaiseException(Error);
    if DirExists(Root + '\app') then
      if not RemovePayload(Root + '\app') then
        RaiseException('The application files could not be removed. No process was stopped.');
    Expected := '"' + Root + '\app\Salcara Bridge.exe" --background';
    if RegQueryStringValue(HKCU, 'Software\Microsoft\Windows\CurrentVersion\Run',
      'SalcaraBridge', Value) then
      if CompareText(Value, Expected) = 0 then
        RegDeleteValue(HKCU, 'Software\Microsoft\Windows\CurrentVersion\Run', 'SalcaraBridge');
    // Do not remove AppData, configuration overrides, Agent installations,
    // user-added files in the parent, update staging or recovery backups.
  end;
end;
