# Syre terminal shell integration. syred points ZDOTDIR here for Syre
# terminals only; each file sources your own startup file (your dotfiles are not
# modified) and the hooks are added after your .zshrc.
__syre_zdotdir=$ZDOTDIR
if [[ -n ${SYRE_USER_ZDOTDIR+x} ]]; then
	__syre_user_zdotdir=$SYRE_USER_ZDOTDIR
else
	__syre_user_zdotdir=$HOME
fi
# Run the user's file with their ZDOTDIR (scripts may read it), at top level so its
# typeset/local behave as usual. A file may change ZDOTDIR; remember that for later files.
__syre_pre() {
	ZDOTDIR=$__syre_user_zdotdir
	[[ $ZDOTDIR != "$__syre_zdotdir" && -r $ZDOTDIR/$1 ]]
}
__syre_post() {
	__syre_user_zdotdir=$ZDOTDIR
	ZDOTDIR=$__syre_zdotdir
}
# Hand ZDOTDIR back to the user once startup is over, so nested shells use their config.
__syre_restore() {
	if [[ -z ${SYRE_USER_ZDOTDIR+x} && $__syre_user_zdotdir == "$HOME" ]]; then
		unset ZDOTDIR
	else
		export ZDOTDIR=$__syre_user_zdotdir
	fi
}
if __syre_pre .zshenv; then source "$ZDOTDIR/.zshenv"; fi
__syre_post
