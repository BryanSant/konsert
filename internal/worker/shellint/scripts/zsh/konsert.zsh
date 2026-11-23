# Konsert terminal shell integration hooks for zsh (Konsert terminals only).
#
# Emits OSC 133 A/B/C/D (prompt, input, pre-exec, exit status) and the Konsert-private
# OSC 633;E (command line) and OSC 633;P;Cwd= (working directory), which carry the
# per-terminal nonce. konsertd parses and strips the 633 marks from the terminal stream.
if [[ -o interactive && -n ${KONSERT_SHELL_NONCE-} && -z ${__konsert_installed-} ]]; then
	__konsert_installed=1
	__konsert_nonce=$KONSERT_SHELL_NONCE
	unset KONSERT_SHELL_NONCE
	__konsert_running=

	# __konsert_esc escapes $1 into __konsert_reply: "\" -> "\\", ";" -> "\x3b" and
	# control characters -> "\xNN", so the value can travel inside an OSC sequence.
	__konsert_esc() {
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
		__konsert_reply=$s
	}

	__konsert_precmd() {
		local ec=$?
		emulate -L zsh
		if [[ -n $__konsert_running ]]; then
			builtin printf '\e]133;D;%s\a' "$ec"
			__konsert_running=
		fi
		__konsert_esc "$PWD"
		builtin printf '\e]633;P;Cwd=%s;%s\a\e]133;A\a' "$__konsert_reply" "$__konsert_nonce"
		return $ec
	}

	# Runs after the other precmd hooks, which may rebuild the prompt (themes).
	__konsert_prompt_end() {
		local ec=$?
		emulate -L zsh
		if [[ -o promptpercent && $PS1 != *$'\e]133;B'* ]]; then
			PS1+=$'%{\e]133;B\a%}'
		fi
		return $ec
	}

	__konsert_preexec() {
		emulate -L zsh
		local cmd=$1
		[[ -n $cmd ]] || cmd=$3
		__konsert_esc "$cmd"
		builtin printf '\e]633;E;%s;%s\a\e]133;C\a' "$__konsert_reply" "$__konsert_nonce"
		__konsert_running=1
	}

	typeset -ga precmd_functions preexec_functions
	precmd_functions=(__konsert_precmd $precmd_functions __konsert_prompt_end)
	preexec_functions+=(__konsert_preexec)
fi
