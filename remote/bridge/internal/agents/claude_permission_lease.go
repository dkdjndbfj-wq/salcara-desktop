package agents

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"sync"
)

// A local helper may outlive the CLI which launched it. Its private capability
// belongs to that worker, not the console token or a reusable session ID.
type claudePermissionLease struct {
	token string
	done  chan struct{}
	once  sync.Once
}

func newClaudePermissionLease() (*claudePermissionLease, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	return &claudePermissionLease{token: base64.RawURLEncoding.EncodeToString(secret), done: make(chan struct{})}, nil
}

func (lease *claudePermissionLease) revoke() {
	if lease != nil {
		lease.once.Do(func() { close(lease.done) })
	}
}

// Called with a.mu held. Startup owns a worker before publishing procs[id]; a
// replacement, retired worker or closed agent never inherits that capability.
func (a *claudeAgent) permissionWorkerActiveLocked(p *claudeProc) bool {
	if a.closed || p == nil || p.permissionLease == nil || !approvalSourceOpen(p.permissionLease.done) || p.transportRetired() {
		return false
	}
	// Retirement reserves stopped before potentially slow process/pipe cleanup.
	// Use the same a.mu -> p.mu order as turn admission, not just closed pipes.
	p.mu.Lock()
	stopped := p.stopped
	p.mu.Unlock()
	if stopped {
		return false
	}
	if current := a.procs[p.id]; current != nil {
		return current == p
	}
	_, owned := a.workers[p]
	return owned
}

func (a *claudeAgent) permissionWorkerActive(p *claudeProc) bool {
	a.mu.Lock()
	active := a.permissionWorkerActiveLocked(p)
	a.mu.Unlock()
	return active
}

func (a *claudeAgent) permissionWorkerForToken(id, token string) *claudeProc {
	a.mu.Lock()
	defer a.mu.Unlock()
	for p := range a.workers {
		if p.id == id && a.permissionWorkerActiveLocked(p) && subtle.ConstantTimeCompare([]byte(token), []byte(p.permissionLease.token)) == 1 {
			return p
		}
	}
	return nil
}
