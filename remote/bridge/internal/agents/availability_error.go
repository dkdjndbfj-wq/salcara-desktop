package agents

import "errors"

// ErrCodexNotInstalled distinguishes an absent optional adapter from an
// installed adapter whose session inventory failed. Other agents must not be
// blocked merely because this computer has never installed Codex.
var ErrCodexNotInstalled = errors.New("没有找到 Codex，请先安装 Codex CLI")
