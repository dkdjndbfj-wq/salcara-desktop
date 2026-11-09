//go:build !windows && !linux && !darwin

package agents

// No proof on an unsupported platform: keep the persisted runtime fence.
func deepSeekCurrentBoot() deepSeekBootProof { return deepSeekBootProof{} }
