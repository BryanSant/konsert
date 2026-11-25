# Syre terminal shell integration for PowerShell (Syre terminals only).
#
# syred starts pwsh/powershell with -NoExit -Command that dot-sources this file as a
# script block (so execution policy does not block it), after your profile has loaded
# as usual (your profile is not modified). It wraps your prompt function and chains
# PSReadLine's AddToHistoryHandler, which sees each accepted command line before it runs.
#
# Emits OSC 133 A/B/C/D (prompt, input, pre-exec, exit status) and the Syre-private
# OSC 633;E (command line) and OSC 633;P;Cwd= (working directory), which carry the
# per-terminal nonce. syred parses and strips the 633 marks from the terminal stream.
if ($global:__SyreInstalled -or -not $env:SYRE_SHELL_NONCE) { return }
$global:__SyreInstalled = $true
$global:__SyreNonce = $env:SYRE_SHELL_NONCE
Remove-Item Env:SYRE_SHELL_NONCE -ErrorAction SilentlyContinue
$global:__SyreRunning = $false
$global:__SyreNative = $false
$global:__SyreOrigPrompt = $function:prompt
# The -Command line that loaded this file may land in history: the first prompt only
# records the history position.
$global:__SyreLastHistoryId = -1

# Escapes a value for an OSC payload: "\" -> "\\", ";" -> "\x3b", controls and every
# non-ASCII byte of the UTF-8 encoding -> "\xNN". The payload is pure ASCII, so the console
# code page (Windows PowerShell 5.1 often writes through a legacy OEM/ANSI page that can't
# represent characters like U+2713) can't mangle it.
function global:__SyreEsc([string]$s) {
    $sb = New-Object System.Text.StringBuilder
    foreach ($b in [System.Text.Encoding]::UTF8.GetBytes($s)) {
        if ($b -eq 0x5c) { [void]$sb.Append('\\') }
        elseif ($b -eq 0x3b) { [void]$sb.Append('\x3b') }
        elseif ($b -lt 0x20 -or $b -ge 0x7f) { [void]$sb.Append(('\x{0:x2}' -f $b)) }
        else { [void]$sb.Append([char]$b) }
    }
    $sb.ToString()
}

function global:__SyreOsc([string]$body) { [string][char]27 + ']' + $body + [char]7 }

function global:__SyreStart([string]$line) {
    (__SyreOsc ('633;E;' + (__SyreEsc $line) + ';' + $global:__SyreNonce)) + (__SyreOsc '133;C')
}

# Reports whether a command line runs a native program or script, whose exit status is
# $LASTEXITCODE. ($LASTEXITCODE keeps its value across cmdlets, so it is only trusted for
# such lines; a failing cmdlet reports 1.)
function global:__SyreUsesNative([string]$line) {
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
function global:__SyrePreexec([string]$line) {
    if ([string]::IsNullOrWhiteSpace($line) -or $global:__SyreRunning) { return }
    $global:__SyreRunning = $true
    $global:__SyreNative = __SyreUsesNative $line
    [Console]::Write((__SyreStart $line))
}

function global:prompt {
    $ok = $?
    $lec = $global:LASTEXITCODE
    $out = ''
    $hist = Get-History -Count 1
    $histId = 0
    if ($hist) { $histId = $hist.Id }
    if ($global:__SyreLastHistoryId -lt 0) {
        $global:__SyreLastHistoryId = $histId
    } elseif ($histId -ne $global:__SyreLastHistoryId) {
        $global:__SyreLastHistoryId = $histId
        if (-not $global:__SyreRunning) {
            # No PSReadLine (or its handler did not run): report the command afterwards.
            $out += __SyreStart $hist.CommandLine
            $global:__SyreRunning = $true
            $global:__SyreNative = __SyreUsesNative $hist.CommandLine
        }
    }
    if ($global:__SyreRunning) {
        $code = 1
        if ($ok) { $code = 0 } elseif ($global:__SyreNative -and $lec -is [int] -and $lec -ne 0) { $code = $lec }
        $out += __SyreOsc ('133;D;' + $code)
        $global:__SyreRunning = $false
    }
    $loc = $executionContext.SessionState.Path.CurrentLocation
    $cwd = $loc.Path
    if ($loc.Provider.Name -eq 'FileSystem') { $cwd = $loc.ProviderPath }
    $out += __SyreOsc ('633;P;Cwd=' + (__SyreEsc $cwd) + ';' + $global:__SyreNonce)
    $out += __SyreOsc '133;A'
    $p = ''
    if ($global:__SyreOrigPrompt) { $p = (& $global:__SyreOrigPrompt) -join '' }
    if ([string]::IsNullOrEmpty($p)) { $p = 'PS ' + $loc.Path + '> ' }
    $global:LASTEXITCODE = $lec
    $out + $p + (__SyreOsc '133;B')
}

if (Get-Module -Name PSReadLine) {
    $global:__SyreOrigHistoryHandler = (Get-PSReadLineOption).AddToHistoryHandler
    Set-PSReadLineOption -AddToHistoryHandler {
        param([string]$line)
        # PSReadLine 2.0 (Windows PowerShell 5.1) also calls this for every line it loads
        # from the history file; only the line just accepted is in the edit buffer.
        $buf = $null
        $cur = 0
        try { [Microsoft.PowerShell.PSConsoleReadLine]::GetBufferState([ref]$buf, [ref]$cur) } catch { $buf = $line }
        if ($buf -eq $line) { __SyrePreexec $line }
        if ($global:__SyreOrigHistoryHandler) { return (& $global:__SyreOrigHistoryHandler $line) }
        return $true
    }
}
