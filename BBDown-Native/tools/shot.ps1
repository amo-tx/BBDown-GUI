# Launch the freshly built GUI, capture its window via PrintWindow
# (works even if the window is partly offscreen or obscured), then kill it.
# Verification only - does not touch user data.
#
# Pure ASCII on purpose: Windows PowerShell 5.1 reads .ps1 as ANSI, so any
# non-ASCII byte would become mojibake and break parsing.
#
# All Win32 interop lives in tools\Cap.cs, compiled by Add-Type -Path.
# Keeping the C# out of a here-string matters: compile failures there show up
# as *non-terminating* errors, so the script keeps running against a type that
# simply does not exist and every [Cap]:: call silently evaporates.
param(
    [string]$Exe,
    [string]$Png,
    [string]$Log,
    [int]$Wait = 6,
    [int]$ShrinkW = 0,
    [int]$ShrinkH = 0,
    [string]$Png2 = '',
    [string[]]$AppArgs = @()
)

$ErrorActionPreference = 'Continue'
function W($m) { Add-Content -Path $Log -Value "$m" -Encoding UTF8 }

Set-Content -Path $Log -Value "=== start ===" -Encoding UTF8

$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Add-Type -AssemblyName System.Drawing

# Both lines are needed and they do different jobs:
#   -AssemblyName System.Drawing  -> loads the assembly into the AppDomain
#   -ReferencedAssemblies         -> hands it to the CodeDom compiler, which
#                                    otherwise reports "System.Drawing.Imaging
#                                    does not exist" and bakes a broken type.
try {
    Add-Type -Path (Join-Path $root 'tools\Cap.cs') -ReferencedAssemblies System.Drawing -ErrorAction Stop
    W "Cap.cs compiled"
} catch {
    W ("COMPILE FAILED: " + $_.Exception.GetType().FullName)
    W ("  " + $_.Exception.Message)
    if ($_.Exception.InnerException -ne $null) { W ("  inner: " + $_.Exception.InnerException.Message) }
    exit 2
}

W ("screen = " + [Cap]::Metrics())

# Must happen before any measurement - see the comment on MakeDpiAware in Cap.cs.
W ([Cap]::MakeDpiAware())
W ("screen after dpi aware = " + [Cap]::Metrics())

$proc = Start-Process -FilePath $Exe -ArgumentList $AppArgs -PassThru
W ("started pid = " + $proc.Id + " args = " + ($AppArgs -join ' '))
Start-Sleep -Seconds $Wait
$proc.Refresh()
if ($proc.HasExited) { W ("exited early, code = " + $proc.ExitCode); exit 1 }

$h = $proc.MainWindowHandle
W ("hwnd = " + $h)
if ($h -eq [IntPtr]::Zero) { W "no hwnd"; $proc.Kill(); exit 1 }

try {
    W ("info: " + [Cap]::Info($h))
    W ("dpi: " + [Cap]::Dpi($h))
    # Dump the control tree first: if the layout overflows, the numbers say so
    # unambiguously, whereas a screenshot only hints at it.
    W "--- control tree (rects relative to window origin, physical px) ---"
    Add-Content -Path $Log -Value ([Cap]::Dump($h)) -Encoding UTF8
    W ("capture: " + [Cap]::Save($h, $Png))

    # Minimum-size pass: the layout must also survive being shrunk. A block whose
    # height collapses to 0 here would be invisible in the default-size shot.
    if ($ShrinkW -gt 0 -and $ShrinkH -gt 0) {
        W "--- shrink pass ---"
        W ([Cap]::ResizeTo($h, $ShrinkW, $ShrinkH))
        Start-Sleep -Milliseconds 900
        W ("info: " + [Cap]::Info($h))
        W "--- control tree at minimum size ---"
        Add-Content -Path $Log -Value ([Cap]::Dump($h)) -Encoding UTF8
        if ($Png2 -ne '') { W ("capture: " + [Cap]::Save($h, $Png2)) }
    }
} catch {
    W ("CAPTURE FAILED: " + $_.Exception.Message)
    if ($_.Exception.InnerException -ne $null) { W ("  inner: " + $_.Exception.InnerException.Message) }
}

if (-not $proc.HasExited) { $proc.Kill(); W "killed" }
Add-Content -Path $Log -Value "=== done ===" -Encoding UTF8
