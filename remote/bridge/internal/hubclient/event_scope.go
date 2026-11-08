package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"salcara/bridge/internal/config"
)

type eventUploadScope struct {
	identity, scope string
	required        bool
}
type eventScopeContextKey struct{}

func botEventScopeRequired(cfg config.Config) bool {
	group, _, _ := cfg.BotEnrollment()
	return group != ""
}
func (c *Client) eventScopeRequired(cfg config.Config) bool {
	value := c.eventScope.Load()
	return botEventScopeRequired(cfg) || value != nil && value.identity == eventIdentity(cfg) && value.required
}
func (c *Client) eventScopeFromContext(ctx context.Context, cfg config.Config) string {
	if scope, frozen := ctx.Value(eventScopeContextKey{}).(string); frozen {
		return scope
	}
	if guard, remote := commandGuardFromContext(ctx); remote {
		return guard.eventScope
	}
	return c.eventScopeFor(cfg)
}
func (c *Client) eventScopeFor(cfg config.Config) string {
	value := c.eventScope.Load()
	if value != nil && value.identity == eventIdentity(cfg) {
		return value.scope
	}
	return ""
}

// A server scope is authority learned from the authenticated handshake. Disk
// restoration alone cannot prove the Bot group still belongs to that phone.
func (c *Client) acceptEventScope(cfg config.Config, scope string, serverRequired ...bool) error {
	c.qmu.Lock()
	defer c.qmu.Unlock()
	if eventIdentity(cfg) != eventIdentity(c.o.Store.Get()) {
		return errors.New("中转站或配对已改变")
	}
	required := c.eventScopeRequired(cfg) || len(serverRequired) != 0 && serverRequired[0]
	if scope == "" {
		if required {
			c.eventScope.Store(&eventUploadScope{identity: eventIdentity(cfg), required: true})
			return errors.New("此 Bot 中转站缺少事件代次，请更新远程服务")
		}
		return nil
	}
	if len(scope) > 256 || strings.ContainsAny(scope, "\r\n\x00") {
		return errors.New("中转站事件代次无效")
	}
	if previous := c.eventScope.Load(); previous != nil && previous.identity == eventIdentity(cfg) && previous.scope == scope && previous.required == required && c.queueEventScope == scope {
		return c.durableErr
	}
	if c.queueEventScope != scope && (c.queueEventScope != "" || required) {
		hadPrivate := len(c.queue) != 0 || len(c.recovery) != 0 || c.inFlightBatchID != "" || c.recoveryAll
		c.queue = nil
		c.queueBytes = 0
		c.inFlight = 0
		c.inFlightBatchID = ""
		c.inFlightPayload = nil
		c.inFlightRecovery = nil
		c.clearRecoveryLocked()
		if hadPrivate {
			c.recoveryGeneration++
			c.recoveryAll = true
			c.recoveryOverflow = c.recoveryGeneration
		}
		c.queueConfig = cfg
	}
	c.queueEventScope = scope
	// Publish before releasing old attachment pins. Both payload and file
	// namespaces now refer to this exact group generation.
	c.eventScope.Store(&eventUploadScope{identity: eventIdentity(cfg), scope: scope, required: required})
	c.syncOutboxAttachmentPinsLocked()
	if err := c.persistOutboxLocked(); err != nil {
		c.durableErr = err
		return err
	}
	return nil
}

func (c *Client) commandEventScopeValid(cfg config.Config, scope string) bool {
	if scope == "" {
		return !c.eventScopeRequired(cfg)
	}
	return scope == c.eventScopeFor(cfg)
}

func (c *Client) registerWithEventScope(ctx context.Context, cfg config.Config, path string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := c.requestFor(ctx, cfg, http.MethodPost, path, strings.NewReader(string(data)), true)
	if err != nil {
		return err
	}
	response, err := c.o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return checkResp(response)
	}
	var result struct {
		EventScope         string `json:"eventScope"`
		EventScopeRequired bool   `json:"eventScopeRequired"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 8192)).Decode(&result); err != nil && !errors.Is(err, io.EOF) {
		return errors.New("中转站登记回执无效")
	}
	return c.acceptEventScope(cfg, result.EventScope, result.EventScopeRequired)
}
