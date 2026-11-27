//go:build windows

package worker

import "testing"

func TestWindowsIsVM(t *testing.T) {
	for bios, want := range map[string]bool{
		"Microsoft Corporation Virtual Machine Hyper-V": true,
		"VMware, Inc. VMware20,1":                       true,
		"QEMU Standard PC (Q35 + ICH9, 2009) SeaBIOS":   true,
		"Amazon EC2 t3.large":                           true,
		"Vultr VC2":                                     true,
		"Micro-Star International Co., Ltd. MS-7C02":    false,
		"Microsoft Corporation Surface Laptop 5":        false,
		"":                                              false,
	} {
		if got := windowsIsVM(bios); got != want {
			t.Errorf("windowsIsVM(%q) = %v, want %v", bios, got, want)
		}
	}
}
