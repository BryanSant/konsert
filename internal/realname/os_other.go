//go:build !linux && !darwin && !windows

package realname

import "context"

func osFullName(context.Context, string) string { return "" }
