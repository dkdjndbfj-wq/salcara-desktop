//go:build darwin

package agents

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func deepSeekCurrentBoot() deepSeekBootProof {
	return deepSeekDarwinBoot(func(name string, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, name, args...).Output()
	})
}

func deepSeekDarwinBoot(run func(string, ...string) ([]byte, error)) deepSeekBootProof {
	return deepSeekBootFromReaders("darwin", func() (string, error) {
		data, err := run("/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice")
		if err != nil {
			return "", err
		}
		var found string
		seen := false
		for _, line := range strings.Split(string(data), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if !ok || strings.TrimSpace(key) != `"IOPlatformUUID"` {
				continue
			}
			if seen {
				return "", errors.New("ambiguous macOS machine identity")
			}
			seen = true
			found, err = strconv.Unquote(strings.TrimSpace(value))
			if err != nil {
				return "", err
			}
		}
		return found, nil
	}, func() (string, error) {
		// kern.boottime changes with wall-clock corrections and is not proof.
		data, err := run("/usr/sbin/sysctl", "-n", "kern.bootsessionuuid")
		return string(data), err
	})
}
