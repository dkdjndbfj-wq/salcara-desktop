//go:build !windows && !linux

package hubclient

import "os"

// No guessed path-based fallback on systems without a verified final-handle
// implementation. Normal remote conversation remains available.
func workspaceVerifyHandle(_, _ *os.File, _, _ string, _ bool) error { return errWorkspaceFiles }

func workspaceRootReadFile(_ *os.Root, _ string) (*os.File, error) { return nil, errWorkspaceFiles }

func workspaceLocalRoot(_ string) bool { return false }
