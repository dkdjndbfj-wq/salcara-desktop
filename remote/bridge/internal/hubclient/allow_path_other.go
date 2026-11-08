//go:build !windows

package hubclient

func resolveCwdPath(path string) (string, error) { return resolve(path) }
