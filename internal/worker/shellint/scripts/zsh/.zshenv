# Konsert terminal shell integration. konsertd points ZDOTDIR here for Konsert
# terminals only; each file sources your own startup file (your dotfiles are not
# modified) and the hooks are added after your .zshrc.
__konsert_zdotdir=$ZDOTDIR
if [[ -n ${KONSERT_USER_ZDOTDIR+x} ]]; then
	__konsert_user_zdotdir=$KONSERT_USER_ZDOTDIR
else
	__konsert_user_zdotdir=$HOME
fi
# Run the user's file with their ZDOTDIR (scripts may read it), at top level so its
# typeset/local behave as usual. A file may change ZDOTDIR; remember that for later files.
__konsert_pre() {
	ZDOTDIR=$__konsert_user_zdotdir
	[[ $ZDOTDIR != "$__konsert_zdotdir" && -r $ZDOTDIR/$1 ]]
}
__konsert_post() {
	__konsert_user_zdotdir=$ZDOTDIR
	ZDOTDIR=$__konsert_zdotdir
}
# Hand ZDOTDIR back to the user once startup is over, so nested shells use their config.
__konsert_restore() {
	if [[ -z ${KONSERT_USER_ZDOTDIR+x} && $__konsert_user_zdotdir == "$HOME" ]]; then
		unset ZDOTDIR
	else
		export ZDOTDIR=$__konsert_user_zdotdir
	fi
}
if __konsert_pre .zshenv; then source "$ZDOTDIR/.zshenv"; fi
__konsert_post
