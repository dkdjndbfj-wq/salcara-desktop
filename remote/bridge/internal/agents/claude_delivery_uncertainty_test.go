package agents

import (
	"context"
	"errors"
	"io"
	"testing"

	"salcara/bridge/internal/protocol"
)

type claudeDeliveryFixtureWriter struct {
	write func([]byte) (int, error)
}

func (w *claudeDeliveryFixtureWriter) Write(data []byte) (int, error) { return w.write(data) }
func (*claudeDeliveryFixtureWriter) Close() error                     { return nil }

func TestClaudeWriteDeliveryUncertaintySurvivesSend(t *testing.T) {
	for _, stage := range []string{"partial error", "short write", "write error", "stopped after write"} {
		t.Run(stage, func(t *testing.T) {
			a := newClaudeAgent(func(protocol.Event) {}, func() Settings { return Settings{ClaudeRoot: "http://127.0.0.1:1", ClaudeKey: "synthetic-api-key"} }, "not-launched", t.TempDir())
			t.Cleanup(a.Close)
			p := claudeSyntheticPermissionWorker(t, a, "fixture-session")
			p.workerSettingsSig = claudeSettingsSig(a.settings(), a.exeOverride)
			p.stdin = &claudeDeliveryFixtureWriter{write: func(data []byte) (int, error) {
				switch stage {
				case "partial error":
					return 1, io.ErrClosedPipe
				case "short write":
					return len(data) - 1, nil
				case "write error":
					return 0, io.ErrClosedPipe
				default:
					p.mu.Lock()
					p.stopped = true
					p.mu.Unlock()
					return len(data), nil
				}
			}}
			err := a.Send(context.Background(), p.id, "synthetic input")
			if protocol.ExecutionErrorCode(err) != "command_delivery_uncertain" || !errors.Is(err, errClaudeDeliveryInterrupted) {
				t.Fatal("attempted Claude write lost delivery uncertainty through Send")
			}
			if (stage == "partial error" || stage == "write error") && !errors.Is(err, io.ErrClosedPipe) {
				t.Fatal("uncertainty discarded the underlying owned pipe error")
			}
		})
	}
}

func TestClaudeRetiredBeforeWriteRefusesWithoutUncertainty(t *testing.T) {
	a := newClaudeAgent(func(protocol.Event) {}, func() Settings { return Settings{ClaudeRoot: "http://127.0.0.1:1", ClaudeKey: "synthetic-api-key"} }, "not-launched", t.TempDir())
	t.Cleanup(a.Close)
	p := claudeSyntheticPermissionWorker(t, a, "fixture-session")
	p.workerSettingsSig = claudeSettingsSig(a.settings(), a.exeOverride)
	p.stdin = &claudeDeliveryFixtureWriter{write: func([]byte) (int, error) {
		t.Fatal("retired worker wrote a new user frame")
		return 0, nil
	}}
	a.stop(p)
	err := a.Send(context.Background(), p.id, "synthetic input")
	if err == nil || protocol.ExecutionErrorCode(err) == "command_delivery_uncertain" {
		t.Fatal("pre-write refusal was reported as an attempted uncertain delivery")
	}
}
