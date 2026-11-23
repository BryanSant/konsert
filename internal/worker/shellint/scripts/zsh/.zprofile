# Konsert terminal shell integration: sources your .zprofile (see .zshenv).
if __konsert_pre .zprofile; then source "$ZDOTDIR/.zprofile"; fi
__konsert_post
