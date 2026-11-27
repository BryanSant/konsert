package chat

import (
	"strings"
	"testing"

	"github.com/BryanSant/syre/internal/worker/guard"
)

func TestShedMessageSaysWhenTheMachineWasFullElsewhere(t *testing.T) {
	hog := guard.Evidence{Kind: "chat", Reason: guard.ReasonBelowReserve, Reserve: 410 * mib, RSS: 6 << 30, HostUsed: 7 << 30}
	if m := ShedMessage(hog); strings.Contains(m, "elsewhere") || !strings.Contains(m, "at 6.0 GiB") {
		t.Fatalf("culprit message = %q", m)
	}
	by := hog
	by.RSS, by.Bystander = 300*mib, true
	m := ShedMessage(by)
	if !strings.Contains(m, "Most of the 7.0 GiB in use was elsewhere on the machine") || !strings.HasSuffix(m, "the work can run again.") {
		t.Fatalf("bystander message = %q", m)
	}
	if ev := ShedEvidence(by); !ev.Bystander || ev.HostUsedMB != 7<<10 {
		t.Fatalf("protocol evidence = %+v", ev)
	}
}
