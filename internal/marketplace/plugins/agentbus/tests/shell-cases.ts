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
  { "name": "the Windows screenshot command", "ok": true, "argv": ["powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", "Add-Type -AssemblyName System.Windows.Forms; $b = [System.Windows.Forms.Screen]::PrimaryScreen.Bounds; $bmp = New-Object System.Drawing.Bitmap $b.Width, $b.Height; [System.Drawing.Graphics]::FromImage($bmp).CopyFromScreen($b.Location, [System.Drawing.Point]::Empty, $b.Size); $bmp.Save($env:AGENTBUS_OUT)"], "env": { "AGENTBUS_OUT": "{out}" }, "output": "image" },
  { "name": "screencapture -x {out}", "ok": true, "argv": ["screencapture", "-x", "{out}"], "output": "image" },
  { "name": "an interpreter with flags and no placeholders", "ok": true, "argv": ["bash", "-e", "script.sh"] },
  { "name": "a versioned plain program", "ok": true, "argv": ["ffmpeg6", "-i", "in.mp4", "{out}"] }
] /* END SHELL CASES */
