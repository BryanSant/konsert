# Konsert terminal shell integration for bash (Konsert terminals only).
#
# konsertd starts bash with `--init-file <this file> -i`, so this file replaces
# ~/.bashrc as the startup file: it first runs your usual startup files (your dotfiles
# are not modified), then adds the hooks. KONSERT_SHELL_LOGIN=1 emulates a login shell
# (/etc/profile and the first of ~/.bash_profile, ~/.bash_login, ~/.profile).
#
# Emits OSC 133 A/B/C/D (prompt, input, pre-exec, exit status) and the Konsert-private
# OSC 633;E (command line) and OSC 633;P;Cwd= (working directory), which carry the
# per-terminal nonce. konsertd parses and strips the 633 marks from the terminal stream.
if [[ -n ${KONSERT_SHELL_LOGIN-} ]]; then
	unset KONSERT_SHELL_LOGIN
	if [[ -r /etc/profile ]]; then . /etc/profile; fi
	if [[ -r ~/.bash_profile ]]; then
		. ~/.bash_profile
	elif [[ -r ~/.bash_login ]]; then
		. ~/.bash_login
	elif [[ -r ~/.profile ]]; then
		. ~/.profile
	fi
else
	if [[ -r /etc/bash.bashrc ]]; then . /etc/bash.bashrc; fi
	if [[ -r ~/.bashrc ]]; then . ~/.bashrc; fi
fi

if [[ $- == *i* && -n ${KONSERT_SHELL_NONCE-} && -z ${__konsert_installed-} ]]; then
	__konsert_installed=1
	__konsert_nonce=$KONSERT_SHELL_NONCE
	unset KONSERT_SHELL_NONCE
	__konsert_running=
	__konsert_ready=
	__konsert_hist=

	# __konsert_esc escapes $1 into __konsert_reply: "\" -> "\\", ";" -> "\x3b" and
	# control characters -> "\xNN", so the value can travel inside an OSC sequence.
	__konsert_esc() {
		local s=$1 bs='\'
		s=${s//$bs/$bs$bs}
		s=${s//;/${bs}x3b}
		s=${s//$'\n'/${bs}x0a}
		s=${s//$'\r'/${bs}x0d}
		s=${s//$'\t'/${bs}x09}
		s=${s//$'\a'/${bs}x07}
		s=${s//$'\e'/${bs}x1b}
		s=${s//$'\x18'/${bs}x18}
		s=${s//$'\x1a'/${bs}x1a}
		__konsert_reply=$s
	}

	# First PROMPT_COMMAND entry: report the finished command, the cwd, prompt start.
	__konsert_precmd() {
		local ec=$?
		__konsert_ready=
		if [[ -n $__konsert_running ]]; then
			builtin printf '\e]133;D;%s\a' "$ec"
			__konsert_running=
		fi
		__konsert_esc "$PWD"
		builtin printf '\e]633;P;Cwd=%s;%s\a\e]133;A\a' "$__konsert_reply" "$__konsert_nonce"
		return $ec
	}

	# Last PROMPT_COMMAND entry: mark the end of the prompt, then arm the DEBUG trap.
	__konsert_prompt_end() {
		local ec=$?
		case $PS1 in
		*'133;B'*) ;;
		*) PS1=$PS1'\[\e]133;B\a\]' ;;
		esac
		__konsert_hist=$(HISTTIMEFORMAT= builtin history 1)
		__konsert_ready=1
		return $ec
	}

	# DEBUG trap: the first command after the prompt is the command line the user ran.
	# Its text comes from history; when history did not record it (HISTCONTROL,
	# HISTIGNORE, history off) the first simple command ($BASH_COMMAND) is used.
	__konsert_preexec() {
		[[ -n $__konsert_ready && -z ${COMP_LINE-} ]] || return 0
		case $BASH_COMMAND in __konsert_*) return 0 ;; esac
		__konsert_ready=
		local h cmd
		h=$(HISTTIMEFORMAT= builtin history 1)
		if [[ -n $h && $h != "$__konsert_hist" ]]; then
			cmd=${h#"${h%%[![:space:]]*}"}
			cmd=${cmd#*[[:space:]]}
			cmd=${cmd#"${cmd%%[![:space:]]*}"}
		else
			cmd=$BASH_COMMAND
		fi
		__konsert_esc "$cmd"
		builtin printf '\e]633;E;%s;%s\a\e]133;C\a' "$__konsert_reply" "$__konsert_nonce"
		__konsert_running=1
		return 0
	}

	# Keep an existing DEBUG trap (e.g. bash-preexec) running after ours. Passing "$_"
	# as the last argument preserves $_ for the user's command.
	__konsert_prev_trap=$(trap -p DEBUG)
	__konsert_prev_trap=${__konsert_prev_trap#"trap -- "}
	__konsert_prev_trap=${__konsert_prev_trap%" DEBUG"}
	if [[ -n $__konsert_prev_trap ]]; then eval "__konsert_prev_trap=$__konsert_prev_trap"; fi
	if [[ -n $__konsert_prev_trap ]]; then
		trap '__konsert_preexec "$_"; eval "$__konsert_prev_trap"' DEBUG
	else
		trap '__konsert_preexec "$_"' DEBUG
	fi

	if [[ $(declare -p PROMPT_COMMAND 2>/dev/null) == "declare -a"* ]]; then
		PROMPT_COMMAND=(__konsert_precmd "${PROMPT_COMMAND[@]}" __konsert_prompt_end)
	else
		__konsert_nl=$'\n'
		PROMPT_COMMAND=__konsert_precmd${PROMPT_COMMAND:+${__konsert_nl}$PROMPT_COMMAND}${__konsert_nl}__konsert_prompt_end
	fi
fi
