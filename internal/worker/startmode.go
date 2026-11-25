package worker

// How a Windows syred was started (MachineFacts.StartMode, SYR-224K5).
const (
	// StartModeService: a Windows service (`syred install --service`).
	StartModeService = "service"
	// StartModeBackgroundTask: a scheduled task that runs whether or not the user is
	// signed in (S4U, batch logon), at boot as well.
	StartModeBackgroundTask = "background_task"
	// StartModeUserSession: in a signed-in user's session (the logon task, or by hand):
	// it only comes back once that user signs in.
	StartModeUserSession = "user_session"
)

// startModeOf classifies a Windows token: a service, a batch logon (S4U task), or an
// interactive session; "" for anything else (an SSH session, run by hand).
func startModeOf(service, batch bool, session uint32) string {
	switch {
	case service:
		return StartModeService
	case batch:
		return StartModeBackgroundTask
	case session != 0:
		return StartModeUserSession
	}
	return ""
}
