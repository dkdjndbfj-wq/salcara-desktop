//go:build !windows && !linux

package hubclient

import "os"

func projectlessDirectoryIdentity(*os.File) (string, error) { return "", errProjectless }
