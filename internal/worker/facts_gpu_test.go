package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/protocol"
)

func TestParseNvidiaSMI(t *testing.T) {
	got := parseNvidiaSMI("NVIDIA GeForce RTX 3090, 24576\nNVIDIA A100-SXM4-80GB, 81920\n")
	if len(got) != 2 || got[0].Model != "NVIDIA GeForce RTX 3090" || got[0].VRAMMB != 24576 || got[0].Vendor != "nvidia" || got[1].VRAMMB != 81920 {
		t.Fatalf("%+v", got)
	}
}

func TestSysfsGPUs(t *testing.T) {
	root := t.TempDir()
	card := func(n, vendor, vram, name string) {
		dev := filepath.Join(root, "card"+n, "device")
		os.MkdirAll(dev, 0o755)
		os.WriteFile(filepath.Join(dev, "vendor"), []byte(vendor+"\n"), 0o644)
		if vram != "" {
			os.WriteFile(filepath.Join(dev, "mem_info_vram_total"), []byte(vram+"\n"), 0o644)
		}
		if name != "" {
			os.WriteFile(filepath.Join(dev, "product_name"), []byte(name+"\n"), 0o644)
		}
	}
	card("1", "0x1002", "8539602944", "Radeon RX 9060 XT") // discrete, 8 GB
	card("2", "0x1002", "536870912", "Raphael")            // the CPU's iGPU, 512 MB
	card("3", "0x10de", "", "")                            // NVIDIA: nvidia-smi already reported it
	card("4", "0x1af4", "", "")                            // virtio: not a GPU to report
	got := sysfsGPUs(context.Background(), root, true)
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	if got[0].Vendor != "amd" || got[0].Model != "Radeon RX 9060 XT" || got[0].VRAMMB != 8144 || got[0].Integrated {
		t.Fatalf("discrete: %+v", got[0])
	}
	if !got[1].Integrated || got[1].VRAMMB != 512 {
		t.Fatalf("igpu: %+v", got[1])
	}
	if got := sysfsGPUs(context.Background(), root, false); len(got) != 3 {
		t.Fatalf("without nvidia-smi the NVIDIA card is listed: %+v", got)
	}
}

func TestParseIORegCores(t *testing.T) {
	if n := parseIORegCores(`      "model" = "Apple M4"` + "\n" + `      "gpu-core-count" = 10`); n != 10 {
		t.Fatalf("cores = %d", n)
	}
}

func TestDeclaredProbes(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sdk", "platform-tools"), 0o755)
	os.WriteFile(filepath.Join(dir, "sdk", "platform-tools", "adb"), nil, 0o755)
	f := protocol.MachineFacts{}
	runDeclaredProbes(context.Background(), []config.ProbeConfig{
		{Name: "android-sdk", Path: filepath.Join(dir, "sdk", "*", "adb")},
		{Name: "missing-sdk", Path: filepath.Join(dir, "nope")},
		{Name: "gitver", Command: []string{"git", "--version"}, VersionRegex: `git version (\d+)\.`},
		{Name: "gitany", Command: []string{"git", "--version"}},
		{Name: "nobin", Command: []string{"definitely-not-a-binary-xyz"}},
	}, &f)
	if f.Tools["android-sdk"] != "installed" || f.Tools["missing-sdk"] != "" || f.Tools["nobin"] != "" {
		t.Fatalf("tools: %v", f.Tools)
	}
	if v := f.Tools["gitver"]; v == "" || v == "installed" || len(v) > 3 {
		t.Fatalf("regex group: %q", v)
	}
	if v := f.Tools["gitany"]; v == "" || v == "installed" {
		t.Fatalf("default version: %q", v)
	}
}

// TestLiveGPUs prints this machine's GPUs (SYRE_LIVE_SCAN=1).
func TestLiveGPUs(t *testing.T) {
	if os.Getenv("SYRE_LIVE_SCAN") == "" {
		t.Skip("set SYRE_LIVE_SCAN=1")
	}
	for _, g := range detectGPUs(context.Background(), 16384) {
		t.Logf("%+v", g)
	}
}
