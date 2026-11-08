package hubclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const previewTransferLimit = 8 << 20

type previewDialog struct {
	ID            string `json:"id"`
	Type          string `json:"type"`
	Message       string `json:"message"`
	DefaultPrompt string `json:"defaultPrompt,omitempty"`
}
type previewChooser struct {
	ID       string `json:"id"`
	Multiple bool   `json:"multiple"`
	NodeID   int64  `json:"-"`
}
type previewDownload struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	State string `json:"state"`
}
type previewUpload struct {
	ID, ChooserID, Name, Path string
	Total, Offset             int64
}

var previewTransferID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,100}$`)
var previewDownloadID = regexp.MustCompile(`^[a-fA-F0-9-]{8,100}$`)

func previewDisplayName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return "download.bin"
	}
	if len([]rune(name)) > 160 {
		name = string([]rune(name)[:160])
	}
	return name
}
func (p *workspacePreview) cancelDownload(id string) {
	if p.conn == nil {
		return
	}
	p.rpcID++
	_ = p.conn.WriteJSON(map[string]any{"id": p.rpcID, "method": "Browser.cancelDownload", "params": map[string]any{"guid": id}})
}
func (p *workspacePreview) handleEvent(method string, raw json.RawMessage) {
	p.handleLogEvent(method, raw)
	switch method {
	case "CSS.styleSheetAdded":
		var event struct {
			Header struct {
				ID  string `json:"styleSheetId"`
				URL string `json:"sourceURL"`
			} `json:"header"`
		}
		if json.Unmarshal(raw, &event) == nil && event.Header.ID != "" && len(event.Header.URL) <= 4096 {
			if p.styleSheets == nil {
				p.styleSheets = map[string]string{}
			}
			if len(p.styleSheets) < 128 {
				p.styleSheets[event.Header.ID] = event.Header.URL
			}
		}
	case "CSS.styleSheetRemoved":
		var event struct {
			ID string `json:"styleSheetId"`
		}
		if json.Unmarshal(raw, &event) == nil {
			delete(p.styleSheets, event.ID)
		}
	case "Page.javascriptDialogOpening":
		var e struct{ Type, Message, DefaultPrompt string }
		if json.Unmarshal(raw, &e) != nil {
			return
		}
		p.eventSequence++
		p.dialog = &previewDialog{fmt.Sprint(p.eventSequence), e.Type, string([]rune(e.Message)[:min(len([]rune(e.Message)), 4096)]), string([]rune(e.DefaultPrompt)[:min(len([]rune(e.DefaultPrompt)), 1024)])}
	case "Page.javascriptDialogClosed":
		p.dialog = nil
	case "Page.fileChooserOpened":
		var e struct {
			Mode          string
			BackendNodeID int64 `json:"backendNodeId"`
		}
		if json.Unmarshal(raw, &e) != nil || e.BackendNodeID <= 0 {
			return
		}
		p.eventSequence++
		p.upload = nil
		p.chooser = &previewChooser{fmt.Sprint(p.eventSequence), e.Mode == "selectMultiple", e.BackendNodeID}
	case "Page.frameNavigated":
		var event struct {
			Frame struct {
				ParentID string `json:"parentId"`
			} `json:"frame"`
		}
		if json.Unmarshal(raw, &event) != nil || event.Frame.ParentID != "" {
			return
		}
		p.chooser = nil
		p.upload = nil
		p.inspection = ""
		p.documentID = ""
		p.element = nil
		p.domError = ""
		p.logs = nil
	case "Browser.downloadWillBegin":
		var e struct {
			GUID              string `json:"guid"`
			SuggestedFilename string `json:"suggestedFilename"`
		}
		if json.Unmarshal(raw, &e) != nil || !previewDownloadID.MatchString(e.GUID) {
			return
		}
		if p.downloads == nil {
			p.downloads = map[string]*previewDownload{}
		}
		if len(p.downloads) >= 8 {
			p.cancelDownload(e.GUID)
			return
		}
		p.downloads[e.GUID] = &previewDownload{e.GUID, previewDisplayName(e.SuggestedFilename), 0, "inProgress"}
	case "Browser.downloadProgress":
		var e struct {
			GUID          string `json:"guid"`
			State         string
			ReceivedBytes float64 `json:"receivedBytes"`
			TotalBytes    float64 `json:"totalBytes"`
		}
		if json.Unmarshal(raw, &e) != nil {
			return
		}
		d := p.downloads[e.GUID]
		if d == nil {
			return
		}
		if e.ReceivedBytes > previewTransferLimit || e.TotalBytes > previewTransferLimit {
			d.State = "canceled"
			p.cancelDownload(e.GUID)
			return
		}
		if e.ReceivedBytes >= 0 {
			d.Size = int64(e.ReceivedBytes)
		}
		if e.State == "completed" || e.State == "canceled" {
			d.State = e.State
		}
	}
}
func (p *workspacePreview) populateFeatures(ctx context.Context, reply *previewReply, capture bool) error {
	if p.dialog == nil {
		var result struct {
			Result struct {
				Value struct {
					Text string `json:"text"`
					X, Y float64
				} `json:"value"`
			} `json:"result"`
		}
		// Fixed, read-only expressions. The phone cannot send arbitrary JavaScript.
		expression := `(()=>{const e=document.activeElement;let text=String(getSelection()||'');if(e&&e.tagName==='TEXTAREA'||e&&e.tagName==='INPUT'&&!['password','file'].includes(e.type)){if(typeof e.selectionStart==='number')text=e.value.slice(e.selectionStart,e.selectionEnd)}return {text:text.slice(0,65536),X:scrollX,Y:scrollY}})()`
		if err := p.call(ctx, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true}, &result); err != nil {
			return err
		}
		reply.SelectionText = result.Result.Value.Text
		reply.ScrollX = result.Result.Value.X
		reply.ScrollY = result.Result.Value.Y
	}
	reply.Dialog = p.dialog
	reply.Chooser = p.chooser
	reply.Inspection = p.inspection
	if err := p.populateDOM(ctx, reply); err != nil {
		return err
	}
	ids := make([]string, 0, len(p.downloads))
	for id := range p.downloads {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		reply.Downloads = append(reply.Downloads, *p.downloads[id])
	}
	reply.LogsSupported = p.logsSupported
	reply.Logs = append([]previewLog(nil), p.logs...)
	return nil
}
func (p *workspacePreview) validateFeatureInput(input map[string]any) error {
	action := str(input, "action")
	allowed := map[string]bool{"action": true}
	switch action {
	case "mouse":
		allowed["phase"], allowed["x"], allowed["y"] = true, true, true
	case "scrollTo":
		allowed["x"], allowed["y"] = true, true
	case "dialog":
		allowed["dialogId"], allowed["accept"], allowed["prompt"] = true, true, true
	case "upload":
		for _, k := range []string{"chooserId", "uploadId", "name", "total", "offset", "dataBase64", "final"} {
			allowed[k] = true
		}
	case "inspect":
	default:
		return p.validateDOMInput(input)
	}
	for key := range input {
		if !allowed[key] {
			return errPreview
		}
	}
	switch action {
	case "mouse":
		if phase := str(input, "phase"); phase != "down" && phase != "move" && phase != "up" {
			return errPreview
		}
		if _, ok := previewInteger(input["x"], 0, p.width-1); !ok {
			return errPreview
		}
		if _, ok := previewInteger(input["y"], 0, p.height-1); !ok {
			return errPreview
		}
	case "scrollTo":
		if _, ok := previewInteger(input["x"], 0, 10000000); !ok {
			return errPreview
		}
		if _, ok := previewInteger(input["y"], 0, 10000000); !ok {
			return errPreview
		}
	case "dialog":
		if p.dialog == nil || str(input, "dialogId") != p.dialog.ID {
			return errors.New("preview_dialog_changed: 网页弹窗已变化")
		}
		if _, ok := input["accept"].(bool); !ok {
			return errPreview
		}
		if text := str(input, "prompt"); len(text) > 4096 || !utf8.ValidString(text) {
			return errPreview
		}
	case "upload":
		if p.chooser == nil || str(input, "chooserId") != p.chooser.ID {
			return errors.New("preview_chooser_changed: 请先点击网页里的选择文件按钮")
		}
		if !previewTransferID.MatchString(str(input, "uploadId")) {
			return errPreview
		}
		name := str(input, "name")
		if name == "" || len(name) > 512 || !utf8.ValidString(name) {
			return errPreview
		}
		total, tok := previewInteger(input["total"], 0, previewTransferLimit)
		offset, ook := previewInteger(input["offset"], 0, previewTransferLimit)
		encoded := str(input, "dataBase64")
		data, err := base64.StdEncoding.DecodeString(encoded)
		final, fok := input["final"].(bool)
		if !tok || !ook || !fok || err != nil || len(encoded) > 16384 || offset+len(data) > total || final && offset+len(data) != total {
			return errPreview
		}
		if p.upload != nil && (p.upload.ID != str(input, "uploadId") || p.upload.ChooserID != p.chooser.ID || p.upload.Total != int64(total) || p.upload.Name != name || p.upload.Offset != int64(offset)) {
			return errors.New("preview_upload_changed: 上传进度已变化，请重新选择文件")
		}
		if p.upload == nil && offset != 0 {
			return errPreview
		}
	}
	return nil
}
func (p *workspacePreview) featureInput(ctx context.Context, input map[string]any) error {
	switch str(input, "action") {
	case "mouse":
		phase := str(input, "phase")
		typ := map[string]string{"down": "mousePressed", "move": "mouseMoved", "up": "mouseReleased"}[phase]
		buttons := 1
		if phase == "up" {
			buttons = 0
		}
		return p.call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": typ, "x": input["x"], "y": input["y"], "button": "left", "buttons": buttons, "clickCount": 1}, nil)
	case "scrollTo":
		x, _ := previewInteger(input["x"], 0, 10000000)
		y, _ := previewInteger(input["y"], 0, 10000000)
		return p.call(ctx, "Runtime.evaluate", map[string]any{"expression": fmt.Sprintf("window.scrollTo(%d,%d)", x, y)}, nil)
	case "dialog":
		err := p.call(ctx, "Page.handleJavaScriptDialog", map[string]any{"accept": input["accept"], "promptText": str(input, "prompt")}, nil)
		if err == nil {
			p.dialog = nil
		}
		return err
	case "upload":
		if p.upload == nil {
			folder := filepath.Join(p.profile, "uploads")
			if err := os.MkdirAll(folder, 0700); err != nil {
				return errPreview
			}
			p.upload = &previewUpload{ID: str(input, "uploadId"), ChooserID: p.chooser.ID, Name: str(input, "name"), Path: filepath.Join(folder, str(input, "uploadId")+"-"+previewDisplayName(str(input, "name")))}
			total, _ := previewInteger(input["total"], 0, previewTransferLimit)
			p.upload.Total = int64(total)
		}
		flags := os.O_WRONLY | os.O_APPEND
		if p.upload.Offset == 0 {
			flags |= os.O_CREATE | os.O_EXCL
		}
		file, err := os.OpenFile(p.upload.Path, flags, 0600)
		if err != nil {
			return errPreview
		}
		data, _ := base64.StdEncoding.DecodeString(str(input, "dataBase64"))
		n, writeErr := file.Write(data)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil || n != len(data) {
			return errPreview
		}
		p.upload.Offset += int64(n)
		if input["final"] == true {
			err = p.call(ctx, "DOM.setFileInputFiles", map[string]any{"backendNodeId": p.chooser.NodeID, "files": []string{p.upload.Path}}, nil)
			if err == nil {
				p.upload = nil
				p.chooser = nil
			}
			return err
		}
	case "inspect":
		var value struct {
			Result struct {
				Value string `json:"value"`
			} `json:"result"`
		}
		expression := `JSON.stringify({title:document.title,url:location.href,elements:[...document.querySelectorAll('button,a,input,textarea,select,h1,h2,h3,[role]')].slice(0,200).map(e=>({tag:e.tagName.toLowerCase(),role:e.getAttribute('role'),name:(e.getAttribute('aria-label')||e.innerText||e.getAttribute('placeholder')||'').slice(0,160)}))},null,2).slice(0,65536)`
		if err := p.call(ctx, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true}, &value); err != nil {
			return err
		}
		p.inspection = value.Result.Value
	default:
		return p.domInput(ctx, input)
	}
	return nil
}
func (p *workspacePreview) downloadChunk(cmd map[string]any) (any, error) {
	id := str(cmd, "downloadId")
	entry := p.downloads[id]
	offset, ok := previewInteger(cmd["offset"], 0, previewTransferLimit)
	if !ok || !previewDownloadID.MatchString(id) || entry == nil || entry.State != "completed" {
		return nil, errors.New("preview_download_unavailable: 下载尚未完成或已取消")
	}
	file, err := os.Open(filepath.Join(p.profile, "downloads", id))
	if err != nil {
		return nil, errPreview
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > previewTransferLimit || int64(offset) > st.Size() {
		return nil, errPreview
	}
	data := make([]byte, min(32768, int(st.Size()-int64(offset))))
	n, err := file.ReadAt(data, int64(offset))
	if err != nil && err != io.EOF {
		return nil, errPreview
	}
	return map[string]any{"pageId": p.id, "downloadId": id, "name": entry.Name, "offset": offset, "total": st.Size(), "dataBase64": base64.StdEncoding.EncodeToString(data[:n]), "done": int64(offset+n) == st.Size()}, nil
}
