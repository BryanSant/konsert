package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/drivers"
)

// GPU detection (SYR-M7F13): NVIDIA through nvidia-smi (Linux, WSL, Windows), AMD and
// Intel through sysfs on Linux (the model from lspci), Apple silicon's unified GPU
// through sysctl and ioreg. Each source is cheap and bounded; a missing one adds
// nothing.

// integratedVRAMMB is the dedicated memory below which an AMD or Intel GPU counts as a
// small integrated one.
const integratedVRAMMB = 2048

// collectGPUs is replaced by tests.
var collectGPUs = detectGPUs

func detectGPUs(ctx context.Context, ramTotalMB uint64) []protocol.GPU {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	gpus := nvidiaGPUs(ctx)
	switch runtime.GOOS {
	case "linux":
		gpus = append(gpus, sysfsGPUs(ctx, "/sys/class/drm", len(gpus) > 0)...)
	case "darwin":
		if runtime.GOARCH == "arm64" {
			gpus = append(gpus, appleGPU(ctx, ramTotalMB)...)
		}
	}
	return gpus
}

// nvidiaGPUs asks nvidia-smi (on PATH, or WSL's /usr/lib/wsl/lib).
func nvidiaGPUs(ctx context.Context) []protocol.GPU {
	bin, ok := drivers.FindBinary("nvidia-smi")
	if !ok {
		if _, err := os.Stat("/usr/lib/wsl/lib/nvidia-smi"); err != nil {
			return nil
		}
		bin = "/usr/lib/wsl/lib/nvidia-smi"
	}
	out, err := exec.CommandContext(ctx, bin, "--query-gpu=name,memory.total", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	return parseNvidiaSMI(string(out))
}

func parseNvidiaSMI(out string) []protocol.GPU {
	var gpus []protocol.GPU
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		name, mem, ok := strings.Cut(line, ",")
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		g := protocol.GPU{Vendor: "nvidia", Model: strings.TrimSpace(name)}
		if v, err := strconv.ParseUint(strings.TrimSpace(mem), 10, 64); err == nil {
			g.VRAMMB = v
		}
		gpus = append(gpus, g)
	}
	return gpus
}

var lspciNameRe = regexp.MustCompile(`\[([^\]]+)\]\s*$`)

// sysfsGPUs reads /sys/class/drm/card*/device: AMD (0x1002) and Intel (0x8086), and
// NVIDIA (0x10de) only when nvidia-smi found none.
func sysfsGPUs(ctx context.Context, root string, haveNvidia bool) []protocol.GPU {
	cards, _ := filepath.Glob(filepath.Join(root, "card[0-9]*", "device"))
	var gpus []protocol.GPU
	seen := map[string]bool{}
	for _, dev := range cards {
		real, err := filepath.EvalSymlinks(dev)
		if err != nil || seen[real] {
			continue
		}
		seen[real] = true
		vendorID := readTrim(filepath.Join(dev, "vendor"))
		var g protocol.GPU
		switch vendorID {
		case "0x1002":
			g.Vendor = "amd"
		case "0x8086":
			g.Vendor = "intel"
		case "0x10de":
			if haveNvidia {
				continue
			}
			g.Vendor = "nvidia"
		default:
			continue
		}
		if v, err := strconv.ParseUint(readTrim(filepath.Join(dev, "mem_info_vram_total")), 10, 64); err == nil {
			g.VRAMMB = v >> 20
		}
		g.Model = readTrim(filepath.Join(dev, "product_name"))
		if g.Model == "" {
			g.Model = lspciModel(ctx, filepath.Base(real))
		}
		if g.Vendor != "nvidia" && g.VRAMMB < integratedVRAMMB {
			g.Integrated = true
		}
		gpus = append(gpus, g)
	}
	return gpus
}

// lspciModel is the device name lspci gives a PCI slot ("Radeon RX 9050 / 9060 XT").
func lspciModel(ctx context.Context, slot string) string {
	bin, ok := drivers.FindBinary("lspci")
	if !ok || slot == "" {
		return ""
	}
	out, err := exec.CommandContext(ctx, bin, "-mm", "-s", slot).Output()
	if err != nil {
		return ""
	}
	// 03:00.0 "VGA compatible controller" "Advanced Micro Devices, Inc. [AMD/ATI]" "Navi 44 [Radeon RX 9050 / 9060 XT]" …
	fields := strings.Split(string(out), "\"")
	if len(fields) < 6 {
		return ""
	}
	name := strings.TrimSpace(fields[5])
	if m := lspciNameRe.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return name
}

// appleGPU is Apple silicon's GPU: the chip's name, its core count, unified memory.
func appleGPU(ctx context.Context, ramTotalMB uint64) []protocol.GPU {
	model := ""
	if out, err := exec.CommandContext(ctx, "sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
		model = strings.TrimSpace(string(out))
	}
	g := protocol.GPU{Vendor: "apple", Model: model, VRAMMB: ramTotalMB, Unified: true}
	if out, err := exec.CommandContext(ctx, "ioreg", "-rc", "AGXAccelerator", "-d1").Output(); err == nil {
		g.Cores = parseIORegCores(string(out))
	}
	return []protocol.GPU{g}
}

var ioregCoresRe = regexp.MustCompile(`"gpu-core-count"\s*=\s*(\d+)`)

func parseIORegCores(out string) int {
	if m := ioregCoresRe.FindStringSubmatch(out); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
