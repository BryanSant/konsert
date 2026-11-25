package controller

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/BryanSant/konsert/internal/worker"
	"github.com/BryanSant/konsert/internal/worker/drivers"
)

// End to end over the SSH transport with a real konsertd and the mock agent (KON-DTN92):
// the agent's ask_user calls go to konsertd's loopback relay and over the SSH link, with
// no public URL and no hub URL recorded for the machine. After the link is dropped and
// re-dialed, the agent's same MCP session keeps working.
func TestMCPRelayOverSSHTransport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const wid = "01890a5d-ac96-774b-bcce-b302099a8070"
	t.Setenv("HOME", t.TempDir())
	t.Setenv(worker.StateDirEnv, t.TempDir())
	sockDir, err := os.MkdirTemp("", "ks")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "run", "d.sock")

	wCfg := config.DefaultWorkerConfig()
	wCfg.WorkerID = wid
	wCfg.Name = "relaybox"
	wCfg.EnableMockDriver = true
	daemon := newTestDaemon(t, wCfg, drivers.DefaultRegistry, nil)
	daemon.SocketPath = sock
	daemonDone := make(chan struct{})
	go func() {
		defer close(daemonDone)
		_ = daemon.Run(ctx)
	}()
	defer func() {
		cancel()
		<-daemonDone
	}()

	cs := newChatServer(t)
	cs.srv.hubKeyFile = filepath.Join(t.TempDir(), HubKeyFileName)
	ssh := &fakeSSHDialer{sock: sock}
	cs.srv.attach.dialSSH = ssh.dial
	cs.srv.attach.minBackoff, cs.srv.attach.maxBackoff = 20*time.Millisecond, 200*time.Millisecond
	w, err := cs.st.EnsureWorker(ctx, store.Worker{ID: wid, Name: "relaybox", Launch: store.LaunchSSH, SSHTarget: "me@relaybox"})
	if err != nil {
		t.Fatal(err)
	}
	cs.srv.attach.ensure(w)
	defer cs.srv.attach.stop(wid)
	waitFor(t, 10*time.Second, "ssh machine online", func() bool { return cs.h.IsWorkerConnected(wid) })
	var acct string
	waitFor(t, 10*time.Second, "mock account", func() bool {
		views, _ := cs.h.agentAccountViews(ctx, wid)
		for _, v := range views {
			if v.Agent == "mock" && v.ChatSupported {
				acct = v.ID
				return true
			}
		}
		return false
	})
	chat := cs.createChat(t, map[string]any{"worker_id": wid, "agent": "mock", "account_id": acct})

	// A rejected ask_user (validation answer) then a blocking one, both relayed.
	cs.send(t, chat, "vague question")
	q := cs.waitQuestion(t, chat.ID, "open question", isPending)
	if q.Rejections != 1 {
		t.Fatalf("question: %+v", q)
	}
	cs.postQuestion(t, q, "answer", map[string]any{"option_id": "option_2"})
	cs.waitReply(t, chat.ID, "Rejected first", `The user chose: "No, keep the strict exit criteria".`)

	// Drop the SSH link: the hub re-dials, and the agent's MCP session still works.
	ssh.kill()
	waitFor(t, 10*time.Second, "re-dial", func() bool { return ssh.count() >= 2 && cs.h.IsWorkerConnected(wid) })
	cs.send(t, chat, "question")
	q2 := cs.waitQuestion(t, chat.ID, "second question", func(x store.Question) bool { return x.ID != q.ID && x.Pending() })
	cs.postQuestion(t, q2, "answer", map[string]any{"option_id": "option_1"})
	cs.waitReply(t, chat.ID, "Ask result:", `The user chose: "Yes".`)
}
