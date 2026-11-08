package agents

import (
	"encoding/json"
	"strings"

	"salcara/bridge/internal/protocol"
)

// CodexToolResultFailed reads explicit failure flags on typed tool results.
// It does not infer completion from an absent flag, prose, or a parent turn.
func CodexToolResultFailed(raw json.RawMessage) bool {
	var item struct {
		Type    string          `json:"type"`
		Success *bool           `json:"success"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return false
	}
	return codexToolResultFailed(item.Type, item.Success, item.Result, item.Error)
}

func codexToolResultFailed(kind string, success *bool, result, toolError json.RawMessage) bool {
	if kind != "mcpToolCall" && kind != "dynamicToolCall" {
		return false
	}
	if success != nil && !*success {
		return true
	}
	if kind != "mcpToolCall" {
		return false
	}
	if value := strings.TrimSpace(string(toolError)); value != "" && value != "null" {
		return true
	}
	var status struct {
		IsError bool `json:"isError"`
	}
	return json.Unmarshal(result, &status) == nil && status.IsError
}

// ComputerActivityFromCodexItem identifies only explicit computer/browser tool
// interfaces. Code, prose, URLs and claimed screenshots cannot establish it.
func ComputerActivityFromCodexItem(raw json.RawMessage) *protocol.ComputerActivity {
	var item cxItem
	if json.Unmarshal(raw, &item) != nil {
		return nil
	}
	return codexComputerActivity(item)
}

func codexComputerActivity(item cxItem) *protocol.ComputerActivity {
	server := item.Server
	if item.Type == "dynamicToolCall" && item.Namespace != nil {
		server = *item.Namespace
	}
	if item.Type != "mcpToolCall" && item.Type != "dynamicToolCall" {
		return nil
	}
	return computerActivityForTool(server, item.Tool, item.Arguments)
}

func computerActivityForTool(server, tool string, arguments json.RawMessage) *protocol.ComputerActivity {
	server, tool, ok := browserToolIdentity(server, tool)
	if !ok {
		return nil
	}
	surface, action := "", ""
	switch server {
	case "cua_repl":
		if tool == "js" {
			surface, action = "computer", "script"
		}
	case "computer", "computer_use":
		surface = "computer"
		switch tool {
		case "click", "double_click", "right_click":
			action = "click"
		case "type", "type_text":
			action = "type"
		case "scroll":
			action = "scroll"
		case "key", "key_press", "keypress":
			action = "key"
		case "move", "move_mouse":
			action = "move"
		case "drag":
			action = "drag"
		case "screenshot", "get_screenshot":
			action = "screenshot"
		case "computer":
			var args struct {
				Action string `json:"action"`
			}
			if json.Unmarshal(arguments, &args) == nil {
				switch args.Action {
				case "left_click", "right_click", "double_click":
					action = "click"
				case "type":
					action = "type"
				case "key":
					action = "key"
				case "scroll":
					action = "scroll"
				case "mouse_move":
					action = "move"
				case "left_click_drag":
					action = "drag"
				case "screenshot":
					action = "screenshot"
				}
			}
		}
	case "playwright", "puppeteer", "chrome-devtools", "chrome_devtools":
		surface = "browser"
		switch tool {
		case "browser_click", "puppeteer_click", "click", "dblclick":
			action = "click"
		case "browser_type", "browser_fill_form", "puppeteer_fill", "fill", "fill_form", "type_text":
			action = "type"
		case "browser_press_key", "press_key":
			action = "key"
		case "browser_drag", "drag":
			action = "drag"
		case "browser_hover", "puppeteer_hover", "hover":
			action = "move"
		case "browser_take_screenshot", "puppeteer_screenshot", "take_screenshot":
			action = "screenshot"
		case "browser_snapshot", "take_snapshot":
			action = "inspect"
		case "browser_navigate", "puppeteer_navigate", "navigate_page", "new_page":
			action = "navigate"
		case "browser_evaluate", "browser_run_code", "puppeteer_evaluate", "evaluate_script":
			action = "script"
		case "browser_wait_for", "wait_for":
			action = "wait"
		}
	}
	if surface == "" || action == "" {
		return nil
	}
	return &protocol.ComputerActivity{Version: 1, Surface: surface, Action: action, Provider: server}
}

// Native history already belongs to the explicitly leased thread. Only inline
// tool image content is forwarded; arbitrary URLs and local paths are not read.
func ComputerActivityImages(raw json.RawMessage) []string {
	var item cxItem
	if json.Unmarshal(raw, &item) != nil || codexComputerActivity(item) == nil {
		return nil
	}
	if item.Type == "mcpToolCall" {
		return mcpResultImages(item.Result)
	}
	_, images := dynamicToolContent(item.ContentItems)
	return images
}
