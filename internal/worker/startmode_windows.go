//go:build windows

package worker

import (
	"github.com/BryanSant/syre/internal/protocol"
	"golang.org/x/sys/windows"
)

// collectStartMode says how this syred was started, from its own token (SYR-224K5),
// so the hub can tell whether it comes back with nobody signed in.
func collectStartMode(f *protocol.MachineFacts) {
	member := func(sid string) bool {
		s, err := windows.StringToSid(sid)
		if err != nil {
			return false
		}
		// Token 0: the calling thread's effective token (CheckTokenMembership).
		ok, err := windows.Token(0).IsMember(s)
		return err == nil && ok
	}
	var session uint32
	_ = windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session)
	f.StartMode = startModeOf(member("S-1-5-6"), member("S-1-5-3"), session)
}
