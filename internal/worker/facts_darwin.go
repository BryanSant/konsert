//go:build darwin

package worker

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/BryanSant/konsert/internal/protocol"
)

func collectPlatformFacts(ctx context.Context, f *protocol.MachineFacts) {
	f.OSName = "macOS"
	if out, err := exec.CommandContext(ctx, "sw_vers", "-productVersion").Output(); err == nil {
		f.OSVersion = strings.TrimSpace(string(out))
	}
	if out, err := exec.CommandContext(ctx, "uname", "-r").Output(); err == nil {
		f.KernelVersion = "Darwin " + strings.TrimSpace(string(out))
	}
	if out, err := exec.CommandContext(ctx, "sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
		f.CPUModel = strings.TrimSpace(string(out))
	}
	if out, err := exec.CommandContext(ctx, "sysctl", "-n", "hw.memsize").Output(); err == nil {
		if b, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64); err == nil {
			f.RAMTotalMB = b / (1024 * 1024)
		}
	}
	// Swap usage: "total = 2048.00M  used = 512.00M  free = 1536.00M"
	if out, err := exec.CommandContext(ctx, "sysctl", "-n", "vm.swapusage").Output(); err == nil {
		parseDarwinSwap(string(out), f)
	}
	// Disk capacity on root filesystem
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err == nil {
		bsize := uint64(st.Bsize)
		f.DiskTotalGB = (st.Blocks * bsize) / (1024 * 1024 * 1024)
		f.DiskFreeGB = (st.Bavail * bsize) / (1024 * 1024 * 1024)
	}
	// macOS /tmp is symlinked to /private/tmp on APFS (not tmpfs unless custom ramdisk)
	if out, err := exec.CommandContext(ctx, "df", "-T", "tmpfs", "/tmp").CombinedOutput(); err == nil && strings.Contains(strings.ToLower(string(out)), "tmpfs") {
		f.TmpIsTmpfs = true
	}
	// Apple Silicon & modern Macs use NVMe/SSD; confirm via diskutil if available
	f.StorageType = "ssd"
	if out, err := exec.CommandContext(ctx, "diskutil", "info", "/").Output(); err == nil {
		low := strings.ToLower(string(out))
		if strings.Contains(low, "nvme") || strings.Contains(low, "apple fabric") {
			f.StorageType = "nvme"
		} else if strings.Contains(low, "solid state:               yes") || strings.Contains(low, "solid state: yes") {
			f.StorageType = "ssd"
		} else if strings.Contains(low, "rotational") {
			f.StorageType = "hdd"
		}
	}
	// VM / Apple Container detection
	if out, err := exec.CommandContext(ctx, "sysctl", "-n", "kern.hv_vmm_present").Output(); err == nil {
		if strings.TrimSpace(string(out)) == "1" {
			f.IsVM = true
		}
	}
	if os.Getenv("APPLE_CONTAINER") != "" {
		f.IsMacContainer = true
		f.IsContainer = true
	}
	if out, err := exec.CommandContext(ctx, "sysctl", "-n", "hw.model").Output(); err == nil {
		if strings.Contains(strings.ToLower(string(out)), "virtualmac") {
			f.IsVM = true
		}
	}
}

func parseDarwinSwap(raw string, f *protocol.MachineFacts) {
	fields := strings.Fields(raw)
	for i := 0; i < len(fields)-2; i++ {
		if fields[i] == "total" && fields[i+1] == "=" {
			f.SwapTotalMB = parseMBToken(fields[i+2])
		}
		if fields[i] == "used" && fields[i+1] == "=" {
			f.SwapUsedMB = parseMBToken(fields[i+2])
		}
	}
}

func parseMBToken(tok string) uint64 {
	tok = strings.TrimSpace(tok)
	mult := 1.0
	switch {
	case strings.HasSuffix(tok, "G"):
		mult = 1024
		tok = strings.TrimSuffix(tok, "G")
	case strings.HasSuffix(tok, "M"):
		tok = strings.TrimSuffix(tok, "M")
	case strings.HasSuffix(tok, "K"):
		mult = 1.0 / 1024.0
		tok = strings.TrimSuffix(tok, "K")
	}
	v, err := strconv.ParseFloat(tok, 64)
	if err != nil || v < 0 {
		return 0
	}
	return uint64(v * mult)
}
