package agents

import (
	"errors"
	"sync/atomic"
)

var errProcessTreeTerminationUnconfirmed = errors.New("进程树退出尚未确认")

// Tracks explicit cleanup only. The zero value is not proof that every
// naturally detached descendant exited. A failed attempt cannot be cleared by
// root Wait, transport retirement, or a timer: none identifies the old tree.
// This in-memory fence does not provide recovery across a Bridge restart.
type processTreeTermination struct {
	state  atomic.Uint32
	proved atomic.Bool
}

func (s *processTreeTermination) begin() { s.proved.Store(false); s.state.Store(1) }

func (s *processTreeTermination) finish(err error) {
	if err != nil {
		s.proved.Store(false)
		s.state.Store(2)
		return
	}
	s.proved.Store(true)
	s.state.Store(0)
}

// A zero-value/normal root exit is not an owned subtree termination receipt.
func (s *processTreeTermination) confirmed() bool { return s.proved.Load() && !s.unconfirmed() }

func (s *processTreeTermination) unconfirmed() bool { return s.state.Load() != 0 }
