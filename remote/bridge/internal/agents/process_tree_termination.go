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
type processTreeTermination struct{ state atomic.Uint32 }

func (s *processTreeTermination) begin() { s.state.Store(1) }

func (s *processTreeTermination) finish(err error) {
	if err != nil {
		s.state.Store(2)
		return
	}
	s.state.Store(0)
}

func (s *processTreeTermination) unconfirmed() bool { return s.state.Load() != 0 }
