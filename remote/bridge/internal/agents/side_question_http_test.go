package agents

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSideQuestionGatewayFailuresAreSpecificBoundedAndNeverReplayed(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"capacity-code", 503, `{"error":{"code":"model_capacity","message":"synthetic-secret"}}`, "所选模型容量已满"},
		{"capacity-legacy", 429, `{"error":{"message":"Selected model is at capacity. Please try a different model. synthetic-secret"}}`, "所选模型容量已满"},
		{"rate", 429, `{"error":{"code":"rate_limit","message":"synthetic-secret"}}`, "独立提问请求受限（HTTP 429）"},
		{"auth-before-prose", 401, `{"error":{"code":"model_capacity","message":"synthetic-secret"}}`, "独立提问未获授权（HTTP 401）"},
		{"permission", 403, `{"error":{"message":"synthetic-secret"}}`, "独立提问访问被拒绝（HTTP 403）"},
		{"upstream", 502, `{"error":{"message":"synthetic-secret https://private.invalid"}}`, "独立提问请求失败（HTTP 502）"},
		{"oversized-error", 503, `{"error":{"code":"model_capacity","message":"` + strings.Repeat("x", 64<<10) + `"}}`, "独立提问请求失败（HTTP 503）"},
		{"redirect", 307, `{"error":{"message":"synthetic-secret"}}`, "独立提问请求失败（HTTP 307）"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/gateway/remote-codex/v1/responses" || r.Header.Get("Authorization") != "Bearer fixture" {
					t.Error("request lost its exact gateway binding")
				}
				if test.status == 307 {
					w.Header().Set("Location", "/must-not-follow")
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			var registry sideQuestionRegistry
			ctx, key, snapshot := context.Background(), "codex:parent", sideStateSnapshot("error-reading")
			binding, guard := sideQuestionBinding{Source: "source"}, func() error { return nil }
			view, err := registry.catalog(ctx, key, snapshot, binding, guard)
			if err != nil || !view.Available {
				t.Fatalf("catalog: %+v %v", view, err)
			}
			request := sideFixtureRequest(view)
			run := func(ctx context.Context, _ sideQuestionInput) (sideQuestionAnswer, bool, error) {
				_, attempted, err := sideQuestionGatewayRequest(ctx, server.URL+"/gateway/remote-codex/v1/responses", "fixture", []byte(`{}`))
				return sideQuestionAnswer{}, attempted, err
			}
			for attempt := 0; attempt < 2; attempt++ {
				result, err := registry.ask(ctx, key, snapshot, request, binding, guard, run, nil)
				if err != nil || result.State != "unconfirmed" || result.Error != test.want || result.Answer != "" || result.Usage != nil || calls.Load() != 1 {
					t.Fatalf("attempt %d: %+v err=%v calls=%d", attempt, result, err, calls.Load())
				}
			}
			request.OperationID = newUUID()
			if _, err := registry.ask(ctx, key, snapshot, request, binding, guard, run, nil); !errors.Is(err, errSideQuestionUnknown) || calls.Load() != 1 || registry.activeTurns() {
				t.Fatalf("failed request was retried: %v calls=%d", err, calls.Load())
			}
		})
	}
}

func TestSideQuestionTransportErrorTextRemainsPrivate(t *testing.T) {
	var registry sideQuestionRegistry
	ctx, key, snapshot := context.Background(), "claude:parent", sideStateSnapshot("error-reading")
	binding, guard := sideQuestionBinding{Source: "source"}, func() error { return nil }
	view, _ := registry.catalog(ctx, key, snapshot, binding, guard)
	result, err := registry.ask(ctx, key, snapshot, sideFixtureRequest(view), binding, guard,
		func(context.Context, sideQuestionInput) (sideQuestionAnswer, bool, error) {
			return sideQuestionAnswer{}, true, errors.New("synthetic-secret https://private.invalid")
		}, nil)
	if err != nil || result.Error != "独立提问结果未确认，未自动重试" || result.State != "unconfirmed" {
		t.Fatalf("arbitrary transport text was exposed: %+v %v", result, err)
	}
}
