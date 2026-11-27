//go:build linux

package worker

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/guard"
)

func collectPlatformFacts(ctx context.Context, f *protocol.MachineFacts) {
	f.OSName = "Linux"

	// /etc/os-release
	if raw, err := os.ReadFile("/etc/os-release"); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(raw))
		for sc.Scan() {
			k, v, ok := strings.Cut(sc.Text(), "=")
			if !ok {
				continue
			}
			v = strings.Trim(v, `"`)
			switch k {
			case "NAME":
				f.OSName = v
			case "VERSION_ID":
				f.OSVersion = v
			}
		}
	}

	// Kernel version & WSL detection
	if raw, err := os.ReadFile("/proc/version"); err == nil {
		s := strings.TrimSpace(string(raw))
		fields := strings.Fields(s)
		if len(fields) >= 3 {
			f.KernelVersion = fields[2]
		}
		low := strings.ToLower(s)
		if strings.Contains(low, "microsoft") || strings.Contains(low, "wsl") {
			f.IsWSL = true
		}
	}
	if os.Getenv("WSL_DISTRO_NAME") != "" || os.Getenv("WSL_INTEROP") != "" {
		f.IsWSL = true
	}

	// CPU model
	if raw, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(raw))
		for sc.Scan() {
			k, v, ok := strings.Cut(sc.Text(), ":")
			if !ok {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(k))
			if key == "model name" || key == "hardware" {
				f.CPUModel = strings.TrimSpace(v)
				break
			}
		}
	}

	// RAM & Swap
	if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
		var memTotalKB, swapTotalKB, swapFreeKB uint64
		sc := bufio.NewScanner(bytes.NewReader(raw))
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 2 {
				continue
			}
			v, _ := strconv.ParseUint(fields[1], 10, 64)
			switch fields[0] {
			case "MemTotal:":
				memTotalKB = v
			case "SwapTotal:":
				swapTotalKB = v
			case "SwapFree:":
				swapFreeKB = v
			}
		}
		f.RAMTotalMB = memTotalKB / 1024
		// Inside a container the workers get its memory limit, not the host's RAM.
		if l := guard.CgroupMemoryLimit() >> 20; l > 0 && l < f.RAMTotalMB {
			f.RAMTotalMB = l
		}
		f.SwapTotalMB = swapTotalKB / 1024
		if swapTotalKB >= swapFreeKB {
			f.SwapUsedMB = (swapTotalKB - swapFreeKB) / 1024
		}
	}

	// Disk space on /
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err == nil {
		bsize := uint64(st.Bsize)
		f.DiskTotalGB = (st.Blocks * bsize) / (1024 * 1024 * 1024)
		f.DiskFreeGB = (st.Bavail * bsize) / (1024 * 1024 * 1024)
	}

	// Check if /tmp is tmpfs via /proc/mounts
	if raw, err := os.ReadFile("/proc/mounts"); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(raw))
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) >= 3 && fields[1] == "/tmp" && fields[2] == "tmpfs" {
				f.TmpIsTmpfs = true
				break
			}
		}
	}

	// Storage type via /sys/block
	if entries, err := os.ReadDir("/sys/block"); err == nil {
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "zram") {
				continue
			}
			if strings.HasPrefix(name, "nvme") {
				f.StorageType = "nvme"
				break
			}
			if raw, err := os.ReadFile(filepath.Join("/sys/block", name, "queue", "rotational")); err == nil {
				if strings.TrimSpace(string(raw)) == "0" {
					f.StorageType = "ssd"
				} else if f.StorageType == "unknown" {
					f.StorageType = "hdd"
				}
			}
		}
	}

	// Container / Mac-container / VM detection
	if _, err := os.Stat("/.dockerenv"); err == nil {
		f.IsContainer = true
	}
	if _, err := os.Stat("/run/.containerenv"); err == nil {
		f.IsContainer = true
	}
	if os.Getenv("container") != "" {
		f.IsContainer = true
	}
	if raw, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		low := strings.ToLower(string(raw))
		if strings.Contains(low, "docker") || strings.Contains(low, "podman") || strings.Contains(low, "kubepods") || strings.Contains(low, "lxc") {
			f.IsContainer = true
		}
	}
	if dmi, err := os.ReadFile("/sys/class/dmi/id/product_name"); err == nil {
		low := strings.ToLower(string(dmi))
		if strings.Contains(low, "apple virtualization") {
			f.IsMacContainer = true
			f.IsVM = true
		} else if strings.Contains(low, "kvm") || strings.Contains(low, "qemu") || strings.Contains(low, "vmware") || strings.Contains(low, "virtualbox") || strings.Contains(low, "hyper-v") {
			f.IsVM = true
		}
	}
	if out, err := exec.CommandContext(ctx, "systemd-detect-virt").Output(); err == nil {
		v := strings.TrimSpace(strings.ToLower(string(out)))
		switch v {
		case "", "none":
		case "docker", "podman", "lxc", "container-other":
			f.IsContainer = true
		case "wsl":
			f.IsWSL = true
		case "apple":
			f.IsMacContainer = true
			f.IsVM = true
		default:
			f.IsVM = true
		}
	}
}
