package gateway

import (
	"bytes"
	"encoding/json"
)

func stripDesktopOnlyOptions(body []byte) []byte {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil {
		return body
	}
	delete(object, "thinking")
	if raw := object["output_config"]; len(raw) > 0 {
		var options map[string]json.RawMessage
		if json.Unmarshal(raw, &options) == nil {
			delete(options, "effort")
			if len(options) == 0 {
				delete(object, "output_config")
			} else {
				object["output_config"], _ = json.Marshal(options)
			}
		}
	}
	data, err := json.Marshal(object)
	if err != nil {
		return body
	}
	return data
}

// Rewrite only protocol metadata, never tool inputs or arbitrary text.
func replaceModelJSON(data []byte, from, to string) []byte {
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil {
		return data
	}
	changed := false
	var model string
	if json.Unmarshal(object["model"], &model) == nil && model == from {
		object["model"], _ = json.Marshal(to)
		changed = true
	}
	if raw := object["message"]; len(raw) > 0 {
		updated := replaceModelJSON(raw, from, to)
		if !bytes.Equal(raw, updated) {
			object["message"] = updated
			changed = true
		}
	}
	if !changed {
		return data
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return data
	}
	return encoded
}

func replaceSSEModel(chunk []byte, from, to string) []byte {
	lines := bytes.Split(chunk, []byte("\n"))
	output := make([][]byte, 0, len(lines))
	flush := func(frame [][]byte) {
		data, indexes := [][]byte{}, []int{}
		for i, line := range frame {
			line = bytes.TrimSuffix(line, []byte("\r"))
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			value := bytes.TrimPrefix(line, []byte("data:"))
			value = bytes.TrimPrefix(value, []byte(" "))
			data, indexes = append(data, value), append(indexes, i)
		}
		original := bytes.Join(data, []byte("\n"))
		updated := replaceModelJSON(original, from, to)
		if len(indexes) == 0 || bytes.Equal(original, updated) {
			output = append(output, frame...)
			return
		}
		first := indexes[0]
		for i, line := range frame {
			if i == first {
				replacement := append([]byte("data: "), updated...)
				if bytes.HasSuffix(line, []byte("\r")) {
					replacement = append(replacement, '\r')
				}
				output = append(output, replacement)
			} else if !bytes.HasPrefix(line, []byte("data:")) {
				output = append(output, line)
			}
		}
	}
	start := 0
	for i, line := range lines {
		if len(bytes.TrimSuffix(line, []byte("\r"))) == 0 {
			flush(lines[start:i])
			output = append(output, line)
			start = i + 1
		}
	}
	if start < len(lines) {
		flush(lines[start:])
	}
	return bytes.Join(output, []byte("\n"))
}
