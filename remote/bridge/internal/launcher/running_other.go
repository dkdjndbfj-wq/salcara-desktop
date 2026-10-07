//go:build !windows

package launcher

import "context"

func runningDesktopTools(context.Context) map[string]Tool { return map[string]Tool{} }
