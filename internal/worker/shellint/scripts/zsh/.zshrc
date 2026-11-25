# Syre terminal shell integration: sources your .zshrc, then adds the hooks.
if __syre_pre .zshrc; then source "$ZDOTDIR/.zshrc"; fi
__syre_post
source "$__syre_zdotdir/syre.zsh"
# A non-login shell reads no .zlogin: startup ends here.
[[ -o login ]] || __syre_restore
