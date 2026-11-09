//go:build !windows && !linux

package hubclient

import (
	"os"
	"salcara/bridge/internal/protocol"
)

func explorerRoots() ([]protocol.Project, error) { return nil, errExplorerLocation }
func explorerLocalPath(string) bool              { return false }
func explorerIsLink(info os.FileInfo) bool       { return true }
