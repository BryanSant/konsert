# Konsert terminal shell integration: sources your .zshrc, then adds the hooks.
if __konsert_pre .zshrc; then source "$ZDOTDIR/.zshrc"; fi
__konsert_post
source "$__konsert_zdotdir/konsert.zsh"
# A non-login shell reads no .zlogin: startup ends here.
[[ -o login ]] || __konsert_restore
