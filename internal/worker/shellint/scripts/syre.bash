# Syre terminal shell integration for bash (Syre terminals only).
#
# syred starts bash with `--init-file <this file> -i`, so this file replaces
# ~/.bashrc as the startup file: it first runs your usual startup files (your dotfiles
# are not modified), then adds the hooks. SYRE_SHELL_LOGIN=1 emulates a login shell
# (/etc/profile and the first of ~/.bash_profile, ~/.bash_login, ~/.profile).
#
# Emits OSC 133 A/B/C/D (prompt, input, pre-exec, exit status) and the Syre-private
# OSC 633;E (command line) and OSC 633;P;Cwd= (working directory), which carry the
# per-terminal nonce. syred parses and strips the 633 marks from the terminal stream.
if [[ -n ${SYRE_SHELL_LOGIN-} ]]; then
	unset SYRE_SHELL_LOGIN
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

if [[ $- == *i* && -n ${SYRE_SHELL_NONCE-} && -z ${__syre_installed-} ]]; then
	__syre_installed=1
	__syre_nonce=$SYRE_SHELL_NONCE
	unset SYRE_SHELL_NONCE
	__syre_running=
	__syre_ready=
	__syre_hist=

	# __syre_esc escapes $1 into __syre_reply: "\" -> "\\", ";" -> "\x3b" and
	# control characters -> "\xNN", so the value can travel inside an OSC sequence.
	__syre_esc() {
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
		__syre_reply=$s
	}

	# First PROMPT_COMMAND entry: report the finished command, the cwd, prompt start.
	__syre_precmd() {
		local ec=$?
		__syre_ready=
		if [[ -n $__syre_running ]]; then
			builtin printf '\e]133;D;%s\a' "$ec"
			__syre_running=
		fi
		local cwd=$PWD
		# Git Bash / MSYS2: /c/... is translated by the worker; other mounts (/, /tmp,
		# /usr) only cygpath can map, so fork it just for those.
		if [[ -n $MSYSTEM && ! $cwd =~ ^/[a-zA-Z](/|$) ]] && builtin type -P cygpath >/dev/null; then
			cwd=$(cygpath -w "$cwd" 2>/dev/null) || cwd=$PWD
		fi
		__syre_esc "$cwd"
		builtin printf '\e]633;P;Cwd=%s;%s\a\e]133;A\a' "$__syre_reply" "$__syre_nonce"
		return $ec
	}

	# Last PROMPT_COMMAND entry: mark the end of the prompt, then arm the DEBUG trap.
	__syre_prompt_end() {
		local ec=$?
		case $PS1 in
		*'133;B'*) ;;
		*) PS1=$PS1'\[\e]133;B\a\]' ;;
		esac
		__syre_hist=$(HISTTIMEFORMAT= builtin history 1)
		__syre_ready=1
		return $ec
	}

	# DEBUG trap: the first command after the prompt is the command line the user ran.
	# Its text comes from history; when history did not record it (HISTCONTROL,
	# HISTIGNORE, history off) the first simple command ($BASH_COMMAND) is used.
	__syre_preexec() {
		[[ -n $__syre_ready && -z ${COMP_LINE-} ]] || return 0
		case $BASH_COMMAND in __syre_*) return 0 ;; esac
		__syre_ready=
		local h cmd
		h=$(HISTTIMEFORMAT= builtin history 1)
		if [[ -n $h && $h != "$__syre_hist" ]]; then
			cmd=${h#"${h%%[![:space:]]*}"}
			cmd=${cmd#*[[:space:]]}
			cmd=${cmd#"${cmd%%[![:space:]]*}"}
		else
			cmd=$BASH_COMMAND
		fi
		__syre_esc "$cmd"
		builtin printf '\e]633;E;%s;%s\a\e]133;C\a' "$__syre_reply" "$__syre_nonce"
		__syre_running=1
		return 0
	}

	# Keep an existing DEBUG trap (e.g. bash-preexec) running after ours. Passing "$_"
	# as the last argument preserves $_ for the user's command.
	__syre_prev_trap=$(trap -p DEBUG)
	__syre_prev_trap=${__syre_prev_trap#"trap -- "}
	__syre_prev_trap=${__syre_prev_trap%" DEBUG"}
	if [[ -n $__syre_prev_trap ]]; then eval "__syre_prev_trap=$__syre_prev_trap"; fi
	if [[ -n $__syre_prev_trap ]]; then
		trap '__syre_preexec "$_"; eval "$__syre_prev_trap"' DEBUG
	else
		trap '__syre_preexec "$_"' DEBUG
	fi

	if [[ $(declare -p PROMPT_COMMAND 2>/dev/null) == "declare -a"* ]]; then
		PROMPT_COMMAND=(__syre_precmd "${PROMPT_COMMAND[@]}" __syre_prompt_end)
	else
		__syre_nl=$'\n'
		PROMPT_COMMAND=__syre_precmd${PROMPT_COMMAND:+${__syre_nl}$PROMPT_COMMAND}${__syre_nl}__syre_prompt_end
	fi
fi
