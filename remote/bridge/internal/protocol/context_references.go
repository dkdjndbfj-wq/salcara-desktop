package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

var contextSessionKey = regexp.MustCompile(`(?i)^[a-z][a-z0-9_-]*:[^|]+$`)
var contextSourceRevision = regexp.MustCompile(`^[a-f0-9]{64}$`)

func contextSafeText(text string, max int) bool {
	return text != "" && len(text) <= max && utf8.ValidString(text) && strings.IndexFunc(text, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) < 0
}

// ContextReference is a user-selected immutable snapshot, never a filesystem
// capability or a request to load/control the source session.
type ContextReference struct {
	Kind        string `json:"kind"`
	SessionKey  string `json:"sessionKey,omitempty"`
	ProjectPath string `json:"projectPath,omitempty"`
	Label       string `json:"label"`
	Text        string `json:"text"`
	Path        string `json:"path,omitempty"`
	MessageID   string `json:"messageId,omitempty"`
	StartLine   *int64 `json:"startLine,omitempty"`
	EndLine     *int64 `json:"endLine,omitempty"`
	StartOffset *int64 `json:"startOffset,omitempty"`
	EndOffset   *int64 `json:"endOffset,omitempty"`
	CapturedAt  int64  `json:"capturedAt"`
	// Selection offsets are UTF-16, in the explicitly named text coordinate
	// system. The source revision only prevents stale highlighting; it grants
	// no permission to read the source message or session.
	Annotation     *string `json:"annotation,omitempty"`
	TextBasis      string  `json:"textBasis,omitempty"`
	BlockIndex     *int64  `json:"blockIndex,omitempty"`
	SourceRevision string  `json:"sourceRevision,omitempty"`
	Side           string  `json:"side,omitempty"`
}

func (ref *ContextReference) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return errors.New("invalid context reference object")
	}
	allowed := map[string]bool{"kind": true, "sessionKey": true, "projectPath": true, "label": true, "text": true, "capturedAt": true, "path": true, "messageId": true, "startLine": true, "endLine": true, "startOffset": true, "endOffset": true, "annotation": true, "textBasis": true, "blockIndex": true, "sourceRevision": true, "side": true}
	for key := range fields {
		if !allowed[key] {
			return errors.New("unknown context reference field")
		}
		if bytes.Equal(bytes.TrimSpace(fields[key]), []byte("null")) {
			return errors.New("null context reference field")
		}
	}
	type plain ContextReference
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	required := []string{"kind", "label", "text", "capturedAt"}
	forbidden := []string{"path", "startLine", "endLine", "projectPath", "side"}
	if value.Kind == "diff" {
		required = append(required, "sessionKey", "path", "startLine", "endLine", "side", "sourceRevision")
		forbidden = []string{"projectPath", "messageId", "startOffset", "endOffset", "textBasis", "blockIndex"}
	} else if value.Kind == "file" {
		_, hasSession := fields["sessionKey"]
		_, hasProject := fields["projectPath"]
		if hasSession == hasProject {
			return errors.New("file reference requires one source")
		}
		if hasSession {
			required = append(required, "sessionKey")
		} else {
			required = append(required, "projectPath")
		}
		required = append(required, "path", "startLine", "endLine")
		forbidden = []string{"messageId", "startOffset", "endOffset", "annotation", "textBasis", "blockIndex", "sourceRevision", "side"}
	} else {
		required = append(required, "sessionKey", "messageId", "startOffset", "endOffset")
		_, hasAnnotation := fields["annotation"]
		_, hasBasis := fields["textBasis"]
		_, hasBlock := fields["blockIndex"]
		_, hasRevision := fields["sourceRevision"]
		if hasAnnotation || hasBasis || hasBlock || hasRevision {
			if !hasBasis || !hasRevision || value.TextBasis == "" || value.SourceRevision == "" {
				return errors.New("selection annotation requires text basis and source revision")
			}
		}
	}
	for _, key := range required {
		if raw, exists := fields[key]; !exists || string(raw) == "null" {
			return errors.New("missing context reference field")
		}
	}
	for _, key := range forbidden {
		if _, exists := fields[key]; exists {
			return errors.New("context reference contains a field for another kind")
		}
	}
	*ref = ContextReference(value)
	return ValidateContextReferences([]ContextReference{*ref})
}

func ValidateContextReferences(refs []ContextReference) error {
	invalid := errors.New("context_reference_invalid: 上下文引用格式无效或超过限制")
	if len(refs) > 8 {
		return invalid
	}
	total := 0
	for _, ref := range refs {
		total += len(ref.Text)
		if ref.Annotation != nil {
			total += len(*ref.Annotation)
			if !contextSnapshotText(*ref.Annotation, 8<<10) {
				return invalid
			}
		}
		if (ref.SessionKey != "" && (!contextSafeText(ref.SessionKey, 256) || !contextSessionKey.MatchString(ref.SessionKey))) ||
			!contextSnapshotText(ref.Text, 32<<10) || ref.Text == "" || total > 64<<10 ||
			!contextSafeText(ref.Label, 200) ||
			ref.CapturedAt <= 0 || ref.CapturedAt > 9007199254740991 {
			return invalid
		}
		switch ref.Kind {
		case "file", "diff":
			if ref.Kind == "diff" {
				if ref.SessionKey == "" || ref.ProjectPath != "" || (ref.Side != "old" && ref.Side != "new") || !contextSourceRevision.MatchString(ref.SourceRevision) || ref.TextBasis != "" || ref.BlockIndex != nil {
					return invalid
				}
			} else if ref.Annotation != nil || ref.TextBasis != "" || ref.BlockIndex != nil || ref.SourceRevision != "" || ref.Side != "" {
				return invalid
			}
			if (ref.SessionKey == "") == (ref.ProjectPath == "") || ref.ProjectPath != "" && !contextSafeText(ref.ProjectPath, 4096) {
				return invalid
			}
			if !contextSafeText(ref.Path, 1024) || strings.ContainsAny(ref.Path, "\\:*?\"<>|") ||
				ref.MessageID != "" || ref.StartOffset != nil || ref.EndOffset != nil || ref.StartLine == nil || ref.EndLine == nil ||
				*ref.StartLine < 1 || *ref.EndLine < *ref.StartLine || *ref.EndLine > 9007199254740991 || *ref.EndLine-*ref.StartLine+1 != int64(strings.Count(ref.Text, "\n")+1) {
				return invalid
			}
			for _, segment := range strings.Split(ref.Path, "/") {
				if segment == "" || segment == "." || segment == ".." || strings.TrimSpace(segment) != segment {
					return invalid
				}
			}
		case "message":
			if ref.SessionKey == "" || ref.ProjectPath != "" || ref.Side != "" {
				return invalid
			}
			if ref.Annotation != nil || ref.TextBasis != "" || ref.BlockIndex != nil || ref.SourceRevision != "" {
				if !contextSourceRevision.MatchString(ref.SourceRevision) {
					return invalid
				}
				switch ref.TextBasis {
				case "source":
					if ref.BlockIndex != nil {
						return invalid
					}
				case "rendered":
					if ref.BlockIndex == nil || *ref.BlockIndex < 0 || *ref.BlockIndex > 100000 {
						return invalid
					}
				default:
					return invalid
				}
			}
			if !contextSafeText(ref.MessageID, 256) ||
				ref.Path != "" || ref.StartLine != nil || ref.EndLine != nil || ref.StartOffset == nil || ref.EndOffset == nil ||
				*ref.StartOffset < 0 || *ref.EndOffset < *ref.StartOffset || *ref.EndOffset > 9007199254740991 ||
				*ref.EndOffset-*ref.StartOffset != int64(len(utf16.Encode([]rune(ref.Text)))) {
				return invalid
			}
		default:
			return invalid
		}
	}
	return nil
}

func contextSnapshotText(text string, max int) bool {
	return len(text) <= max && utf8.ValidString(text) && strings.IndexFunc(text, func(r rune) bool {
		return r < 32 && r != '\t' && r != '\n' && r != '\r' || r == 127
	}) < 0
}

func HasContextAnnotations(refs []ContextReference) bool {
	for _, ref := range refs {
		if ref.Kind == "message" && (ref.Annotation != nil || ref.TextBasis != "" || ref.BlockIndex != nil || ref.SourceRevision != "") {
			return true
		}
	}
	return false
}

func HasDiffAnnotations(refs []ContextReference) bool {
	for _, ref := range refs {
		if ref.Kind == "diff" {
			return true
		}
	}
	return false
}

func CloneContextReferences(refs []ContextReference) []ContextReference {
	if len(refs) == 0 {
		return nil
	}
	out := append([]ContextReference(nil), refs...)
	for i := range out {
		if out[i].Annotation != nil {
			annotation := *out[i].Annotation
			out[i].Annotation = &annotation
		}
		for _, p := range []**int64{&out[i].StartLine, &out[i].EndLine, &out[i].StartOffset, &out[i].EndOffset, &out[i].BlockIndex} {
			if *p != nil {
				value := **p
				*p = &value
			}
		}
	}
	return out
}
