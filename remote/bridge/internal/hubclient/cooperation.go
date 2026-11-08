package hubclient

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/desktopcompanion"
	"salcara/bridge/internal/protocol"
)

const cooperationPrepareCommand = "session.cooperation.prepare"

var errCooperationStale = errors.New("cooperation_stale: 来源、目标或连接已变化，请重新选择任务")

// Protected by taskConfigMu. Tickets never survive a Bridge restart. A ticket
// is one explicit user submission, not a reusable model-to-model send grant.
type cooperationTicket struct {
	Scope, Source, Target string
	Agent                 agents.Agent
	Expires               time.Time
	Used                  bool
}

func validateCooperationFields(cmd map[string]any) error {
	_, ticket := cmd["cooperationTicket"]
	_, source := cmd["sourceSessionKey"]
	if _, forged := cmd["messageOrigin"]; forged {
		return errors.New("cooperation_invalid: 消息来源只能由电脑生成")
	}
	if !ticket && !source && str(cmd, "type") != cooperationPrepareCommand {
		return nil
	}
	if str(cmd, "type") != cooperationPrepareCommand && str(cmd, "type") != "session.send" {
		return errors.New("cooperation_invalid: 此操作不支持跨任务发送")
	}
	if str(cmd, "controlSurface") != "cli" || !protocol.ValidCooperationSessionKey(str(cmd, "sourceSessionKey")) || !protocol.ValidCooperationSessionKey(str(cmd, "sessionKey")) || str(cmd, "sourceSessionKey") == str(cmd, "sessionKey") {
		return errCooperationStale
	}
	if str(cmd, "type") == "session.send" && (!ticket || len(str(cmd, "cooperationTicket")) != 64 || !desktopcompanion.ValidNativeOperationID(str(cmd, "operationId")) || len(str(cmd, "text")) > 64<<10) {
		return errors.New("cooperation_invalid: 跨任务消息缺少有效票据或请求编号")
	}
	return nil
}

func cooperationSession(info protocol.SessionInfo, key, agent string, target bool) bool {
	if info.SessionKey != key || info.Tool != agent || info.Archived || !info.Controllable || info.ControlSurface != "" && info.ControlSurface != "cli" || info.ParentSessionKey != "" {
		return false
	}
	// Only an idle target may receive a new turn; never queue behind a running
	// peer or reinterpret a new user message as steer/resume authority.
	return !target || info.Status == "idle" || info.Status == "failed"
}

func (c *Client) cooperationScope(ctx context.Context, cfg config.Config, a agents.Agent, cmd map[string]any) string {
	return strconv.FormatUint(c.cooperationEpoch.Load(), 10) + "\x00" + cfg.DeviceID + "\x00" + featureCatalogScope(ctx, cfg, a, str(cmd, "accountId"), str(cmd, "sourceSessionKey"), str(cmd, "sessionKey"))
}

func (c *Client) cooperationPair(ctx context.Context, cfg config.Config, cmd map[string]any) (agents.Agent, protocol.SessionInfo, protocol.SessionInfo, error) {
	var source, target protocol.SessionInfo
	a, id, err := agentFor(c.manager(), str(cmd, "sessionKey"))
	if err != nil {
		return nil, source, target, err
	}
	if a.ID() != "codex" && a.ID() != "claude" || !strings.HasPrefix(str(cmd, "sourceSessionKey"), a.ID()+":") {
		return nil, source, target, errors.New("cooperation_unavailable: 目前只支持同一 Agent 内的受管理 CLI 任务")
	}
	if _, ok := a.(turnOptionsSender); !ok {
		return nil, source, target, errCooperationStale
	}
	if err := validateAdapterCommand(a, cmd); err != nil {
		return nil, source, target, err
	}
	if err := c.validateExecutionAccountFor(ctx, cfg, a.ID(), str(cmd, "sourceSessionKey"), str(cmd, "accountId")); err != nil {
		return nil, source, target, err
	}
	if err := requireAgentCapability(a, "send"); err != nil {
		return nil, source, target, err
	}
	source, _, err = a.Open(ctx, strings.TrimPrefix(str(cmd, "sourceSessionKey"), a.ID()+":"))
	if err != nil {
		return nil, source, target, err
	}
	target, _, err = a.Open(ctx, id)
	if err != nil {
		return nil, source, target, err
	}
	if !cooperationSession(source, str(cmd, "sourceSessionKey"), a.ID(), false) || !cooperationSession(target, str(cmd, "sessionKey"), a.ID(), true) {
		return nil, source, target, errCooperationStale
	}
	source.ControlSurface, target.ControlSurface = "cli", "cli"
	return a, source, target, nil
}

func (c *Client) prepareCooperation(ctx context.Context, cfg config.Config, cmd map[string]any) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a, source, target, err := c.cooperationPair(ctx, cfg, cmd)
	if err != nil {
		return nil, err
	}
	if err := c.validateCommandGuard(ctx, cooperationPrepareCommand); err != nil {
		return nil, err
	}
	current := cfg
	if c.o.Store != nil {
		current = executionConfig(ctx, c.o.Store.Get())
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if env, ok := executionSourceFromContext(ctx); ok {
		if err := c.validateExecutionSourceFor(env, current); err != nil {
			return nil, err
		}
	}
	scope := c.cooperationScope(ctx, cfg, a, cmd)
	manager := c.manager()
	if manager == nil || manager.Get(a.ID()) != a || c.cooperationScope(ctx, current, a, cmd) != scope {
		return nil, errCooperationStale
	}
	if c.cooperationTickets == nil {
		c.cooperationTickets = map[string]*cooperationTicket{}
	}
	for key, ticket := range c.cooperationTickets {
		if time.Now().After(ticket.Expires) {
			delete(c.cooperationTickets, key)
		}
	}
	if len(c.cooperationTickets) >= 64 {
		return nil, errors.New("cooperation_busy: 待发送的跨任务请求过多，请稍后重试")
	}
	token, expires := config.RandomToken(32), time.Now().Add(5*time.Minute)
	c.cooperationTickets[token] = &cooperationTicket{Scope: scope, Source: source.SessionKey, Target: target.SessionKey, Agent: a, Expires: expires}
	return protocol.CooperationPreparation{Ticket: token, ExpiresAt: expires.UnixMilli(), Source: source, Target: target}, nil
}

func (c *Client) admitCooperation(ctx context.Context, cfg config.Config, cmd map[string]any, opts *agents.TurnOptions) error {
	if str(cmd, "cooperationTicket") == "" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ticket := c.cooperationTickets[str(cmd, "cooperationTicket")]
	if ticket == nil || ticket.Used || time.Now().After(ticket.Expires) || ticket.Source != str(cmd, "sourceSessionKey") || ticket.Target != str(cmd, "sessionKey") {
		return errCooperationStale
	}
	a, source, _, err := c.cooperationPair(ctx, cfg, cmd)
	if err != nil {
		return err
	}
	if a != ticket.Agent || ticket.Scope != c.cooperationScope(ctx, cfg, a, cmd) {
		return errCooperationStale
	}
	title := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, source.Title)
	runes := []rune(title)
	if len(runes) > 160 {
		title = string(runes[:160])
	}
	opts.CrossTaskOrigin = &protocol.MessageOrigin{Kind: "user_cross_task", Verification: "bridge_receipt", SourceSessionKey: ticket.Source, SourceTitle: title, TargetSessionKey: ticket.Target, ReceiptID: str(cmd, "operationId")}
	permission := opts.ValidatePermission
	opts.ValidatePermission = func() error {
		if permission != nil {
			if err := permission(); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.validateCommandGuard(ctx, "session.send"); err != nil {
			return err
		}
		current := cfg
		if c.o.Store != nil {
			current = executionConfig(ctx, c.o.Store.Get())
		}
		manager := c.manager()
		if time.Now().After(ticket.Expires) || ticket.Scope != c.cooperationScope(ctx, current, a, cmd) || manager == nil || manager.Get(a.ID()) != a {
			return errCooperationStale
		}
		sourceNow, _, err := a.Open(ctx, strings.TrimPrefix(ticket.Source, a.ID()+":"))
		if err != nil || !cooperationSession(sourceNow, ticket.Source, a.ID(), false) {
			return errCooperationStale
		}
		if c.o.Store != nil {
			current = executionConfig(ctx, c.o.Store.Get())
		}
		if time.Now().After(ticket.Expires) || ticket.Scope != c.cooperationScope(ctx, current, a, cmd) {
			return errCooperationStale
		}
		if env, ok := executionSourceFromContext(ctx); ok {
			return c.validateExecutionSourceFor(env, current)
		}
		return nil
	}
	// Consume before native admission; ambiguous native failures cannot be
	// replayed as a fresh turn. Hub operation receipts resolve transport retries.
	ticket.Used = true
	return nil
}

// Monotonic epoch prevents an A→B→A source switch from reviving old tickets.
// Ordinary unrelated preference/usage writes do not invalidate a preparation.
func (c *Client) watchCooperationSource() {
	if c.o.Store == nil {
		return
	}
	c.o.Store.OnChange(func(old, next config.Config) {
		if old.DeviceID != next.DeviceID || config.RemoteIdentity(old) != config.RemoteIdentity(next) || old.PhoneBindingID != next.PhoneBindingID || old.PhoneHash != next.PhoneHash || !sameCooperationPolicy(old.RemoteStationObservation, next.RemoteStationObservation) {
			c.cooperationEpoch.Add(1)
			return
		}
		m := c.manager()
		if m == nil {
			c.cooperationEpoch.Add(1)
			return
		}
		for _, id := range []string{"codex", "claude"} {
			if a := m.Get(id); a != nil && featureCatalogScope(context.Background(), old, a, "", "", "") != featureCatalogScope(context.Background(), next, a, "", "", "") {
				c.cooperationEpoch.Add(1)
				return
			}
		}
	})
}

func sameCooperationPolicy(a, b *config.StationObservation) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Mode == b.Mode && a.Revision == b.Revision && config.StationPolicyEqual(a.Station, b.Station)
}
