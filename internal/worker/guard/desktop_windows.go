//go:build windows

package guard

import "golang.org/x/sys/windows"

// verNTWorkstation is OSVERSIONINFOEX.wProductType for client editions.
const verNTWorkstation = 1

// DetectDesktop treats client editions as desktops and Server editions as headless.
func DetectDesktop() bool {
	v := windows.RtlGetVersion()
	return v == nil || v.ProductType == verNTWorkstation
}
