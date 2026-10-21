; Inno Setup 6.3+ script for Pintu. Built by build.ps1 -Installer:
;   ISCC /DAppVersion=v1.2.3 /DAppVersionNumeric=1.2.3.0 /DArch=amd64 /DBinDir=..\dist\pintu-windows-amd64 pintu.iss
;
; pintu.exe goes to Program Files; config, database and log live in
; %ProgramData%\Pintu, readable only by SYSTEM and Administrators.

#ifndef AppVersion
  #define AppVersion "dev"
#endif
#ifndef AppVersionNumeric
  #define AppVersionNumeric "0.0.0.0"
#endif
#ifndef Arch
  #define Arch "amd64"
#endif
#ifndef BinDir
  #define BinDir "..\dist\pintu-windows-" + Arch
#endif

[Setup]
AppId={{A0C1A2DA-165A-4EB8-AE10-9C8AA039EB6C}
AppName=Pintu
AppVersion={#AppVersion}
AppVerName=Pintu {#AppVersion}
AppPublisher=Pintu
AppPublisherURL=https://github.com/Ryandinulfatah12/pintu
AppSupportURL=https://github.com/Ryandinulfatah12/pintu/issues
VersionInfoVersion={#AppVersionNumeric}
DefaultDirName={autopf}\Pintu
DisableProgramGroupPage=yes
PrivilegesRequired=admin
LicenseFile=..\LICENSE
SetupIconFile=..\winres\icon.ico
UninstallDisplayIcon={app}\pintu.exe
OutputBaseFilename=pintu-{#AppVersion}-windows-{#Arch}-setup
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
MinVersion=10.0
#if Arch == "arm64"
ArchitecturesAllowed=arm64
ArchitecturesInstallIn64BitMode=arm64
#else
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
#endif

[Tasks]
Name: firewall; Description: "Izinkan akses dari perangkat lain di jaringan (aturan Windows Firewall)"; Flags: unchecked

[Files]
Source: "{#BinDir}\pintu.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#BinDir}\.env.example"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#BinDir}\LICENSE"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#BinDir}\THIRD_PARTY_NOTICES.md"; DestDir: "{app}"; Flags: ignoreversion

[Dirs]
Name: "{commonappdata}\Pintu\data"

[Run]
Filename: "{app}\pintu.exe"; Parameters: "config ensure-secret ""{code:ConfigFile}"""; Flags: runhidden; StatusMsg: "Membuat SECRET_KEY..."
Filename: "{sys}\icacls.exe"; Parameters: """{commonappdata}\Pintu"" /inheritance:r /grant:r *S-1-5-18:(OI)(CI)F *S-1-5-32-544:(OI)(CI)F /T /C /Q"; Flags: runhidden; StatusMsg: "Mengamankan folder config..."
Filename: "{app}\pintu.exe"; Parameters: "service install --config ""{code:ConfigFile}"""; Flags: runhidden; StatusMsg: "Memasang service Pintu..."
Filename: "{sys}\netsh.exe"; Parameters: "advfirewall firewall delete rule name=""Pintu"""; Flags: runhidden
Filename: "{sys}\netsh.exe"; Parameters: "advfirewall firewall add rule name=""Pintu"" dir=in action=allow program=""{app}\pintu.exe"" enable=yes"; Flags: runhidden; Tasks: firewall
Filename: "{app}\pintu.exe"; Parameters: "service start"; Flags: runhidden; StatusMsg: "Menjalankan service Pintu..."
Filename: "http://localhost:{code:PortValue}/"; Description: "Buka Pintu di browser"; Flags: shellexec postinstall nowait skipifsilent

[UninstallRun]
Filename: "{app}\pintu.exe"; Parameters: "service uninstall"; Flags: runhidden; RunOnceId: "RemoveService"
Filename: "{sys}\netsh.exe"; Parameters: "advfirewall firewall delete rule name=""Pintu"""; Flags: runhidden; RunOnceId: "RemoveFirewall"

[Code]
var
  AdminPage: TInputQueryWizardPage;
  SecureBox: TNewCheckBox;

function ConfigFile(Param: String): String;
begin
  Result := ExpandConstant('{commonappdata}\Pintu\pintu.env');
end;

// Port from the wizard on a fresh install, otherwise from the existing pintu.env.
function PortValue(Param: String): String;
var
  Lines: TArrayOfString;
  I: Integer;
  Line: String;
begin
  Result := '8080';
  if (AdminPage <> nil) and not FileExists(ConfigFile('')) then
  begin
    Result := Trim(AdminPage.Values[3]);
    Exit;
  end;
  if LoadStringsFromFile(ConfigFile(''), Lines) then
    for I := 0 to GetArrayLength(Lines) - 1 do
    begin
      Line := Trim(Lines[I]);
      if Pos('PORT=', Line) = 1 then
      begin
        Line := Trim(Copy(Line, 6, Length(Line)));
        StringChangeEx(Line, '"', '', True);
        if StrToIntDef(Line, 0) > 0 then
          Result := Line;
      end;
    end;
end;

procedure InitializeWizard;
begin
  AdminPage := CreateInputQueryPage(wpSelectDir, 'Akun admin Pintu',
    'Akun ini dipakai untuk login ke Pintu dari browser.',
    'Config disimpan di ' + ExpandConstant('{commonappdata}\Pintu\pintu.env') + '.');
  AdminPage.Add('Username:', False);
  AdminPage.Add('Password (minimal 8 karakter):', True);
  AdminPage.Add('Ulangi password:', True);
  AdminPage.Add('Port HTTP:', False);
  AdminPage.Values[0] := 'admin';
  AdminPage.Values[3] := '8080';

  SecureBox := TNewCheckBox.Create(AdminPage);
  SecureBox.Parent := AdminPage.Surface;
  SecureBox.Top := AdminPage.Edits[3].Top + AdminPage.Edits[3].Height + ScaleY(16);
  SecureBox.Width := AdminPage.SurfaceWidth;
  SecureBox.Height := ScaleY(17);
  SecureBox.Caption := 'Diakses lewat HTTPS (reverse proxy / Cloudflare Tunnel), set COOKIE_SECURE=true';
end;

// pintu.env is only written on a fresh install; upgrades keep the existing one.
function ShouldSkipPage(PageID: Integer): Boolean;
begin
  Result := (PageID = AdminPage.ID) and FileExists(ConfigFile(''));
end;

function NextButtonClick(CurPageID: Integer): Boolean;
var
  Port: Integer;
begin
  Result := True;
  if CurPageID <> AdminPage.ID then
    Exit;
  Port := StrToIntDef(Trim(AdminPage.Values[3]), 0);
  if Trim(AdminPage.Values[0]) = '' then
  begin
    MsgBox('Username wajib diisi.', mbError, MB_OK);
    Result := False;
  end
  else if Length(AdminPage.Values[1]) < 8 then
  begin
    MsgBox('Password minimal 8 karakter.', mbError, MB_OK);
    Result := False;
  end
  else if AdminPage.Values[1] <> AdminPage.Values[2] then
  begin
    MsgBox('Password dan ulangan password tidak sama.', mbError, MB_OK);
    Result := False;
  end
  else if (Port < 1) or (Port > 65535) then
  begin
    MsgBox('Port harus 1-65535.', mbError, MB_OK);
    Result := False;
  end;
end;

// Values are always quoted; Pintu's env parser strips one pair of outer quotes.
function EnvLine(Key, Value: String): String;
begin
  Result := Key + '="' + Value + '"';
end;

procedure WriteConfig;
var
  Dir, Secure: String;
  Lines: TArrayOfString;
begin
  Dir := ExpandConstant('{commonappdata}\Pintu');
  ForceDirectories(Dir + '\data');
  if SecureBox.Checked then Secure := 'true' else Secure := 'false';

  SetArrayLength(Lines, 13);
  Lines[0] := '# Config Pintu. Ubah lalu restart service: pintu.exe service stop, pintu.exe service start';
  Lines[1] := '# JANGAN ganti SECRET_KEY setelah ada data: password koneksi yang tersimpan jadi tidak bisa dibuka.';
  Lines[2] := 'SECRET_KEY=';
  Lines[3] := EnvLine('ADMIN_USER', Trim(AdminPage.Values[0]));
  Lines[4] := EnvLine('ADMIN_PASSWORD', AdminPage.Values[1]);
  Lines[5] := EnvLine('PORT', Trim(AdminPage.Values[3]));
  Lines[6] := EnvLine('COOKIE_SECURE', Secure);
  Lines[7] := EnvLine('SESSION_HOURS', '12');
  Lines[8] := EnvLine('LOG_LEVEL', 'ERRORS');
  Lines[9] := EnvLine('DATA_DIR', Dir + '\data');
  Lines[10] := EnvLine('LOG_FILE', Dir + '\pintu.log');
  Lines[11] := '# guacd (untuk RDP) jalan di Docker Desktop / WSL';
  Lines[12] := EnvLine('GUACD_HOST', '127.0.0.1');
  SaveStringsToUTF8FileWithoutBOM(ConfigFile(''), Lines, False);
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  if (CurStep = ssInstall) and not FileExists(ConfigFile('')) then
    WriteConfig;
end;

// Stop a running service so pintu.exe is not locked during an upgrade.
function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  ResultCode: Integer;
begin
  Result := '';
  if FileExists(ExpandConstant('{app}\pintu.exe')) then
    Exec(ExpandConstant('{app}\pintu.exe'), 'service stop', '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  Dir: String;
begin
  Dir := ExpandConstant('{commonappdata}\Pintu');
  if (CurUninstallStep = usPostUninstall) and DirExists(Dir) and not UninstallSilent then
    if MsgBox('Hapus juga config dan database Pintu di ' + Dir + '?' + #13#10#13#10 +
        'Pilih No untuk menyimpannya (misalnya untuk dipasang ulang nanti).',
        mbConfirmation, MB_YESNO or MB_DEFBUTTON2) = IDYES then
      DelTree(Dir, True, True, True);
end;
