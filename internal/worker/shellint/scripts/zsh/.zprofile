# Syre terminal shell integration: sources your .zprofile (see .zshenv).
if __syre_pre .zprofile; then source "$ZDOTDIR/.zprofile"; fi
__syre_post
