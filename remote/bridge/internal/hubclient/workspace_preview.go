package hubclient

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"salcara/bridge/internal/config"
)

// Preview pages run on the paired computer in isolated temporary browser
// profiles. Only frames and explicit input cross the authenticated channel;
// no phone credential, host profile, debugging socket or filesystem URL does.
type workspacePreview struct {
	mu                                          sync.Mutex
	scope                                       workspaceFileScope
	id, openID, initialURL, profile, cdpSession string
	lastURL                                     string
	process                                     *exec.Cmd
	done                                        chan struct{}
	conn                                        *websocket.Conn
	rpcID                                       int64
	sequence                                    int64
	lastInput                                   [32]byte
	lastInputConfirmed                          bool
	touched                                     time.Time
	width, height                               int
	initialWidth, initialHeight                 int
	closed                                      bool
	dialog                                      *previewDialog
	chooser                                     *previewChooser
	eventSequence                               int64
	downloads                                   map[string]*previewDownload
	upload                                      *previewUpload
	inspection                                  string
	documentID, documentURL, documentFrame      string
	element                                     *previewElement
	domError                                    string
	styleSheets                                 map[string]string
	logsSupported                               bool
	logs                                        []previewLog
	logSequence                                 int64
}
type previewReply struct {
	PageID        string            `json:"pageId"`
	URL           string            `json:"url"`
	Image         string            `json:"imageBase64,omitempty"`
	Width         int               `json:"width"`
	Height        int               `json:"height"`
	NextSequence  int64             `json:"nextSequence"`
	SelectionText string            `json:"selectionText,omitempty"`
	ScrollX       float64           `json:"scrollX"`
	ScrollY       float64           `json:"scrollY"`
	Dialog        *previewDialog    `json:"dialog,omitempty"`
	Chooser       *previewChooser   `json:"chooser,omitempty"`
	Downloads     []previewDownload `json:"downloads,omitempty"`
	Inspection    string            `json:"inspection,omitempty"`
	DOMSupported  bool              `json:"domSupported,omitempty"`
	DocumentID    string            `json:"documentId,omitempty"`
	Element       *previewElement   `json:"element,omitempty"`
	DOMError      string            `json:"domError,omitempty"`
	LogsSupported bool              `json:"logsSupported,omitempty"`
	Logs          []previewLog      `json:"logs,omitempty"`
}

var errPreview = errors.New("preview_unavailable: 电脑网页预览不可用，请重新打开")

func previewURL(raw string) (*url.URL, error) {
	if len(raw) == 0 || len(raw) > 4096 || !utf8.ValidString(raw) || strings.ContainsAny(raw, "\r\n\x00") {
		return nil, errPreview
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Host == "" {
		return nil, errPreview
	}
	host := strings.ToLower(u.Hostname())
	// Only explicit loopback addresses. Never resolve arbitrary names to infer authority.
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return nil, errPreview
	}
	return u, nil
}
func previewBrowserExecutable() string {
	var paths []string
	if runtime.GOOS == "windows" {
		for _, root := range []string{os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramFiles"), os.Getenv("LOCALAPPDATA")} {
			if root != "" {
				paths = append(paths, filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe"), filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"))
			}
		}
	} else if runtime.GOOS == "darwin" {
		paths = []string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"}
	}
	for _, path := range paths {
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			return path
		}
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "microsoft-edge"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return ""
}
func (c *Client) previewScope(ctx context.Context, cmd map[string]any) (workspaceFileScope, error) {
	var zero workspaceFileScope
	key, surface, expected := str(cmd, "sessionKey"), str(cmd, "controlSurface"), str(cmd, "expectedCwd")
	if key == "" || len(key) > 256 || (surface != "cli" && surface != "desktop") || len(expected) > 4096 || c.o.Store == nil {
		return zero, errPreview
	}
	cwd, err := c.workspaceSessionCwd(ctx, key, surface)
	if err != nil || !workspaceExpectedCwdMatches(expected, cwd) {
		return zero, errWorkspaceChanged
	}
	c.taskConfigMu.Lock()
	defer c.taskConfigMu.Unlock()
	if err = c.validateCommandGuard(ctx, str(cmd, "type")); err != nil {
		return zero, err
	}
	cfg := c.o.Store.Get()
	root, err := workspaceConfiguredRoot(cfg, cwd)
	if err != nil {
		return zero, err
	}
	return workspaceFileScope{key, surface, root, config.RemoteIdentity(cfg), cfg.DeviceID, cfg.PhoneBindingID, cfg.PhoneHash}, nil
}
func (c *Client) dispatchWorkspacePreview(ctx context.Context, cmd map[string]any) (any, error) {
	typ := str(cmd, "type")
	allowed := map[string]bool{"type": true, "sessionKey": true, "controlSurface": true, "expectedCwd": true}
	switch typ {
	case "workspace.preview.open":
		allowed["operationId"], allowed["url"], allowed["width"], allowed["height"] = true, true, true, true
	case "workspace.preview.frame":
		allowed["pageId"], allowed["capture"] = true, true
	case "workspace.preview.input":
		allowed["pageId"], allowed["sequence"], allowed["input"] = true, true, true
	case "workspace.preview.close":
		allowed["pageId"] = true
	case "workspace.preview.file":
		allowed["pageId"], allowed["downloadId"], allowed["offset"] = true, true, true
	default:
		return nil, errPreview
	}
	for name := range cmd {
		if !allowed[name] {
			return nil, errPreview
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	scope, err := c.previewScope(ctx, cmd)
	if err != nil {
		return nil, err
	}
	c.previewMu.Lock()
	if c.previews == nil {
		c.previews = map[string]*workspacePreview{}
	}
	var page *workspacePreview
	for id, p := range c.previews {
		if p.scope.identity != scope.identity || p.scope.binding != scope.binding || p.scope.phone != scope.phone || p.scope.device != scope.device {
			p.stop()
			delete(c.previews, id)
		}
	}
	if typ == "workspace.preview.open" {
		raw := str(cmd, "url")
		_, urlErr := previewURL(raw)
		width, wok := previewInteger(cmd["width"], 320, 1440)
		height, hok := previewInteger(cmd["height"], 240, 1200)
		if urlErr != nil || !wok || !hok || !terminalIdentifier(str(cmd, "operationId")) {
			c.previewMu.Unlock()
			return nil, errPreview
		}
		for _, p := range c.previews {
			if p.openID == str(cmd, "operationId") {
				page = p
				break
			}
		}
		if page != nil && (page.scope != scope || page.initialURL != raw || page.initialWidth != width || page.initialHeight != height) {
			c.previewMu.Unlock()
			return nil, errWorkspaceChanged
		}
		if page == nil {
			if len(c.previews) >= 4 {
				c.previewMu.Unlock()
				return nil, errors.New("preview_capacity: 最多打开 4 个电脑预览，请先关闭一个")
			}
			page, err = startWorkspacePreview(ctx, scope, raw, str(cmd, "operationId"), width, height)
			if err != nil {
				c.previewMu.Unlock()
				return nil, err
			}
			c.previews[page.id] = page
			go c.watchWorkspacePreview(page)
		}
	} else {
		page = c.previews[str(cmd, "pageId")]
	}
	c.previewMu.Unlock()
	if page == nil || page.scope != scope {
		return nil, errWorkspaceChanged
	}
	page.mu.Lock()
	defer func() {
		closed := page.closed
		page.mu.Unlock()
		if closed {
			c.previewMu.Lock()
			if c.previews[page.id] == page {
				delete(c.previews, page.id)
			}
			c.previewMu.Unlock()
		}
	}()
	if page.closed {
		return nil, errPreview
	}
	// Waiting for another frame or browser startup must not extend a revoked
	// lease. Check again immediately before admitting any page operation.
	if current, checkErr := c.previewScope(ctx, cmd); checkErr != nil || current != scope {
		page.stopLocked()
		return nil, errWorkspaceChanged
	}
	page.touched = time.Now()
	if typ == "workspace.preview.close" {
		page.stopLocked()
		return map[string]any{"pageId": page.id, "closed": true}, nil
	}
	if typ == "workspace.preview.file" {
		reply, readErr := page.downloadChunk(cmd)
		if readErr != nil {
			return nil, readErr
		}
		if current, checkErr := c.previewScope(ctx, cmd); checkErr != nil || current != scope {
			page.stopLocked()
			return nil, errWorkspaceChanged
		}
		return reply, nil
	}
	if typ == "workspace.preview.input" {
		seq, ok := previewInteger(cmd["sequence"], 1, 1<<40)
		raw, _ := json.Marshal(cmd["input"])
		hash := sha256.Sum256(raw)
		if !ok || len(raw) > 20<<10 {
			return nil, errPreview
		}
		if int64(seq) == page.sequence-1 && hash == page.lastInput {
			if !page.lastInputConfirmed {
				return nil, errors.New("preview_input_uncertain: 网页操作结果未确认，请刷新后检查页面")
			}
			return map[string]any{"pageId": page.id, "sequence": seq}, nil
		}
		if int64(seq) != page.sequence {
			return nil, errors.New("preview_input_uncertain: 网页操作状态未确认，请刷新预览")
		}
		input, ok := cmd["input"].(map[string]any)
		if !ok {
			return nil, errPreview
		}
		if err = page.validateInput(input); err != nil {
			return nil, err
		}
		// Reserve before delivery: a lost CDP response must not repeat a click/submit.
		page.sequence++
		page.lastInput = hash
		page.lastInputConfirmed = false
		if err = page.input(ctx, input); err != nil {
			return nil, err
		}
		if current, checkErr := c.previewScope(ctx, cmd); checkErr != nil || current != scope {
			page.stopLocked()
			return nil, errWorkspaceChanged
		}
		page.lastInputConfirmed = true
		return map[string]any{"pageId": page.id, "sequence": seq}, nil
	}
	capture := true
	if typ == "workspace.preview.frame" {
		var ok bool
		capture, ok = cmd["capture"].(bool)
		if !ok {
			return nil, errPreview
		}
	}
	reply, err := page.frame(ctx, capture)
	if err != nil {
		return nil, err
	}
	latest, err := c.previewScope(ctx, cmd)
	if err != nil || latest != scope {
		page.stopLocked()
		return nil, errWorkspaceChanged
	}
	return reply, nil
}
func startWorkspacePreview(ctx context.Context, scope workspaceFileScope, raw, openID string, width, height int) (*workspacePreview, error) {
	executable := previewBrowserExecutable()
	if executable == "" {
		return nil, errors.New("preview_browser_missing: 电脑需要安装 Microsoft Edge 或 Google Chrome")
	}
	profile, err := os.MkdirTemp("", "salcara-preview-")
	if err != nil {
		return nil, errPreview
	}
	token := make([]byte, 24)
	if _, err = rand.Read(token); err != nil {
		removePreviewProfile(profile)
		return nil, errPreview
	}
	p := &workspacePreview{scope: scope, id: base64.RawURLEncoding.EncodeToString(token), openID: openID, initialURL: raw, profile: profile, width: width, height: height, initialWidth: width, initialHeight: height, sequence: 1, touched: time.Now(), done: make(chan struct{})}
	p.process = exec.Command(executable, "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check", "--disable-extensions", "--disable-background-networking", "--remote-debugging-address=127.0.0.1", "--remote-debugging-port=0", "--user-data-dir="+profile, "about:blank")
	p.process.Env = terminalEnvironment()
	hidePreviewProcess(p.process)
	if err = p.process.Start(); err != nil {
		removePreviewProfile(profile)
		return nil, errPreview
	}
	go func() { _ = p.process.Wait(); close(p.done) }()
	success := false
	defer func() {
		if !success {
			p.stop()
		}
	}()
	var endpoint string
	for {
		rawPort, readErr := os.ReadFile(filepath.Join(profile, "DevToolsActivePort"))
		if readErr == nil {
			lines := strings.Split(strings.TrimSpace(string(rawPort)), "\n")
			if len(lines) == 2 && strings.HasPrefix(lines[1], "/devtools/browser/") {
				endpoint = "ws://127.0.0.1:" + strings.TrimSpace(lines[0]) + strings.TrimSpace(lines[1])
				break
			}
		}
		select {
		case <-ctx.Done():
			return nil, errPreview
		case <-p.done:
			return nil, errPreview
		case <-time.After(40 * time.Millisecond):
		}
	}
	p.conn, _, err = websocket.DefaultDialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return nil, errPreview
	}
	p.conn.SetReadLimit(2 << 20)
	var target struct {
		TargetID string `json:"targetId"`
	}
	if err = p.call(ctx, "Target.createTarget", map[string]any{"url": "about:blank"}, &target); err != nil {
		return nil, err
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err = p.call(ctx, "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached); err != nil {
		return nil, err
	}
	// The dedicated process only contains this preview target. CDP session ID is
	// retained privately; the caller receives an unrelated random page handle.
	p.cdpSession = attached.SessionID
	if err = p.call(ctx, "Emulation.setDeviceMetricsOverride", map[string]any{"width": width, "height": height, "deviceScaleFactor": 1, "mobile": false}, nil); err != nil {
		return nil, err
	}
	if err = p.call(ctx, "Page.enable", map[string]any{}, nil); err != nil {
		return nil, err
	}
	for _, domain := range []string{"DOM.enable", "CSS.enable", "Runtime.enable"} {
		if err = p.call(ctx, domain, map[string]any{}, nil); err != nil {
			return nil, err
		}
	}
	p.logsSupported = true
	if err = os.Mkdir(filepath.Join(profile, "downloads"), 0700); err != nil {
		return nil, errPreview
	}
	if err = p.call(ctx, "Browser.setDownloadBehavior", map[string]any{"behavior": "allowAndName", "downloadPath": filepath.Join(profile, "downloads"), "eventsEnabled": true}, nil); err != nil {
		return nil, err
	}
	if err = p.call(ctx, "Page.setInterceptFileChooserDialog", map[string]any{"enabled": true}, nil); err != nil {
		return nil, err
	}
	if err = p.call(ctx, "Page.navigate", map[string]any{"url": raw}, nil); err != nil {
		return nil, err
	}
	success = true
	return p, nil
}
func (p *workspacePreview) call(ctx context.Context, method string, params any, result any) error {
	if p.conn == nil || ctx.Err() != nil {
		return errPreview
	}
	p.rpcID++
	id := p.rpcID
	deadline := time.Now().Add(8 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = p.conn.SetWriteDeadline(deadline)
	_ = p.conn.SetReadDeadline(deadline)
	message := map[string]any{"id": id, "method": method, "params": params}
	if p.cdpSession != "" && !strings.HasPrefix(method, "Browser.") && !strings.HasPrefix(method, "Target.") {
		message["sessionId"] = p.cdpSession
	}
	if err := p.conn.WriteJSON(message); err != nil {
		return errPreview
	}
	for {
		var reply struct {
			ID        int64           `json:"id"`
			Result    json.RawMessage `json:"result"`
			Error     json.RawMessage `json:"error"`
			Method    string          `json:"method"`
			Params    json.RawMessage `json:"params"`
			SessionID string          `json:"sessionId"`
		}
		if err := p.conn.ReadJSON(&reply); err != nil {
			return errPreview
		}
		if reply.ID != id {
			if reply.Method != "" && (reply.SessionID == "" || reply.SessionID == p.cdpSession) {
				p.handleEvent(reply.Method, reply.Params)
				// Chromium can withhold the triggering input reply until the user
				// dismisses a JavaScript dialog. The opening event proves that the
				// action reached the page; release the page lock so the phone can
				// answer it. Its eventual reply is drained by the next RPC.
				if reply.Method == "Page.javascriptDialogOpening" && (strings.HasPrefix(method, "Input.") || method == "Runtime.evaluate" || method == "Page.getFrameTree" || method == "Page.captureScreenshot" || method == "Page.navigate" || method == "Page.reload") {
					return nil
				}
			}
			continue
		}
		if len(reply.Error) > 0 {
			return errPreview
		}
		if result != nil && json.Unmarshal(reply.Result, result) != nil {
			return errPreview
		}
		return nil
	}
}
func (p *workspacePreview) frame(ctx context.Context, capture bool) (previewReply, error) {
	reply := previewReply{PageID: p.id, Width: p.width, Height: p.height, NextSequence: p.sequence}
	// Renderer commands such as getFrameTree can be suspended by an open
	// JavaScript dialog. Surface the known dialog without issuing another RPC.
	if p.dialog != nil {
		reply.URL = p.initialURL
		if p.lastURL != "" {
			reply.URL = p.lastURL
		}
		if err := p.populateFeatures(ctx, &reply, false); err != nil {
			return reply, err
		}
		return reply, nil
	}
	var tree struct {
		FrameTree struct {
			Frame struct {
				URL string `json:"url"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := p.call(ctx, "Page.getFrameTree", map[string]any{}, &tree); err != nil {
		return reply, err
	}
	u, err := url.Parse(tree.FrameTree.Frame.URL)
	if err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil {
		reply.URL = u.String()
	} else {
		reply.URL = p.initialURL
		if p.lastURL != "" {
			reply.URL = p.lastURL
		}
	}
	p.lastURL = reply.URL
	if capture && p.dialog == nil {
		var shot struct {
			Data string `json:"data"`
		}
		if err := p.call(ctx, "Page.captureScreenshot", map[string]any{"format": "jpeg", "quality": 65, "captureBeyondViewport": false}, &shot); err != nil {
			return reply, err
		}
		if len(shot.Data) > 220<<10 {
			return reply, errors.New("preview_frame_large: 页面图像过大，请缩小预览尺寸")
		}
		reply.Image = shot.Data
	}
	if err = p.populateFeatures(ctx, &reply, capture); err != nil {
		return reply, err
	}
	return reply, nil
}
func (p *workspacePreview) validateInput(input map[string]any) error {
	action := str(input, "action")
	allowed := map[string]bool{"action": true}
	switch action {
	case "click":
		allowed["x"], allowed["y"] = true, true
	case "scroll":
		allowed["deltaY"] = true
	case "text":
		allowed["text"] = true
	case "key":
		allowed["key"], allowed["modifiers"] = true, true
	case "navigate":
		allowed["url"] = true
	case "resize":
		allowed["width"], allowed["height"] = true, true
	case "reload":
	default:
		return p.validateFeatureInput(input)
	}
	for key := range input {
		if !allowed[key] {
			return errPreview
		}
	}
	switch action {
	case "click":
		_, xok := previewInteger(input["x"], 0, p.width-1)
		_, yok := previewInteger(input["y"], 0, p.height-1)
		if !xok || !yok {
			return errPreview
		}
	case "scroll":
		if _, ok := previewNumber(input["deltaY"], -2000, 2000); !ok {
			return errPreview
		}
	case "text":
		text, ok := input["text"].(string)
		if !ok || len(text) > 16384 || !utf8.ValidString(text) {
			return errPreview
		}
	case "key":
		if _, ok := previewKeyCodes[str(input, "key")]; !ok {
			return errPreview
		}
		if value, exists := input["modifiers"]; exists {
			if _, ok := previewInteger(value, 0, 15); !ok {
				return errPreview
			}
		}
	case "navigate":
		if _, err := previewURL(str(input, "url")); err != nil {
			return err
		}
	case "resize":
		_, wok := previewInteger(input["width"], 320, 1440)
		_, hok := previewInteger(input["height"], 240, 1200)
		if !wok || !hok {
			return errPreview
		}
	}
	return nil
}
func (p *workspacePreview) input(ctx context.Context, input map[string]any) error {
	if err := p.validateInput(input); err != nil {
		return err
	}
	action := str(input, "action")
	switch action {
	case "click":
		x, xok := previewInteger(input["x"], 0, p.width-1)
		y, yok := previewInteger(input["y"], 0, p.height-1)
		if !xok || !yok {
			return errPreview
		}
		for _, typ := range []string{"mousePressed", "mouseReleased"} {
			if err := p.call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": typ, "x": x, "y": y, "button": "left", "clickCount": 1}, nil); err != nil {
				return err
			}
		}
	case "scroll":
		delta, ok := previewNumber(input["deltaY"], -2000, 2000)
		if !ok {
			return errPreview
		}
		return p.call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mouseWheel", "x": p.width / 2, "y": p.height / 2, "deltaX": 0, "deltaY": delta}, nil)
	case "text":
		text, ok := input["text"].(string)
		if !ok || len(text) > 16384 || !utf8.ValidString(text) {
			return errPreview
		}
		return p.call(ctx, "Input.insertText", map[string]any{"text": text}, nil)
	case "key":
		key := str(input, "key")
		code, ok := previewKeyCodes[key]
		if !ok {
			return errPreview
		}
		for _, typ := range []string{"keyDown", "keyUp"} {
			params := map[string]any{"type": typ, "key": key, "windowsVirtualKeyCode": code}
			if modifiers, exists := input["modifiers"]; exists {
				params["modifiers"] = modifiers
			}
			if key == "Enter" && typ == "keyDown" {
				params["text"] = "\r"
			}
			if err := p.call(ctx, "Input.dispatchKeyEvent", params, nil); err != nil {
				return err
			}
		}
	case "navigate":
		u, err := previewURL(str(input, "url"))
		if err != nil {
			return err
		}
		return p.call(ctx, "Page.navigate", map[string]any{"url": u.String()}, nil)
	case "reload":
		return p.call(ctx, "Page.reload", map[string]any{"ignoreCache": true}, nil)
	case "resize":
		width, wok := previewInteger(input["width"], 320, 1440)
		height, hok := previewInteger(input["height"], 240, 1200)
		if !wok || !hok {
			return errPreview
		}
		if err := p.call(ctx, "Emulation.setDeviceMetricsOverride", map[string]any{"width": width, "height": height, "deviceScaleFactor": 1, "mobile": false}, nil); err != nil {
			return err
		}
		p.width = width
		p.height = height
	default:
		return p.featureInput(ctx, input)
	}
	return nil
}
func (p *workspacePreview) stop() { p.mu.Lock(); defer p.mu.Unlock(); p.stopLocked() }
func (p *workspacePreview) stopLocked() {
	if p.closed {
		return
	}
	p.closed = true
	if p.conn != nil {
		_ = p.conn.SetWriteDeadline(time.Now().Add(time.Second))
		_ = p.conn.WriteJSON(map[string]any{"id": p.rpcID + 1, "method": "Browser.close"})
		_ = p.conn.Close()
	}
	if p.process != nil && p.process.Process != nil {
		select {
		case <-p.done:
		case <-time.After(2 * time.Second):
			_ = p.process.Process.Kill()
			<-p.done
		}
	}
	// Only the exact directory returned by MkdirTemp belongs to this page.
	removePreviewProfile(p.profile)
}

// Never recursively delete a caller-supplied path or a browser-created link.
// The only admitted directories are direct children of the OS temp directory
// with the exact prefix used by MkdirTemp above.
func removePreviewProfile(profile string) {
	root, rootErr := filepath.Abs(os.TempDir())
	resolved, err := filepath.Abs(profile)
	if err != nil || rootErr != nil || profile == "" || !workspaceSamePath(filepath.Dir(resolved), root) || !strings.HasPrefix(filepath.Base(resolved), "salcara-preview-") || len(filepath.Base(resolved)) <= len("salcara-preview-") {
		return
	}
	st, err := os.Lstat(resolved)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return
	}
	_ = os.RemoveAll(resolved)
}
func (c *Client) watchWorkspacePreview(p *workspacePreview) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		p.mu.Lock()
		expired := p.closed || time.Since(p.touched) > 90*time.Second
		if c.o.Store != nil {
			cfg := c.o.Store.Get()
			expired = expired || config.RemoteIdentity(cfg) != p.scope.identity || cfg.DeviceID != p.scope.device || cfg.PhoneBindingID != p.scope.binding || cfg.PhoneHash != p.scope.phone
			if root, err := workspaceConfiguredRoot(cfg, p.scope.root); err != nil || !workspaceSamePath(root, p.scope.root) {
				expired = true
			}
		}
		select {
		case <-p.done:
			expired = true
		default:
		}
		p.mu.Unlock()
		if !expired {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			cwd, err := c.workspaceSessionCwd(ctx, p.scope.key, p.scope.surface)
			cancel()
			expired = err != nil || !workspaceExpectedCwdMatches(p.scope.root, cwd)
		}
		if expired {
			p.stop()
			c.previewMu.Lock()
			if c.previews[p.id] == p {
				delete(c.previews, p.id)
			}
			c.previewMu.Unlock()
			return
		}
	}
}
func (c *Client) closeWorkspacePreviews() {
	c.previewMu.Lock()
	defer c.previewMu.Unlock()
	for _, p := range c.previews {
		p.stop()
	}
	c.previews = nil
}

func previewInteger(value any, min, max int) (int, bool) {
	n, ok := terminalInteger(value, int64(min), int64(max))
	return int(n), ok
}
func previewNumber(value any, min, max float64) (float64, bool) {
	var number float64
	switch value := value.(type) {
	case float64:
		number = value
	case int:
		number = float64(value)
	case int64:
		number = float64(value)
	default:
		return 0, false
	}
	return number, !math.IsNaN(number) && !math.IsInf(number, 0) && number >= min && number <= max
}

var previewKeyCodes = map[string]int{"Enter": 13, "Tab": 9, "Backspace": 8, "Escape": 27, "ArrowUp": 38, "ArrowDown": 40, "ArrowLeft": 37, "ArrowRight": 39, "Delete": 46, "Home": 36, "End": 35, "PageUp": 33, "PageDown": 34, "a": 65, "c": 67, "v": 86, "x": 88, "y": 89, "z": 90}
