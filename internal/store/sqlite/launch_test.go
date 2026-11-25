package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/store"
)

func TestEnsureWorkerPreCreatesAndHelloKeepsLaunch(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "syre.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	const id = "01890a5d-ac96-774b-bcce-b302099a8057"
	w, err := st.EnsureWorker(ctx, store.Worker{
		ID: id, Name: "box", OS: "linux", Arch: "amd64",
		Launch: store.LaunchSSH, SSHTarget: "me@box",
	})
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != "offline" || !strings.HasPrefix(w.SID, "m-") || w.HostGroup != "box" ||
		w.Launch != store.LaunchSSH || w.SSHTarget != "me@box" {
		t.Fatalf("pre-created worker = %+v", w)
	}

	if err := st.SetWorkerLaunchState(ctx, id, store.LaunchStateFailed, "boom", time.Now()); err != nil {
		t.Fatal(err)
	}
	// worker.hello refreshes the row but must keep the sid and the launch settings.
	if err := st.UpsertWorker(ctx, store.Worker{ID: id, Name: "box", HostGroup: "box", OS: "linux", Arch: "amd64", Status: "online"}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetWorker(ctx, id)
	if got.SID != w.SID || got.Status != "online" || got.Launch != store.LaunchSSH || got.SSHTarget != "me@box" ||
		got.LaunchState != store.LaunchStateFailed || got.LaunchError != "boom" || got.LaunchAt == nil {
		t.Fatalf("after hello = %+v", got)
	}

	// Ensuring again only changes the launch fields.
	got, err = st.EnsureWorker(ctx, store.Worker{ID: id, Name: "ignored", Launch: store.LaunchLocal})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "box" || got.Status != "online" || got.Launch != store.LaunchLocal || got.SSHTarget != "" || got.SID != w.SID {
		t.Fatalf("re-ensure = %+v", got)
	}
	if err := st.SetWorkerLaunchState(ctx, "missing", "", "", time.Time{}); err != store.ErrNotFound {
		t.Fatalf("unknown worker: %v", err)
	}
}
