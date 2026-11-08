package hubclient

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

type previewRect struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}
type previewStyleChange struct {
	Property       string `json:"property"`
	Before         string `json:"before"`
	BeforePriority string `json:"beforePriority"`
	Value          string `json:"value"`
	Priority       string `json:"priority"`
}
type previewStyleSource struct {
	URL      string `json:"url"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	Selector string `json:"selector"`
}
type previewElement struct {
	ID                      string               `json:"id"`
	DocumentID              string               `json:"documentId"`
	Revision                int                  `json:"revision"`
	URL                     string               `json:"url"`
	Tag                     string               `json:"tag"`
	Selector                string               `json:"selector"`
	Text                    string               `json:"text"`
	OuterHTML               string               `json:"outerHTML"`
	Rect                    previewRect          `json:"rect"`
	Computed                map[string]string    `json:"computed"`
	Changes                 []previewStyleChange `json:"changes"`
	Sources                 []previewStyleSource `json:"sources"`
	objectID                string
	backendID               int64
	originalCSS, appliedCSS string
}

// Deliberately excludes resource-bearing properties and arbitrary stylesheet text.
var previewStyleProperties = strings.Fields("color background-color font-size font-weight font-family line-height letter-spacing text-align text-decoration-color opacity display visibility width height min-width min-height max-width max-height margin-top margin-right margin-bottom margin-left padding-top padding-right padding-bottom padding-left border-top-width border-right-width border-bottom-width border-left-width border-top-color border-right-color border-bottom-color border-left-color border-top-style border-right-style border-bottom-style border-left-style border-radius gap row-gap column-gap flex-direction align-items justify-content flex-grow flex-shrink flex-basis position top right bottom left z-index overflow-x overflow-y box-shadow transform")

func previewStyleAllowed(property string) bool {
	for _, v := range previewStyleProperties {
		if v == property {
			return true
		}
	}
	return false
}
func previewStyleValue(value string) bool {
	if len(value) > 512 || !utf8.ValidString(value) || strings.ContainsAny(value, "\\@;{}<>\x00\r\n") {
		return false
	}
	lower := strings.ToLower(value)
	for _, token := range []string{"url(", "url (", "expression", "javascript:", "data:", "/*", "*/", "var(", "env(", "attr("} {
		if strings.Contains(lower, token) {
			return false
		}
	}
	return true
}
func (p *workspacePreview) validateDOMInput(input map[string]any) error {
	allowed := map[string]bool{"action": true, "documentId": true}
	switch str(input, "action") {
	case "element-pick":
		allowed["x"], allowed["y"] = true, true
		if _, ok := previewInteger(input["x"], 0, p.width-1); !ok {
			return errPreview
		}
		if _, ok := previewInteger(input["y"], 0, p.height-1); !ok {
			return errPreview
		}
	case "style-preview", "style-undo":
		allowed["elementId"], allowed["revision"] = true, true
		if !previewTransferID.MatchString(str(input, "elementId")) {
			return errPreview
		}
		if _, ok := previewInteger(input["revision"], 1, 1<<30); !ok {
			return errPreview
		}
		if str(input, "action") == "style-preview" {
			allowed["property"], allowed["value"], allowed["important"] = true, true, true
			value, ok := input["value"].(string)
			if !ok || !previewStyleAllowed(str(input, "property")) || !previewStyleValue(value) {
				return errPreview
			}
			if _, ok := input["important"].(bool); !ok {
				return errPreview
			}
		}
	default:
		return errPreview
	}
	if !previewTransferID.MatchString(str(input, "documentId")) {
		return errPreview
	}
	for key := range input {
		if !allowed[key] {
			return errPreview
		}
	}
	return nil
}

func (p *workspacePreview) refreshDOMDocument(ctx context.Context) (bool, error) {
	var tree struct {
		FrameTree struct {
			Frame struct {
				ID       string `json:"id"`
				LoaderID string `json:"loaderId"`
				URL      string `json:"url"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := p.call(ctx, "Page.getFrameTree", map[string]any{}, &tree); err != nil {
		return false, err
	}
	f := tree.FrameTree.Frame
	if p.dialog != nil {
		return false, nil
	}
	if _, err := previewURL(f.URL); err != nil || f.ID == "" || f.LoaderID == "" {
		p.element = nil
		p.documentID = ""
		return false, nil
	}
	hash := sha256.Sum256([]byte(p.id + "\x00" + f.ID + "\x00" + f.LoaderID))
	id := base64.RawURLEncoding.EncodeToString(hash[:])
	if p.documentID != id {
		p.element = nil
		p.domError = ""
	}
	p.documentID, p.documentURL, p.documentFrame = id, f.URL, f.ID
	return true, nil
}
func (p *workspacePreview) populateDOM(ctx context.Context, reply *previewReply) error {
	if p.dialog != nil {
		reply.DocumentID = p.documentID
		return nil
	}
	ok, err := p.refreshDOMDocument(ctx)
	if err != nil {
		return err
	}
	reply.DOMSupported = ok
	reply.DocumentID = p.documentID
	reply.DOMError = p.domError
	if !ok || p.element == nil {
		return nil
	}
	e := p.element
	value, err := p.elementCall(ctx, e, "read", map[string]any{})
	if err != nil {
		return err
	}
	if p.element != e || p.documentID != e.DocumentID {
		reply.DocumentID = p.documentID
		reply.DOMSupported = p.documentID != ""
		return nil
	}
	if value.Error != "" {
		p.element = nil
		p.domError = value.Error
		reply.DOMError = value.Error
		return nil
	}
	p.updateElementSnapshot(e, value)
	reply.Element = e
	return nil
}

type previewDOMResult struct {
	Error          string            `json:"error"`
	Tag            string            `json:"tag"`
	Selector       string            `json:"selector"`
	Text           string            `json:"text"`
	OuterHTML      string            `json:"outerHTML"`
	Rect           previewRect       `json:"rect"`
	Computed       map[string]string `json:"computed"`
	CSS            string            `json:"css"`
	Before         string            `json:"before"`
	BeforePriority string            `json:"beforePriority"`
	Value          string            `json:"value"`
	Priority       string            `json:"priority"`
}

// Fixed code only. Mutation uses the retained node object, never a selector lookup.
const previewDOMFunction = `function(action,p){
 if(!(this instanceof Element)||!this.isConnected||this.ownerDocument!==document||window!==top)return {error:'所选元素已被网页替换，请重新选择'};
 if(action!=='read'&&action!=='undo'&&location.href!==p.url)return {error:'网页地址已变化，请撤销预览后重新选择'};
 const e=this, s=e.style;
 let before='',beforePriority='',value='',priority='';
 if(action==='apply'||action==='undo'){
  if(p.expectedCSS!==null&&s.cssText!==p.expectedCSS)return {error:'网页已自行修改此元素的样式，未覆盖新变化；刷新页面可清除临时预览'};
  if(action==='undo'){s.cssText=p.originalCSS;}
  else{
   if(p.value!==''&&!CSS.supports(p.property,p.value))return {error:'浏览器不支持这个样式值'};
   const original=document.createElement('div').style;original.cssText=p.originalCSS===null?s.cssText:p.originalCSS;
   before=original.getPropertyValue(p.property);beforePriority=original.getPropertyPriority(p.property);
   if(p.value==='')s.removeProperty(p.property);else s.setProperty(p.property,p.value,p.important?'important':'');
   value=s.getPropertyValue(p.property);priority=s.getPropertyPriority(p.property);
  }
 }
 const r=e.getBoundingClientRect(), computed={},cs=getComputedStyle(e);
 for(const k of p.properties)computed[k]=cs.getPropertyValue(k).slice(0,512);
 const parts=[];let n=e;
 for(let i=0;n&&i<8;i++,n=n.parentElement){let v=n.tagName.toLowerCase();if(n.id){parts.unshift('#'+CSS.escape(n.id));break;}const parent=n.parentElement;if(parent){const same=[...parent.children].filter(x=>x.tagName===n.tagName);if(same.length>1)v+=':nth-of-type('+(same.indexOf(n)+1)+')';}parts.unshift(v);}
 return {tag:e.tagName.toLowerCase(),selector:parts.join(' > ').slice(0,2048),text:(e.innerText||e.textContent||'').slice(0,2048),outerHTML:e.outerHTML.slice(0,8192),rect:{x:r.x,y:r.y,width:r.width,height:r.height},computed,css:s.cssText,before,beforePriority,value,priority};
}`

func (p *workspacePreview) elementCall(ctx context.Context, e *previewElement, action string, payload map[string]any) (previewDOMResult, error) {
	payload["properties"] = previewStyleProperties
	payload["url"] = e.URL
	var result struct {
		Result struct {
			Value previewDOMResult `json:"value"`
		} `json:"result"`
		Exception json.RawMessage `json:"exceptionDetails"`
	}
	err := p.call(ctx, "Runtime.callFunctionOn", map[string]any{"objectId": e.objectID, "functionDeclaration": previewDOMFunction, "arguments": []any{map[string]any{"value": action}, map[string]any{"value": payload}}, "returnByValue": true}, &result)
	if err != nil {
		return previewDOMResult{}, err
	}
	if len(result.Exception) > 0 {
		return previewDOMResult{}, errPreview
	}
	return result.Result.Value, nil
}
func (p *workspacePreview) updateElementSnapshot(e *previewElement, v previewDOMResult) {
	e.Tag = v.Tag
	e.Selector = v.Selector
	e.Text = v.Text
	e.OuterHTML = v.OuterHTML
	e.Rect = v.Rect
	e.Computed = v.Computed
}
func (p *workspacePreview) rejectDOM(message string) error { p.domError = message; return nil }
func (p *workspacePreview) domInput(ctx context.Context, input map[string]any) error {
	if p.dialog != nil {
		return p.rejectDOM("请先处理网页弹窗")
	}
	ok, err := p.refreshDOMDocument(ctx)
	if err != nil {
		return err
	}
	if !ok || str(input, "documentId") != p.documentID {
		return p.rejectDOM("网页已刷新或不支持元素编辑，请重新选择")
	}
	p.domError = ""
	if str(input, "action") == "element-pick" {
		if p.element != nil && len(p.element.Changes) > 0 {
			return p.rejectDOM("请先撤销当前元素的临时样式，再选择其他元素")
		}
		var hit struct {
			BackendID int64  `json:"backendNodeId"`
			FrameID   string `json:"frameId"`
		}
		if err := p.call(ctx, "DOM.getNodeForLocation", map[string]any{"x": input["x"], "y": input["y"], "includeUserAgentShadowDOM": false}, &hit); err != nil {
			return err
		}
		if hit.BackendID <= 0 || hit.FrameID != p.documentFrame {
			return p.rejectDOM("嵌入网页中的元素暂不支持编辑，请选择主页面元素")
		}
		p.element = nil
		if err := p.call(ctx, "Runtime.releaseObjectGroup", map[string]any{"objectGroup": "salcara-element"}, nil); err != nil {
			return err
		}
		var resolved struct {
			Object struct {
				ID string `json:"objectId"`
			} `json:"object"`
		}
		if err := p.call(ctx, "DOM.resolveNode", map[string]any{"backendNodeId": hit.BackendID, "objectGroup": "salcara-element"}, &resolved); err != nil {
			return err
		}
		if resolved.Object.ID == "" {
			return p.rejectDOM("这里没有可编辑的页面元素")
		}
		p.eventSequence++
		e := &previewElement{ID: fmt.Sprintf("element_%d", p.eventSequence), DocumentID: p.documentID, Revision: 1, URL: p.documentURL, objectID: resolved.Object.ID, backendID: hit.BackendID, Changes: []previewStyleChange{}, Sources: []previewStyleSource{}}
		v, err := p.elementCall(ctx, e, "read", map[string]any{})
		if err != nil {
			return err
		}
		if v.Error != "" {
			return p.rejectDOM(v.Error)
		}
		p.updateElementSnapshot(e, v)
		e.originalCSS = v.CSS
		e.appliedCSS = v.CSS
		if err := p.populateElementSources(ctx, e); err != nil {
			return err
		}
		// Reading sources may drain a navigation event. Never attach an old node to a new document.
		if p.documentID != e.DocumentID {
			return p.rejectDOM("网页已刷新，请重新选择")
		}
		p.element = e
		return nil
	}
	e := p.element
	revision, _ := previewInteger(input["revision"], 1, 1<<30)
	if e == nil || e.ID != str(input, "elementId") || e.DocumentID != p.documentID || e.Revision != revision {
		return p.rejectDOM("元素或样式版本已变化，请使用最新选择重试")
	}
	if str(input, "action") == "style-undo" {
		if len(e.Changes) == 0 {
			return nil
		}
		v, err := p.elementCall(ctx, e, "undo", map[string]any{"expectedCSS": e.appliedCSS, "originalCSS": e.originalCSS})
		if err != nil {
			return err
		}
		if v.Error != "" {
			return p.rejectDOM(v.Error)
		}
		p.updateElementSnapshot(e, v)
		e.appliedCSS = v.CSS
		e.originalCSS = v.CSS
		e.Changes = []previewStyleChange{}
		e.Revision++
		return nil
	}
	property := str(input, "property")
	index := -1
	for i, c := range e.Changes {
		if c.Property == property {
			index = i
			break
		}
	}
	if index < 0 && len(e.Changes) >= 16 {
		return p.rejectDOM("单个元素最多预览 16 项样式，请先撤销")
	}
	payload := map[string]any{"property": property, "value": str(input, "value"), "important": input["important"], "expectedCSS": e.appliedCSS, "originalCSS": e.originalCSS}
	v, err := p.elementCall(ctx, e, "apply", payload)
	if err != nil {
		return err
	}
	if v.Error != "" {
		return p.rejectDOM(v.Error)
	}
	p.updateElementSnapshot(e, v)
	e.appliedCSS = v.CSS
	e.Revision++
	change := previewStyleChange{property, v.Before, v.BeforePriority, v.Value, v.Priority}
	if index < 0 {
		e.Changes = append(e.Changes, change)
	} else {
		e.Changes[index] = change
	}
	return nil
}

func (p *workspacePreview) populateElementSources(ctx context.Context, e *previewElement) error {
	// Request the frontend DOM node handle only inside the private CDP session.
	if err := p.call(ctx, "DOM.getDocument", map[string]any{}, nil); err != nil {
		return err
	}
	var nodes struct {
		IDs []int64 `json:"nodeIds"`
	}
	if err := p.call(ctx, "DOM.pushNodesByBackendIdsToFrontend", map[string]any{"backendNodeIds": []int64{e.backendID}}, &nodes); err != nil {
		return err
	}
	if len(nodes.IDs) == 0 || nodes.IDs[0] == 0 {
		return nil
	}
	var matched struct {
		Rules []struct {
			Rule struct {
				SheetID   string `json:"styleSheetId"`
				Selectors struct {
					Text string `json:"text"`
				} `json:"selectorList"`
				Style struct {
					Range *struct {
						Line   int `json:"startLine"`
						Column int `json:"startColumn"`
					} `json:"range"`
				} `json:"style"`
			} `json:"rule"`
		} `json:"matchedCSSRules"`
	}
	if err := p.call(ctx, "CSS.getMatchedStylesForNode", map[string]any{"nodeId": nodes.IDs[0]}, &matched); err != nil {
		return err
	}
	for _, m := range matched.Rules {
		rule := m.Rule
		url := p.styleSheets[rule.SheetID]
		if url == "" || rule.Style.Range == nil || len(e.Sources) >= 12 {
			continue
		}
		selector := rule.Selectors.Text
		if len(selector) > 2048 {
			selector = selector[:2048]
		}
		e.Sources = append(e.Sources, previewStyleSource{url, rule.Style.Range.Line + 1, rule.Style.Range.Column + 1, selector})
	}
	return nil
}
