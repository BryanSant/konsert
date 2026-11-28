package realname

import (
	"context"
	"os"
	"os/user"
	"testing"
)

// SYRE_REALNAME_LIVE=1 prints what this machine reports for the current user.
func TestLiveDetect(t *testing.T) {
	if os.Getenv("SYRE_REALNAME_LIVE") == "" {
		t.Skip("set SYRE_REALNAME_LIVE=1 to look up the current user's name")
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("login=%s name=%q", u.Username, Detect(context.Background(), u.Username))
}
