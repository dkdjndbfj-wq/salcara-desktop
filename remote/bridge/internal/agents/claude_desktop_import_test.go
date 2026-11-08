package agents

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestClaudeDesktopImportClaimAtMostOnce(t *testing.T) {
	a := &claudeAgent{fromPhone: map[string]bool{"phone-created": true}}
	var claims atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if a.ClaimDesktopImport("phone-created") {
				claims.Add(1)
			}
		}()
	}
	wg.Wait()
	if claims.Load() != 1 || a.ClaimDesktopImport("phone-created") || a.ClaimDesktopImport("external") {
		t.Fatal("desktop import was duplicated or external session was claimed")
	}
}

func TestClaudeDesktopImportClaimDoesNotReappearAfterCloseOrRestart(t *testing.T) {
	a := &claudeAgent{closed: true, fromPhone: map[string]bool{"phone-created": true}}
	if a.ClaimDesktopImport("phone-created") {
		t.Fatal("closing worker allowed import")
	}
	restarted := &claudeAgent{}
	if restarted.ClaimDesktopImport("phone-created") {
		t.Fatal("unproven import survived restart")
	}
}
