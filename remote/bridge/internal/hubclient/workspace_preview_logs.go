package hubclient

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

type previewLog struct {
	ID        string `json:"id"`
	Level     string `json:"level"`
	Message   string `json:"message"`
	Source    string `json:"source,omitempty"`
	Line      int64  `json:"line,omitempty"`
	Column    int64  `json:"column,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}
type previewLogArgument struct {
	Type           string          `json:"type"`
	Value          json.RawMessage `json:"value"`
	Description    string          `json:"description"`
	Unserializable string          `json:"unserializableValue"`
}
type previewLogLocation struct {
	URL    string `json:"url"`
	Line   int64  `json:"lineNumber"`
	Column int64  `json:"columnNumber"`
}
type previewLogStack struct {
	Frames []previewLogLocation `json:"callFrames"`
}

func previewLogText(value string, limit int) (string, bool) {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return -1
		}
		return r
	}, value)
	// The mobile wire parser bounds JavaScript string length (UTF-16 units),
	// so a supplementary-plane character consumes two units. Never split one.
	units := 0
	for index, r := range value {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units+width > limit {
			return value[:index], true
		}
		units += width
	}
	return value, false
}
func previewLogValue(arg previewLogArgument) string {
	if len(arg.Value) > 0 {
		var text string
		if json.Unmarshal(arg.Value, &text) == nil {
			return text
		}
		return string(arg.Value)
	}
	if arg.Unserializable != "" {
		return arg.Unserializable
	}
	if arg.Description != "" {
		return arg.Description
	}
	return arg.Type
}
func (p *workspacePreview) appendLog(level, message string, location previewLogLocation) {
	message, clipped := previewLogText(message, 2000)
	if message == "" {
		return
	}
	source, _ := previewLogText(location.URL, 1024)
	p.logSequence++
	entry := previewLog{ID: fmt.Sprintf("cdp:%d", p.logSequence), Level: level, Message: message, Source: source, Truncated: clipped}
	if source != "" && location.Line >= 0 && location.Line < 99999999 {
		entry.Line = location.Line + 1
	}
	if entry.Line > 0 && location.Column >= 0 && location.Column < 99999999 {
		entry.Column = location.Column + 1
	}
	p.logs = append(p.logs, entry)
	bytes := 0
	for i := len(p.logs) - 1; i >= 0; i-- {
		bytes += len(p.logs[i].Message) + len(p.logs[i].Source)
		if bytes > 32000 || len(p.logs)-i > 64 {
			p.logs = append([]previewLog(nil), p.logs[i+1:]...)
			break
		}
	}
}
func (p *workspacePreview) handleLogEvent(method string, raw json.RawMessage) {
	if !p.logsSupported {
		return
	}
	switch method {
	case "Runtime.consoleAPICalled":
		var event struct {
			Type  string               `json:"type"`
			Args  []previewLogArgument `json:"args"`
			Stack previewLogStack      `json:"stackTrace"`
		}
		if json.Unmarshal(raw, &event) != nil {
			return
		}
		level := event.Type
		switch level {
		case "warning":
			level = "warn"
		case "assert":
			level = "error"
		case "debug", "log", "info", "warn", "error":
		default:
			return
		}
		parts := make([]string, 0, min(len(event.Args), 20))
		for i, arg := range event.Args {
			if i == 20 {
				break
			}
			parts = append(parts, previewLogValue(arg))
		}
		location := previewLogLocation{Line: -1, Column: -1}
		if len(event.Stack.Frames) > 0 {
			location = event.Stack.Frames[0]
		}
		p.appendLog(level, strings.Join(parts, " "), location)
	case "Runtime.exceptionThrown":
		var event struct {
			Details struct {
				Text      string             `json:"text"`
				Exception previewLogArgument `json:"exception"`
				previewLogLocation
				Stack previewLogStack `json:"stackTrace"`
			} `json:"exceptionDetails"`
		}
		if json.Unmarshal(raw, &event) != nil {
			return
		}
		message := previewLogValue(event.Details.Exception)
		if message == "" {
			message = event.Details.Text
		}
		location := event.Details.previewLogLocation
		if location.URL == "" && len(event.Details.Stack.Frames) > 0 {
			location = event.Details.Stack.Frames[0]
		}
		p.appendLog("error", message, location)
	}
}
