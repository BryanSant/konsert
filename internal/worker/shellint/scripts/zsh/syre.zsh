# Syre terminal shell integration hooks for zsh (Syre terminals only).
#
# Emits OSC 133 A/B/C/D (prompt, input, pre-exec, exit status) and the Syre-private
# OSC 633;E (command line) and OSC 633;P;Cwd= (working directory), which carry the
# per-terminal nonce. syred parses and strips the 633 marks from the terminal stream.
if [[ -o interactive && -n ${SYRE_SHELL_NONCE-} && -z ${__syre_installed-} ]]; then
	__syre_installed=1
	__syre_nonce=$SYRE_SHELL_NONCE
	unset SYRE_SHELL_NONCE
	__syre_running=

	# __syre_esc escapes $1 into __syre_reply: "\" -> "\\", ";" -> "\x3b" and
	# control characters -> "\xNN", so the value can travel inside an OSC sequence.
	__syre_esc() {
		emulate -L zsh
		local s=$1
		s=${s//\\/\\\\}
		s=${s//;/\\x3b}
		s=${s//$'\n'/\\x0a}
		s=${s//$'\r'/\\x0d}
		s=${s//$'\t'/\\x09}
		s=${s//$'\a'/\\x07}
		s=${s//$'\e'/\\x1b}
		s=${s//$'\x18'/\\x18}
		s=${s//$'\x1a'/\\x1a}
		__syre_reply=$s
	}

	__syre_precmd() {
		local ec=$?
		emulate -L zsh
		if [[ -n $__syre_running ]]; then
			builtin printf '\e]133;D;%s\a' "$ec"
			__syre_running=
		fi
		__syre_esc "$PWD"
		builtin printf '\e]633;P;Cwd=%s;%s\a\e]133;A\a' "$__syre_reply" "$__syre_nonce"
		return $ec
	}

	# Runs after the other precmd hooks, which may rebuild the prompt (themes).
	__syre_prompt_end() {
		local ec=$?
		emulate -L zsh
		if [[ -o promptpercent && $PS1 != *$'\e]133;B'* ]]; then
			PS1+=$'%{\e]133;B\a%}'
		fi
		return $ec
	}

	__syre_preexec() {
		emulate -L zsh
		local cmd=$1
		[[ -n $cmd ]] || cmd=$3
		__syre_esc "$cmd"
		builtin printf '\e]633;E;%s;%s\a\e]133;C\a' "$__syre_reply" "$__syre_nonce"
		__syre_running=1
	}

	typeset -ga precmd_functions preexec_functions
	precmd_functions=(__syre_precmd $precmd_functions __syre_prompt_end)
	preexec_functions+=(__syre_preexec)
fi
