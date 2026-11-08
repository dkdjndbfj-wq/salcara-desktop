package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"salcara/bridge/internal/protocol"
)

// TaskCreate/Get/Update/List are checklist tools, not Agent/Task process
// lifecycles. These records never enter taskTools, activeTasks or session routing.
// Shapes are the SDK tool outputs, also shipped in Claude Code 2.1.220's
// sdk-tools.d.ts. Text sent to the model is not a structured-state fallback.
const (
	maxClaudeChecklistTasks = 512
	maxClaudeChecklistCalls = 2048
	maxClaudeChecklistBytes = 1 << 20
)

type claudeChecklistTask struct {
	ID, Subject, Status, Owner string
	BlockedBy                  []string
	seq                        uint64
	incarnation                uint64
}

type claudeChecklistCall struct {
	name, taskID, status string
	subject, owner       *string
	blockedBy            []string
	seq                  uint64 // state revision observed when this call started
	incarnation          uint64 // observed target identity, not its task status
	startStatus          string
	startSubject         string
	startOwner           string
	ev                   protocol.Event
	finished             bool
	fullSnapshot         bool
}

type claudeTaskChecklist struct {
	tasks       map[string]claudeChecklistTask
	versions    map[string]uint64 // includes deletions until a newer complete list
	deleted     map[string]uint64 // confirmed deletes fence late creates across reuse
	calls       map[string]*claudeChecklistCall
	callOrder   []string
	seq         uint64 // advances only for an accepted native result
	snapshotSeq uint64 // complete-list result revision, including absent IDs
	complete    bool
	capped      bool
}

func claudeChecklistTool(name string) bool {
	switch name {
	case "TaskCreate", "TaskGet", "TaskUpdate", "TaskList":
		return true
	}
	return false
}

func validChecklistID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func checklistDisplay(s string, limit int) string {
	return truncHead(strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " "), limit)
}

func checklistInputString(input map[string]any, key string) *string {
	if value, ok := input[key].(string); ok {
		value = checklistDisplay(value, 512)
		return &value
	}
	return nil
}

func (m *claudeMapper) beginChecklistTool(id, name string, input map[string]any, event protocol.Event) []protocol.Event {
	if id == "" || len(id) > 512 {
		return []protocol.Event{event}
	}
	if m.checklist == nil {
		m.checklist = &claudeTaskChecklist{tasks: map[string]claudeChecklistTask{}, versions: map[string]uint64{}, deleted: map[string]uint64{}, calls: map[string]*claudeChecklistCall{}}
	}
	c := m.checklist
	if _, seen := c.calls[id]; seen {
		return nil
	}
	if len(c.calls) >= maxClaudeChecklistCalls {
		// Keep every dedup tombstone. Evicting a finished ID lets its old native
		// result be replayed as a fresh mutation. Degrade to ordinary tool rows.
		m.tools[id] = &claudeToolUse{name: name, ev: event}
		out := []protocol.Event{event}
		if !c.capped {
			c.capped = true
			notice := m.base(event.TS)
			hash := sha256.Sum256([]byte(m.sk + "\x00" + m.parentID))
			notice.ID = "claude-tasks-limit:" + hex.EncodeToString(hash[:])
			notice.Type, notice.Level, notice.Text = "notice", "info", "任务状态记录已达上限，后续操作仍显示原始结果"
			out = append(out, notice)
		}
		return out
	}
	taskID, status := inStr(input, "taskId"), inStr(input, "status")
	if !validChecklistID(taskID) {
		taskID = ""
	}
	if len(status) > 32 {
		status = "invalid"
	}
	call := &claudeChecklistCall{name: name, taskID: taskID, status: status, subject: checklistInputString(input, "subject"), owner: checklistInputString(input, "owner"), seq: c.seq, ev: event}
	call.incarnation = c.tasks[taskID].incarnation
	call.startStatus, call.startSubject, call.startOwner = c.tasks[taskID].Status, c.tasks[taskID].Subject, c.tasks[taskID].Owner
	if values, ok := input["addBlockedBy"].([]any); ok && len(values) <= maxClaudeChecklistTasks {
		for _, value := range values {
			if id, ok := value.(string); ok && validChecklistID(id) {
				call.blockedBy = append(call.blockedBy, id)
			}
		}
	}
	c.calls[id] = call
	c.callOrder = append(c.callOrder, id)
	return []protocol.Event{event}
}

func (m *claudeMapper) checklistSourceMatches(line *claudeLine) bool {
	if line != nil && m.parentID == "" && strings.HasPrefix(m.sk, "claude:") {
		for _, id := range []string{line.SessionID, line.SessionIDT} {
			if id != "" && id != strings.TrimPrefix(m.sk, "claude:") {
				return false
			}
		}
	}
	return true
}

// The SDK places one structured result at message level. Multiple result
// blocks do not establish which block owns it; do not guess from a task ID.
// Some local transcript versions use camelCase. Conflicting duplicate fields
// are not admitted, even if one of them happens to fit the requested tool.
func claudeChecklistPayload(line *claudeLine, blocks []claudeBlock) json.RawMessage {
	var resultID string
	count := 0
	for _, b := range blocks {
		if b.Type == "tool_result" {
			count++
			resultID = b.ToolUseID
		}
	}
	if count != 1 || resultID == "" || line.ToolUseID != "" && line.ToolUseID != resultID {
		return nil
	}
	if len(line.ToolUseResult) > 0 && len(line.ToolUseResultT) > 0 {
		return nil
	}
	raw := line.ToolUseResult
	if len(raw) == 0 {
		raw = line.ToolUseResultT
	}
	if len(raw) == 0 || len(raw) > maxClaudeChecklistBytes || !utf8.Valid(raw) {
		return nil
	}
	return raw
}

func (m *claudeMapper) checklistResult(line *claudeLine, block claudeBlock, raw json.RawMessage, ts int64) ([]protocol.Event, bool) {
	if m.checklist == nil {
		return nil, false
	}
	call := m.checklist.calls[block.ToolUseID]
	if call == nil {
		// Capacity fallback retains operation success/failure, but can never
		// resume checklist projection or evict an old call's dedup tombstone.
		tu := m.tools[block.ToolUseID]
		if tu == nil || !claudeChecklistTool(tu.name) {
			return nil, false
		}
		if !m.checklistSourceMatches(line) {
			return nil, true
		}
		event := m.toolResult(block, ts)
		event.OutputFinal = true
		if line.IsError {
			event.Status = "failed"
		}
		if tu.name == "TaskUpdate" {
			var outcome struct {
				Success *bool  `json:"success"`
				Error   string `json:"error"`
			}
			if json.Unmarshal(raw, &outcome) == nil && outcome.Success != nil && (!*outcome.Success || outcome.Error != "") {
				event.Status, event.Output = "failed", truncHead(outcome.Error, maxOutputChars)
				if event.Output == "" {
					event.Output = "任务更新失败"
				}
			}
		}
		return []protocol.Event{event}, true
	}
	// Live admission already checks the worker identity. Retain a guard for
	// direct CLI history projection; Desktop and child readers own their native
	// namespace verification because their display session keys are different.
	if !m.checklistSourceMatches(line) {
		return nil, true
	}
	if call.finished {
		return nil, true
	}
	call.finished = true
	event := call.ev
	event.TS, event.Status, event.OutputFinal = ts, "done", true
	event.Output = truncTail(toolResultText(block.Content), maxOutputChars)
	if block.IsError || line.IsError {
		event.Status = "failed"
		return []protocol.Event{event}, true
	}
	valid, nativeError := m.checklist.apply(call, raw)
	if nativeError != "" {
		event.Status, event.Output = "failed", truncHead(nativeError, maxOutputChars)
	}
	out := []protocol.Event{event}
	if valid {
		snapshot := m.checklistSnapshot(block.ToolUseID, ts)
		out = append(out, snapshot)
	}
	return out, true
}

type claudeChecklistRecord struct {
	ID          string    `json:"id"`
	Subject     *string   `json:"subject"`
	Description *string   `json:"description"`
	Status      string    `json:"status"`
	Owner       *string   `json:"owner"`
	Blocks      *[]string `json:"blocks"`
	BlockedBy   *[]string `json:"blockedBy"`
}

func checklistStatus(status string) bool {
	return status == "pending" || status == "in_progress" || status == "completed"
}

func validChecklistIDs(ids []string) bool {
	if len(ids) > maxClaudeChecklistTasks {
		return false
	}
	for _, id := range ids {
		if !validChecklistID(id) {
			return false
		}
	}
	return true
}

func (record claudeChecklistRecord) task(seq uint64, full bool) (claudeChecklistTask, bool) {
	if !validChecklistID(record.ID) || record.Subject == nil || !checklistStatus(record.Status) || record.BlockedBy == nil || !validChecklistIDs(*record.BlockedBy) {
		return claudeChecklistTask{}, false
	}
	if full && (record.Description == nil || record.Blocks == nil || !validChecklistIDs(*record.Blocks)) {
		return claudeChecklistTask{}, false
	}
	task := claudeChecklistTask{ID: record.ID, Subject: checklistDisplay(*record.Subject, 512), Status: record.Status, BlockedBy: append([]string(nil), (*record.BlockedBy)...), seq: seq}
	if record.Owner != nil {
		task.Owner = checklistDisplay(*record.Owner, 128)
	}
	return task, true
}

func (c *claudeTaskChecklist) freshRead(id string, startedAt uint64) bool {
	return startedAt >= c.snapshotSeq && startedAt >= c.versions[id]
}

func (c *claudeTaskChecklist) put(task claudeChecklistTask) bool {
	if _, exists := c.tasks[task.ID]; !exists && len(c.tasks) >= maxClaudeChecklistTasks {
		return false
	}
	if _, exists := c.versions[task.ID]; !exists && len(c.versions) >= maxClaudeChecklistCalls {
		return false
	}
	if task.incarnation == 0 {
		task.incarnation = c.tasks[task.ID].incarnation
		if task.incarnation == 0 {
			task.incarnation = task.seq
		}
	}
	c.tasks[task.ID], c.versions[task.ID] = task, task.seq
	c.seq = task.seq
	return true
}

func (c *claudeTaskChecklist) remove(id string, seq uint64) bool {
	if _, exists := c.versions[id]; !exists && len(c.versions) >= maxClaudeChecklistCalls {
		return false
	}
	delete(c.tasks, id)
	c.versions[id] = seq
	c.seq = seq
	return true
}

func (c *claudeTaskChecklist) apply(call *claudeChecklistCall, raw json.RawMessage) (bool, string) {
	if len(raw) == 0 {
		return false, ""
	}
	switch call.name {
	case "TaskCreate":
		var result struct {
			Task *claudeChecklistRecord `json:"task"`
		}
		if json.Unmarshal(raw, &result) != nil || result.Task == nil || !validChecklistID(result.Task.ID) || result.Task.Subject == nil {
			return false, ""
		}
		// Creation's documented output has no status. The created checklist item
		// starts pending; a requested status is never accepted from input here.
		if c.deleted[result.Task.ID] > call.seq || c.tasks[result.Task.ID].seq > call.seq {
			return true, ""
		}
		return c.put(claudeChecklistTask{ID: result.Task.ID, Subject: checklistDisplay(*result.Task.Subject, 512), Status: "pending", seq: c.seq + 1, incarnation: c.seq + 1}), ""
	case "TaskGet":
		var result struct {
			Task json.RawMessage `json:"task"`
		}
		if json.Unmarshal(raw, &result) != nil || len(result.Task) == 0 || !validChecklistID(call.taskID) {
			return false, ""
		}
		if string(result.Task) == "null" {
			if !c.freshRead(call.taskID, call.seq) {
				return true, ""
			}
			return c.remove(call.taskID, c.seq+1), ""
		}
		var record claudeChecklistRecord
		if json.Unmarshal(result.Task, &record) != nil || record.ID != call.taskID {
			return false, ""
		}
		task, valid := record.task(c.seq+1, true)
		if !valid {
			return false, ""
		}
		if !c.freshRead(call.taskID, call.seq) {
			return true, ""
		}
		return c.put(task), ""
	case "TaskUpdate":
		return c.update(call, raw)
	case "TaskList":
		var result struct {
			Tasks *[]claudeChecklistRecord `json:"tasks"`
		}
		if json.Unmarshal(raw, &result) != nil || result.Tasks == nil || len(*result.Tasks) > maxClaudeChecklistTasks {
			return false, ""
		}
		tasks := make(map[string]claudeChecklistTask, len(*result.Tasks))
		for _, record := range *result.Tasks {
			task, valid := record.task(c.seq+1, false)
			if !valid {
				return false, ""
			}
			if _, duplicate := tasks[task.ID]; duplicate {
				return false, ""
			}
			tasks[task.ID] = task
		}
		if call.seq < c.snapshotSeq {
			return true, ""
		}
		// A read begun before a newer confirmed change cannot resurrect an old
		// status/deletion, nor discard a task created while the read was running.
		versions := map[string]uint64{}
		superseded := false
		for id, version := range c.versions {
			if version > call.seq {
				superseded = true
				versions[id] = version
				if newer, exists := c.tasks[id]; exists {
					tasks[id] = newer
				} else {
					delete(tasks, id)
				}
			}
		}
		if len(tasks) > maxClaudeChecklistTasks {
			return false, ""
		}
		for id, task := range tasks {
			if task.incarnation == 0 {
				task.incarnation = c.tasks[id].incarnation
				if task.incarnation == 0 {
					task.incarnation = task.seq
				}
				tasks[id] = task
			}
			versions[id] = task.seq
		}
		c.seq++
		c.tasks, c.versions, c.snapshotSeq, c.complete = tasks, versions, c.seq, true
		call.fullSnapshot = !superseded
		return true, ""
	}
	return false, ""
}

func (c *claudeTaskChecklist) update(call *claudeChecklistCall, raw json.RawMessage) (bool, string) {
	var result struct {
		Success       *bool     `json:"success"`
		TaskID        string    `json:"taskId"`
		UpdatedFields *[]string `json:"updatedFields"`
		Error         string    `json:"error"`
		StatusChange  *struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"statusChange"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Success == nil || !validChecklistID(call.taskID) || result.TaskID != call.taskID {
		return false, ""
	}
	if !*result.Success || result.Error != "" {
		if result.Error != "" {
			return false, result.Error
		}
		return false, "任务更新失败"
	}
	if result.UpdatedFields == nil || len(*result.UpdatedFields) > 64 {
		return false, ""
	}
	current, exists := c.tasks[call.taskID]
	// Unknown and explicitly absent are different observations. Update cannot
	// create a task, so a late acknowledgement must not synthesize an item after
	// a newer null, deletion, or complete list established that it is absent.
	if !exists && (c.versions[call.taskID] > call.seq || c.snapshotSeq > call.seq) {
		return false, ""
	}
	// The native ID may have been deleted and reused while this write was in
	// flight. Its late success belongs to the old incarnation, not the new task.
	if c.tasks[call.taskID].incarnation != call.incarnation {
		return false, ""
	}
	fields := map[string]bool{}
	for _, field := range *result.UpdatedFields {
		fields[field] = true
	}
	status := ""
	if result.StatusChange != nil {
		if !checklistStatus(result.StatusChange.From) || !(checklistStatus(result.StatusChange.To) || result.StatusChange.To == "deleted") || !fields["status"] {
			return false, ""
		}
		status = result.StatusChange.To
		if call.status != "" && call.status != status {
			return false, ""
		}
	} else if fields["status"] {
		// Explicit success plus an explicit changed field confirms that input
		// field. A plain acknowledgement or an unlisted field never does.
		if !checklistStatus(call.status) && call.status != "deleted" {
			return false, ""
		}
		status = call.status
	}
	if exists && current.seq > call.seq {
		// statusChange can establish a causal transition despite late delivery.
		// A conflicting predecessor is an older/ambiguous mutation, not proof
		// that the current task should move backwards. Unversioned fields also
		// cannot overwrite a value that changed while this call was in flight.
		if status != "" && (result.StatusChange != nil && current.Status != result.StatusChange.From || result.StatusChange == nil && current.Status != call.startStatus) {
			return false, ""
		}
		if fields["subject"] && current.Subject != call.startSubject || fields["owner"] && current.Owner != call.startOwner {
			return false, ""
		}
	}
	if status == "deleted" {
		if !c.remove(call.taskID, c.seq+1) {
			return false, ""
		}
		c.deleted[call.taskID] = c.seq
		return true, ""
	}
	task := current
	if !exists {
		// A middle/tail read may only observe an update. It supplies no other
		// tasks and cannot establish an unknown item's status from its title.
		if status == "" {
			return false, ""
		}
		task = claudeChecklistTask{ID: call.taskID, Subject: "任务"}
	}
	task.seq = c.seq + 1
	if status != "" {
		task.Status = status
	}
	if fields["subject"] && call.subject != nil {
		task.Subject = *call.subject
	}
	if fields["owner"] && call.owner != nil {
		task.Owner = *call.owner
	}
	if fields["blockedBy"] || fields["addBlockedBy"] {
		seen := map[string]bool{}
		for _, id := range task.BlockedBy {
			seen[id] = true
		}
		for _, id := range call.blockedBy {
			if !seen[id] && len(task.BlockedBy) < maxClaudeChecklistTasks {
				task.BlockedBy = append(task.BlockedBy, id)
				seen[id] = true
			}
		}
	}
	return c.put(task), ""
}

func (m *claudeMapper) checklistSnapshot(toolID string, ts int64) protocol.Event {
	c := m.checklist
	ids := make([]string, 0, len(c.tasks))
	for id := range c.tasks {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, aerr := strconv.ParseUint(ids[i], 10, 64)
		b, berr := strconv.ParseUint(ids[j], 10, 64)
		if (aerr == nil) != (berr == nil) {
			return aerr == nil
		}
		if aerr == nil && berr == nil && a != b {
			return a < b
		}
		return ids[i] < ids[j]
	})
	lines := make([]string, 0, len(ids))
	done := 0
	for _, id := range ids {
		task := c.tasks[id]
		mark := "○"
		if task.Status == "completed" {
			mark = "✓"
			done++
		} else if task.Status == "in_progress" {
			mark = "▸"
		}
		line := mark + " #" + task.ID + " " + task.Subject
		if task.Owner != "" {
			line += " · " + task.Owner
		}
		if len(task.BlockedBy) > 0 {
			visible := task.BlockedBy
			if len(visible) > 8 {
				visible = visible[:8]
			}
			line += " · 等待 #" + strings.Join(visible, "、#")
			if len(task.BlockedBy) > len(visible) {
				line += "…"
			}
		}
		lines = append(lines, line)
	}
	hash := sha256.Sum256([]byte(m.sk + "\x00" + m.parentID + "\x00" + toolID))
	event := m.base(ts)
	event.Type, event.Kind, event.PlanKind = "tool", "plan", "tasks"
	event.ID = "claude-tasks:" + hex.EncodeToString(hash[:])
	// done means this immutable snapshot was read successfully. The checklist
	// completion belongs to its entries, never to the tool's lifecycle status.
	event.Status, event.OutputFinal = "done", true
	event.Title = "任务列表 · "
	// A cached earlier list is not evidence that this single-item operation
	// just enumerated every task. Only the native TaskList result has that scope.
	full := c.complete && c.calls[toolID] != nil && c.calls[toolID].fullSnapshot
	if !full {
		event.Title += "已知 "
	}
	event.Title += itoa(done) + "/" + itoa(len(ids))
	if full && len(ids) == 0 {
		event.Title = "任务列表 · 已清空"
	}
	event.Output = strings.Join(lines, "\n")
	if utf8.RuneCountInString(event.Output) > maxOutputChars {
		shown, used := 0, 0
		for _, line := range lines {
			length := utf8.RuneCountInString(line) + 1
			if used+length > maxOutputChars-64 {
				break
			}
			shown++
			used += length
		}
		event.Detail = "显示前 " + itoa(shown) + "/" + itoa(len(lines)) + " 项"
		event.Output = strings.Join(lines[:shown], "\n") + "\n…其余 " + itoa(len(lines)-shown) + " 项未展开"
	}
	return event
}
