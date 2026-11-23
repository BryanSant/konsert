# Konsert terminal shell integration for PowerShell (Konsert terminals only).
#
# konsertd starts pwsh/powershell with -NoExit -Command that dot-sources this file as a
# script block (so execution policy does not block it), after your profile has loaded
# as usual (your profile is not modified). It wraps your prompt function and chains
# PSReadLine's AddToHistoryHandler, which sees each accepted command line before it runs.
#
# Emits OSC 133 A/B/C/D (prompt, input, pre-exec, exit status) and the Konsert-private
# OSC 633;E (command line) and OSC 633;P;Cwd= (working directory), which carry the
# per-terminal nonce. konsertd parses and strips the 633 marks from the terminal stream.
if ($global:__KonsertInstalled -or -not $env:KONSERT_SHELL_NONCE) { return }
$global:__KonsertInstalled = $true
$global:__KonsertNonce = $env:KONSERT_SHELL_NONCE
Remove-Item Env:KONSERT_SHELL_NONCE -ErrorAction SilentlyContinue
$global:__KonsertRunning = $false
$global:__KonsertNative = $false
$global:__KonsertOrigPrompt = $function:prompt
# The -Command line that loaded this file may land in history: the first prompt only
# records the history position.
$global:__KonsertLastHistoryId = -1

# Escapes a value for an OSC payload: "\" -> "\\", ";" -> "\x3b", controls -> "\xNN".
function global:__KonsertEsc([string]$s) {
    $sb = New-Object System.Text.StringBuilder
    foreach ($ch in $s.ToCharArray()) {
        $c = [int]$ch
        if ($ch -eq [char]'\') { [void]$sb.Append('\\') }
        elseif ($ch -eq [char]';') { [void]$sb.Append('\x3b') }
        elseif ($c -lt 0x20 -or $c -eq 0x7f) { [void]$sb.Append(('\x{0:x2}' -f $c)) }
        else { [void]$sb.Append($ch) }
    }
    $sb.ToString()
}

function global:__KonsertOsc([string]$body) { [string][char]27 + ']' + $body + [char]7 }

function global:__KonsertStart([string]$line) {
    (__KonsertOsc ('633;E;' + (__KonsertEsc $line) + ';' + $global:__KonsertNonce)) + (__KonsertOsc '133;C')
}

# Reports whether a command line runs a native program or script, whose exit status is
# $LASTEXITCODE. ($LASTEXITCODE keeps its value across cmdlets, so it is only trusted for
# such lines; a failing cmdlet reports 1.)
function global:__KonsertUsesNative([string]$line) {
    try {
        $ast = [System.Management.Automation.Language.Parser]::ParseInput($line, [ref]$null, [ref]$null)
        $cmds = $ast.FindAll({ param($n) $n -is [System.Management.Automation.Language.CommandAst] }, $true)
        foreach ($c in $cmds) {
            $name = $c.GetCommandName()
            if (-not $name) { continue }
            $info = Get-Command -Name $name -ErrorAction SilentlyContinue | Select-Object -First 1
            if ($info -and ($info.CommandType -eq 'Application' -or $info.CommandType -eq 'ExternalScript')) { return $true }
        }
    } catch { }
    $false
}

# Called by PSReadLine when a line is accepted, before it runs.
function global:__KonsertPreexec([string]$line) {
    if ([string]::IsNullOrWhiteSpace($line) -or $global:__KonsertRunning) { return }
    $global:__KonsertRunning = $true
    $global:__KonsertNative = __KonsertUsesNative $line
    [Console]::Write((__KonsertStart $line))
}

function global:prompt {
    $ok = $?
    $lec = $global:LASTEXITCODE
    $out = ''
    $hist = Get-History -Count 1
    $histId = 0
    if ($hist) { $histId = $hist.Id }
    if ($global:__KonsertLastHistoryId -lt 0) {
        $global:__KonsertLastHistoryId = $histId
    } elseif ($histId -ne $global:__KonsertLastHistoryId) {
        $global:__KonsertLastHistoryId = $histId
        if (-not $global:__KonsertRunning) {
            # No PSReadLine (or its handler did not run): report the command afterwards.
            $out += __KonsertStart $hist.CommandLine
            $global:__KonsertRunning = $true
            $global:__KonsertNative = __KonsertUsesNative $hist.CommandLine
        }
    }
    if ($global:__KonsertRunning) {
        $code = 1
        if ($ok) { $code = 0 } elseif ($global:__KonsertNative -and $lec -is [int] -and $lec -ne 0) { $code = $lec }
        $out += __KonsertOsc ('133;D;' + $code)
        $global:__KonsertRunning = $false
    }
    $loc = $executionContext.SessionState.Path.CurrentLocation
    $cwd = $loc.Path
    if ($loc.Provider.Name -eq 'FileSystem') { $cwd = $loc.ProviderPath }
    $out += __KonsertOsc ('633;P;Cwd=' + (__KonsertEsc $cwd) + ';' + $global:__KonsertNonce)
    $out += __KonsertOsc '133;A'
    $p = ''
    if ($global:__KonsertOrigPrompt) { $p = (& $global:__KonsertOrigPrompt) -join '' }
    if ([string]::IsNullOrEmpty($p)) { $p = 'PS ' + $loc.Path + '> ' }
    $global:LASTEXITCODE = $lec
    $out + $p + (__KonsertOsc '133;B')
}

if (Get-Module -Name PSReadLine) {
    $global:__KonsertOrigHistoryHandler = (Get-PSReadLineOption).AddToHistoryHandler
    Set-PSReadLineOption -AddToHistoryHandler {
        param([string]$line)
        # PSReadLine 2.0 (Windows PowerShell 5.1) also calls this for every line it loads
        # from the history file; only the line just accepted is in the edit buffer.
        $buf = $null
        $cur = 0
        try { [Microsoft.PowerShell.PSConsoleReadLine]::GetBufferState([ref]$buf, [ref]$cur) } catch { $buf = $line }
        if ($buf -eq $line) { __KonsertPreexec $line }
        if ($global:__KonsertOrigHistoryHandler) { return (& $global:__KonsertOrigHistoryHandler $line) }
        return $true
    }
}
