//go:build linux

package agents

import (
	"errors"
	"io"
	"os"
)

func deepSeekCurrentBoot() deepSeekBootProof {
	return deepSeekBootFromReaders("linux", func() (string, error) {
		return deepSeekReadBootIDFile("/etc/machine-id")
	}, func() (string, error) {
		return deepSeekReadBootIDFile("/proc/sys/kernel/random/boot_id")
	})
}

func deepSeekReadBootIDFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 129))
	if err != nil {
		return "", err
	}
	if len(data) > 128 {
		return "", errors.New("oversized Linux boot identity")
	}
	return string(data), nil
}
