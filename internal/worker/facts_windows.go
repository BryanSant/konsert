//go:build windows

package worker

import (
	"context"
	"os/exec"
	"strings"
	"unsafe"

	"github.com/BryanSant/syre/internal/protocol"
	"golang.org/x/sys/windows/registry"
)

func collectPlatformFacts(ctx context.Context, f *protocol.MachineFacts) {
	f.OSName = "Windows"
	if out, err := exec.CommandContext(ctx, "cmd", "/C", "ver").Output(); err == nil {
		f.OSVersion = strings.TrimSpace(string(out))
	}
	ms := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms))); r != 0 {
		f.RAMTotalMB = ms.TotalPhys >> 20
	}
	f.IsVM = windowsIsVM(biosStrings())
}

// biosStrings reads the SMBIOS manufacturer and product the firmware reports.
func biosStrings() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\BIOS`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	var parts []string
	for _, name := range []string{"SystemManufacturer", "SystemProductName", "BIOSVendor"} {
		if v, _, err := k.GetStringValue(name); err == nil {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, " ")
}

// windowsIsVM matches the firmware strings of common hypervisors and clouds. The CPUID
// hypervisor bit is not used: it is also set on bare-metal hosts running Hyper-V/VBS.
func windowsIsVM(bios string) bool {
	low := strings.ToLower(bios)
	for _, sig := range []string{
		"virtual machine", "vmware", "virtualbox", "innotek", "qemu", "kvm", "xen",
		"amazon ec2", "google compute engine", "parallels", "bochs", "openstack", "vultr",
		"digitalocean", "hetzner", "bhyve",
	} {
		if strings.Contains(low, sig) {
			return true
		}
	}
	return false
}
