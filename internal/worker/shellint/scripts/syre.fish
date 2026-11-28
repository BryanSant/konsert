# Syre shell integration for fish 3.x and 4.x (SYR-0N110). Loaded with
# `fish --init-command 'source …'`, which runs after the user's config.fish, so their
# configuration is never touched. Syre starts fish 4 with --features no-mark-prompt:
# fish's own OSC 133 marks would arrive before the command-line mark Syre needs; these
# hooks emit them in the order the tracker reads them.

if not status is-interactive
    exit
end

set -g __syre_nonce $SYRE_SHELL_NONCE
set -e SYRE_SHELL_NONCE

# VS Code OSC 633 escaping: "\" is "\\", ";" is "\x3b", control characters are "\xNN".
# A command typed over several lines is reported as typed (newlines become \x0a).
function __syre_esc
    printf '%s' $argv[1] | string replace -a -- '\\' '\\\\' | string replace -a -- ';' '\\x3b' | string replace -a -- \t '\\x09' | string replace -a -- \r '\\x0d' | string replace -a -- \e '\\x1b' | string replace -a -- \a '\\x07' | string join -- '\\x0a'
end

function __syre_prompt --on-event fish_prompt
    printf '\e]633;P;Cwd=%s;%s\a\e]133;A\a' (__syre_esc $PWD) $__syre_nonce
end

function __syre_preexec --on-event fish_preexec
    printf '\e]633;E;%s;%s\a\e]133;C\a' (__syre_esc $argv[1]) $__syre_nonce
end

function __syre_postexec --on-event fish_postexec
    printf '\e]133;D;%s\a' $status
end
