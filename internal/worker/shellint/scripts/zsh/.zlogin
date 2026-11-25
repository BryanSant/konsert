# Syre terminal shell integration: sources your .zlogin (see .zshenv).
if __syre_pre .zlogin; then source "$ZDOTDIR/.zlogin"; fi
__syre_post
# Interactive startup ends here. A non-interactive login shell (a terminal that runs a
# command first, then `exec zsh -l`) keeps ZDOTDIR so the exec'd shell is integrated too.
[[ -o interactive ]] && __syre_restore
