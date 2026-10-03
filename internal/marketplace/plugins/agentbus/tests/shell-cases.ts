// Shell command definitions the proxy (at registry load) and the mod (at run time) must judge the
// same way. internal/slackbridge/commands_test.go reads the JSON array between the markers below,
// so keep it strict JSON. Each argv is the windows one; ok says whether the definition may run.
export type ShellCase = {
  name: string
  ok: boolean
  argv: string[]
  env?: Record<string, string>
  args_pattern?: string
  args_enum?: string[]
  output?: string
}

export const SHELL_CASES: ShellCase[] = /* BEGIN SHELL CASES */ [
  { "name": "powershell with a trailing dot", "ok": false, "argv": ["powershell.", "-NoProfile", "{args}"], "args_enum": ["x"] },
  { "name": "powershell.exe with a trailing space", "ok": false, "argv": ["powershell.exe ", "{args}"], "args_enum": ["x"] },
  { "name": "full path, upper case, trailing dots", "ok": false, "argv": ["C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\POWERSHELL.EXE..", "{out}"] },
  { "name": "cmd.exe. /c with {out}", "ok": false, "argv": ["cmd.exe.", "/c", "x", "{out}"] },
  { "name": "run.bat. as the program", "ok": false, "argv": ["run.bat."] },
  { "name": "shot.ps1 . as the program", "ok": false, "argv": ["C:\\tools\\shot.ps1 ."] },
  { "name": "a .cmd program with a trailing space", "ok": false, "argv": ["run.CMD "] },
  { "name": "nodejs -e", "ok": false, "argv": ["nodejs", "-e", "{args}"], "args_enum": ["x"] },
  { "name": "sudo -s", "ok": false, "argv": ["sudo", "-s", "{args}"], "args_enum": ["x"] },
  { "name": "su -c", "ok": false, "argv": ["su", "-c", "{args}"], "args_enum": ["x"] },
  { "name": "tclsh8.6", "ok": false, "argv": ["tclsh8.6", "{args}"], "args_enum": ["x"] },
  { "name": "lua5.4", "ok": false, "argv": ["lua5.4", "x.lua", "{out}"] },
  { "name": "perl5.36", "ok": false, "argv": ["perl5.36", "{out}"] },
  { "name": "python3.12.exe", "ok": false, "argv": ["python3.12.exe", "x.py", "{out}"] },
  { "name": "python.com.exe", "ok": false, "argv": ["python.com.exe", "{args}"], "args_enum": ["x"] },
  { "name": "pwsh-preview", "ok": false, "argv": ["pwsh-preview", "{args}"], "args_enum": ["x"] },
  { "name": "powershell_ise", "ok": false, "argv": ["powershell_ise.exe", "{out}"] },
  { "name": "watch", "ok": false, "argv": ["watch", "{args}"], "args_enum": ["x"] },
  { "name": "script -qc", "ok": false, "argv": ["script", "-qc", "x", "{out}"] },
  { "name": "flock -c", "ok": false, "argv": ["flock", "/tmp/l", "-c", "{args}"], "args_enum": ["x"] },
  { "name": "forfiles /c", "ok": false, "argv": ["forfiles.exe", "/c", "{args}"], "args_enum": ["x"] },
  { "name": "rundll32", "ok": false, "argv": ["rundll32", "{args}"], "args_enum": ["x"] },
  { "name": "regsvr32", "ok": false, "argv": ["C:\\Windows\\System32\\regsvr32.exe", "{args}"], "args_enum": ["x"] },
  { "name": "conhost", "ok": false, "argv": ["conhost.exe", "{args}"], "args_enum": ["x"] },
  { "name": "wt", "ok": false, "argv": ["wt", "{args}"], "args_enum": ["x"] },
  { "name": "ubuntu.exe run", "ok": false, "argv": ["ubuntu.exe", "run", "{args}"], "args_enum": ["x"] },
  { "name": "pypy3", "ok": false, "argv": ["pypy3", "{out}"] },
  { "name": "powershell with a bare {args}", "ok": false, "argv": ["powershell", "-NoProfile", "{args}"], "args_enum": ["x"] },
  { "name": "pwsh {args}", "ok": false, "argv": ["pwsh", "{args}"], "args_enum": ["x"] },
  { "name": "python -m {args}", "ok": false, "argv": ["python", "-m", "{args}"], "args_enum": ["x"] },
  { "name": "sh {args}", "ok": false, "argv": ["sh", "{args}"], "args_enum": ["x"] },
  { "name": "env {args}", "ok": false, "argv": ["env", "{args}"], "args_enum": ["x"] },
  { "name": "node x.js {out}", "ok": false, "argv": ["node", "x.js", "{out}"] },
  { "name": "free text {args} in argv", "ok": false, "argv": ["git", "log", "{args}"], "args_pattern": "[a-z]+" },
  { "name": "an enum {args} in argv for a plain program", "ok": true, "argv": ["git", "switch", "{args}"], "args_enum": ["main", "dev"] },
  { "name": "free text {args} through env", "ok": true, "argv": ["tool.exe"], "env": { "AGENTBUS_ARGS": "{args}" }, "args_pattern": "[a-z]+" },
  { "name": "free text {args} through env to an interpreter", "ok": true, "argv": ["pwsh", "-NoProfile", "-Command", "Select-String -Pattern $env:AGENTBUS_ARGS x.log"], "env": { "AGENTBUS_ARGS": "{args}" }, "args_pattern": "[a-z]+" },
  { "name": "docs/agent-commands/screenshot.yaml (windows)", "ok": true, "argv": ["powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", "$ErrorActionPreference = 'Stop'\n$q = [string][char]34\n$u = '[DllImport(' + $q + 'user32.dll' + $q + ')] public static extern '\nAdd-Type -Namespace AgentbusShot -Name User32 -MemberDefinition ('[StructLayout(LayoutKind.Sequential)] public struct RECT { public int Left, Top, Right, Bottom; } ' + $u + 'bool SetProcessDPIAware(); ' + $u + 'bool IsIconic(IntPtr hWnd); ' + $u + 'bool GetWindowRect(IntPtr hWnd, out RECT rect); ' + $u + 'bool PrintWindow(IntPtr hWnd, IntPtr hdc, uint flags);')\nAdd-Type -AssemblyName System.Drawing, System.Windows.Forms\n[void][AgentbusShot.User32]::SetProcessDPIAware()\n$bmp = $null\ntry {\n  $child = Get-CimInstance Win32_Process -Filter ('ProcessId=' + $PID)\n  for ($i = 0; $i -lt 32 -and $child -and $child.ParentProcessId -gt 0; $i++) {\n    $parent = Get-CimInstance Win32_Process -Filter ('ProcessId=' + $child.ParentProcessId)\n    if (-not $parent -or -not $parent.CreationDate -or $parent.CreationDate -gt $child.CreationDate) { break }\n    $child = $parent\n    $proc = Get-Process -Id $parent.ProcessId -ErrorAction SilentlyContinue\n    if (-not $proc -or $proc.MainWindowHandle -eq [IntPtr]::Zero) { continue }\n    $hwnd = $proc.MainWindowHandle\n    $rect = New-Object AgentbusShot.User32+RECT\n    if ([AgentbusShot.User32]::IsIconic($hwnd) -or -not [AgentbusShot.User32]::GetWindowRect($hwnd, [ref]$rect)) { break }\n    $w = $rect.Right - $rect.Left\n    $h = $rect.Bottom - $rect.Top\n    if ($w -le 0 -or $h -le 0) { break }\n    $bmp = New-Object System.Drawing.Bitmap $w, $h\n    $g = [System.Drawing.Graphics]::FromImage($bmp)\n    $dc = $g.GetHdc()\n    $ok = [AgentbusShot.User32]::PrintWindow($hwnd, $dc, 2)\n    $g.ReleaseHdc($dc)\n    $g.Dispose()\n    if (-not $ok) { $bmp.Dispose(); $bmp = $null }\n    break\n  }\n} catch {\n  $bmp = $null\n}\nif (-not $bmp) {\n  $b = [System.Windows.Forms.Screen]::PrimaryScreen.Bounds\n  $bmp = New-Object System.Drawing.Bitmap $b.Width, $b.Height\n  $g = [System.Drawing.Graphics]::FromImage($bmp)\n  $g.CopyFromScreen($b.Location, [System.Drawing.Point]::Empty, $b.Size)\n  $g.Dispose()\n}\n$max = 3670016\n$img = $bmp\n$ms = New-Object System.IO.MemoryStream\n$img.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)\nwhile ($ms.Length -gt $max -and $img.Width -gt 640) {\n  $nw = [int]($img.Width * 0.75)\n  $nh = [Math]::Max(1, [int]($img.Height * 0.75))\n  $small = New-Object System.Drawing.Bitmap $nw, $nh\n  $g = [System.Drawing.Graphics]::FromImage($small)\n  $g.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::HighQualityBicubic\n  $g.DrawImage($img, 0, 0, $nw, $nh)\n  $g.Dispose()\n  $img = $small\n  $ms = New-Object System.IO.MemoryStream\n  $img.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)\n}\nif ($ms.Length -gt $max) {\n  $jpeg = [System.Drawing.Imaging.ImageCodecInfo]::GetImageEncoders() | Where-Object { $_.MimeType -eq 'image/jpeg' }\n  $params = New-Object System.Drawing.Imaging.EncoderParameters 1\n  $params.Param[0] = New-Object System.Drawing.Imaging.EncoderParameter ([System.Drawing.Imaging.Encoder]::Quality, [long]85)\n  $ms = New-Object System.IO.MemoryStream\n  $img.Save($ms, $jpeg, $params)\n}\n[System.IO.File]::WriteAllBytes($env:AGENTBUS_OUT, $ms.ToArray())\n"], "env": { "AGENTBUS_OUT": "{out}" }, "output": "image" },
  { "name": "cmd /c with %AGENTBUS_ARGS%", "ok": false, "argv": ["cmd", "/c", "tool.exe %AGENTBUS_ARGS%"], "env": { "AGENTBUS_ARGS": "{args}" }, "args_pattern": "[a-z]+" },
  { "name": "CMD.EXE. with a lowercase %agentbus_out%", "ok": false, "argv": ["C:\\Windows\\System32\\CMD.EXE.", "/c", "copy x.png %agentbus_out%"], "env": { "AGENTBUS_OUT": "{out}" }, "output": "image" },
  { "name": "cmd /v:on with !AGENTBUS_ARGS!", "ok": false, "argv": ["cmd", "/v:on", "/c", "tool.exe !AGENTBUS_ARGS!"], "env": { "AGENTBUS_ARGS": "{args}" }, "args_pattern": "[a-z]+" },
  { "name": "cmd after a %AGENTBUS_OUT% element", "ok": false, "argv": ["tool.exe", "%AGENTBUS_OUT%", "cmd"], "env": { "AGENTBUS_OUT": "{out}" }, "output": "image" },
  { "name": "cmd /c that leaves AGENTBUS_ARGS to the program", "ok": true, "argv": ["cmd", "/c", "tool.exe"], "env": { "AGENTBUS_ARGS": "{args}" }, "args_pattern": "[a-z]+" },
  { "name": "%AGENTBUS_ARGS% with no cmd to expand it", "ok": true, "argv": ["tool.exe", "--fmt=%AGENTBUS_ARGS%"], "env": { "AGENTBUS_ARGS": "{args}" }, "args_pattern": "[a-z]+" },
  { "name": "screencapture -x {out}", "ok": true, "argv": ["screencapture", "-x", "{out}"], "output": "image" },
  { "name": "an interpreter with flags and no placeholders", "ok": true, "argv": ["bash", "-e", "script.sh"] },
  { "name": "a versioned plain program", "ok": true, "argv": ["ffmpeg6", "-i", "in.mp4", "{out}"] }
] /* END SHELL CASES */
