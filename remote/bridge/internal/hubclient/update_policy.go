package hubclient

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

// UpdatePolicy is supplied only by the authenticated desktop shell after it
// verifies the publisher's signed manifest. It is separate from the short
// installer lease: retrying or cancelling an installation never admits work.
type UpdatePolicy struct {
	Required                bool   `json:"required"`
	MinimumSupportedVersion string `json:"minimumSupportedVersion,omitempty"`
	TargetVersion           string `json:"targetVersion,omitempty"`
	Reason                  string `json:"reason,omitempty"`
}

var ErrUpdateRequired = errors.New("update_required: 当前版本已不再受支持，请先更新桌面软件；已有任务可以继续完成")

func releaseVersion(value string) ([3]uint64, bool) {
	var version [3]uint64
	parts := strings.Split(value, ".")
	if len(parts) != 3 || len(value) > 64 {
		return version, false
	}
	for i, part := range parts {
		if part == "" {
			return version, false
		}
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return version, false
			}
		}
		value, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return version, false
		}
		version[i] = value
	}
	return version, true
}

func versionBefore(a, b [3]uint64) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func (c *Client) SetUpdatePolicy(policy UpdatePolicy) error {
	invalid := errors.New("更新兼容策略无效")
	if !utf8.ValidString(policy.Reason) || utf8.RuneCountInString(policy.Reason) > 1000 {
		return invalid
	}
	if policy.MinimumSupportedVersion != "" || policy.Required {
		minimum, validMinimum := releaseVersion(policy.MinimumSupportedVersion)
		target, validTarget := releaseVersion(policy.TargetVersion)
		// Build metadata does not change the compatible release version.
		currentText := strings.TrimPrefix(strings.SplitN(strings.SplitN(c.o.Version, "+", 2)[0], "-", 2)[0], "v")
		current, validCurrent := releaseVersion(currentText)
		if !validMinimum || !validTarget || !validCurrent || versionBefore(target, minimum) || policy.Required != versionBefore(current, minimum) {
			return invalid
		}
	} else if policy.TargetVersion != "" {
		if _, ok := releaseVersion(policy.TargetVersion); !ok {
			return invalid
		}
	}
	// Publishing the policy is immediate, even while an admitted request is
	// running. The admission checks never run inside active-turn validators.
	c.updatePolicy.Store(&policy)
	return nil
}

func (c *Client) UpdatePolicy() UpdatePolicy {
	if policy := c.updatePolicy.Load(); policy != nil {
		return *policy
	}
	return UpdatePolicy{}
}

func (c *Client) CheckUpdateAdmission() error {
	if c.UpdatePolicy().Required {
		return ErrUpdateRequired
	}
	return nil
}

func (c *Client) restoreUpdatePolicy() {
	encoded := os.Getenv("SALCARA_UPDATE_POLICY")
	if encoded == "" {
		return
	}
	var policy UpdatePolicy
	if len(encoded) > 8192 || json.Unmarshal([]byte(encoded), &policy) != nil || c.SetUpdatePolicy(policy) != nil {
		c.log.Printf("update: invalid desktop startup policy ignored")
	}
}

// New turns and task creation are gated; history, stopping, approvals and
// steering an already-running turn remain available so work can finish.
func startsUpdateBlockedWork(cmd map[string]any) bool {
	switch str(cmd, "type") {
	case "session.start", "session.send", "session.continue", "desktop.session.send", "session.compact", cooperationPrepareCommand,
		"session.side-question.ask", "session.side-question.send", "workspace.terminal.open":
		return true
	case "session.workflow":
		return str(cmd, "kind") != "" && str(cmd, "kind") != "goal.pause" && str(cmd, "kind") != "goal.clear"
	case "session.action":
		return str(cmd, "action") == "fork"
	case "session.side-question.tools":
		return str(cmd, "action") == "send"
	case "workspace.project.preview":
		return str(cmd, "action") == "start"
	}
	return false
}

func (c *Client) checkUpdateCommand(cmd map[string]any) error {
	if startsUpdateBlockedWork(cmd) {
		return c.CheckUpdateAdmission()
	}
	return nil
}
