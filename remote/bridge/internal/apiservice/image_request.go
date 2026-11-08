package apiservice

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type imageRequest struct {
	model  string
	stream bool
	body   []byte
}

func imagePath(path string) bool {
	return path == "v1/images/generations" || path == "v1/images/edits"
}

// Normalize only declared image fields. Multipart is read from the already
// bounded upload in memory; no file path is opened and no reference URL fetched.
func parseImageRequest(raw []byte, contentType, path string, official bool) (imageRequest, int) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !imagePath(path) {
		return imageRequest{}, 400
	}
	if mediaType == "multipart/form-data" {
		if path != "v1/images/edits" || params["boundary"] == "" {
			return imageRequest{}, 400
		}
		raw, err = normalizeImageMultipart(raw, params["boundary"])
		if err != nil {
			return imageRequest{}, 400
		}
	} else if mediaType != "application/json" {
		return imageRequest{}, 415
	}
	if !imageUniqueJSON(raw) {
		return imageRequest{}, 400
	}
	var v map[string]json.RawMessage
	if json.Unmarshal(raw, &v) != nil || v == nil {
		return imageRequest{}, 400
	}
	allowed := map[string]bool{}
	for _, key := range []string{"model", "prompt", "stream", "n", "size", "quality", "background", "output_format", "output_compression", "partial_images", "moderation", "response_format", "input_fidelity", "images", "mask", "user"} {
		allowed[key] = true
	}
	for key := range v {
		if !allowed[key] {
			return imageRequest{}, 501
		}
	}
	var out imageRequest
	var prompt string
	if !imageString(v["model"], &out.model) || strings.TrimSpace(out.model) != out.model || out.model == "" || len(out.model) > 200 || !imageString(v["prompt"], &prompt) || strings.TrimSpace(prompt) == "" || len(prompt) > 256<<10 {
		return out, 400
	}
	for _, field := range []string{"size", "quality", "background", "output_format", "moderation", "response_format", "input_fidelity", "user"} {
		if value, ok := v[field]; ok {
			var text string
			if !imageString(value, &text) || text == "" || len(text) > 512 {
				return out, 400
			}
		}
	}
	if raw, ok := v["stream"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &out.stream) != nil {
			return out, 400
		}
	}
	for _, bound := range []struct {
		key      string
		min, max int
	}{{"n", 1, 10}, {"output_compression", 0, 100}, {"partial_images", 0, 3}} {
		if raw, ok := v[bound.key]; ok {
			var value int
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil || value < bound.min || value > bound.max {
				return out, 400
			}
		}
	}
	if raw, ok := v["response_format"]; ok {
		var value string
		_ = json.Unmarshal(raw, &value)
		if value != "url" && value != "b64_json" {
			return out, 400
		}
	}
	if path == "v1/images/generations" {
		if v["images"] != nil || v["mask"] != nil || v["input_fidelity"] != nil {
			return out, 400
		}
	} else {
		var images []map[string]json.RawMessage
		if json.Unmarshal(v["images"], &images) != nil || len(images) < 1 || len(images) > 16 {
			return out, 400
		}
		for _, image := range images {
			if !validImageReference(image) {
				return out, 400
			}
		}
		if mask, ok := v["mask"]; ok {
			var value map[string]json.RawMessage
			if json.Unmarshal(mask, &value) != nil || !validImageReference(value) {
				return out, 400
			}
		}
	}
	if official {
		switch out.model {
		case "gpt-image-1.5", "gpt-image-2", "gpt-image-2.5", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst":
			// These exact IDs select the SDK's direct image endpoint. Do not
			// permit a suffix or slash variant to execute a different model.
		default:
			return out, 501
		}
	}
	out.body = raw
	return out, 0
}

func imageString(raw []byte, dst *string) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && json.Unmarshal(raw, dst) == nil
}

func validImageReference(v map[string]json.RawMessage) bool {
	var text string
	if len(v) != 1 || !imageString(v["image_url"], &text) || len(text) > 24<<20 {
		return false
	}
	if strings.HasPrefix(text, "data:") {
		parts := strings.SplitN(text, ",", 2)
		if len(parts) != 2 || !strings.HasSuffix(parts[0], ";base64") {
			return false
		}
		kind := strings.TrimSuffix(strings.TrimPrefix(parts[0], "data:"), ";base64")
		decoded, err := base64.StdEncoding.DecodeString(parts[1])
		return err == nil && len(decoded) > 0 && len(decoded) <= 16<<20 && supportedImageMIME(kind) && http.DetectContentType(decoded) == kind
	}
	u, err := url.Parse(text)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == "" && !strings.ContainsAny(text, "\r\n\x00")
}

func supportedImageMIME(kind string) bool {
	return kind == "image/png" || kind == "image/jpeg" || kind == "image/webp"
}

func normalizeImageMultipart(raw []byte, boundary string) ([]byte, error) {
	v := map[string]any{}
	images := []map[string]string{}
	reader := multipart.NewReader(bytes.NewReader(raw), boundary)
	imageField := ""
	for count := 0; ; count++ {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil || count >= 40 {
			return nil, io.ErrUnexpectedEOF
		}
		name := part.FormName()
		data, readErr := io.ReadAll(io.LimitReader(part, (16<<20)+1))
		_ = part.Close()
		if readErr != nil || len(data) > 16<<20 || name == "" {
			return nil, io.ErrUnexpectedEOF
		}
		if part.FileName() != "" {
			if name != "image" && name != "image[]" && name != "mask" {
				return nil, io.ErrUnexpectedEOF
			}
			kind := http.DetectContentType(data)
			if len(data) == 0 || !supportedImageMIME(kind) {
				return nil, io.ErrUnexpectedEOF
			}
			ref := map[string]string{"image_url": "data:" + kind + ";base64," + base64.StdEncoding.EncodeToString(data)}
			if name == "mask" {
				if v[name] != nil {
					return nil, io.ErrUnexpectedEOF
				}
				v[name] = ref
			} else {
				if imageField != "" && imageField != name || len(images) >= 16 {
					return nil, io.ErrUnexpectedEOF
				}
				imageField = name
				images = append(images, ref)
			}
			continue
		}
		if name == "image" || name == "image[]" || name == "images" || name == "mask" || v[name] != nil || len(data) > 256<<10 {
			return nil, io.ErrUnexpectedEOF
		}
		switch name {
		case "stream":
			parsed, e := strconv.ParseBool(string(data))
			if e != nil || string(data) != "true" && string(data) != "false" {
				return nil, io.ErrUnexpectedEOF
			}
			v[name] = parsed
		case "n", "output_compression", "partial_images":
			parsed, e := strconv.Atoi(string(data))
			if e != nil {
				return nil, io.ErrUnexpectedEOF
			}
			v[name] = parsed
		default:
			v[name] = string(data)
		}
	}
	v["images"] = images
	return json.Marshal(v)
}

func imageUniqueJSON(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	budget := 65536
	var walk func(int) bool
	walk = func(depth int) bool {
		budget--
		if depth > 32 || budget < 0 {
			return false
		}
		t, err := d.Token()
		if err != nil {
			return false
		}
		x, container := t.(json.Delim)
		if !container {
			return depth != 0
		}
		switch x {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				t, e := d.Token()
				key, ok := t.(string)
				if e != nil || !ok || seen[key] {
					return false
				}
				seen[key] = true
				if !walk(depth + 1) {
					return false
				}
			}
			end, e := d.Token()
			return e == nil && end == json.Delim('}')
		case '[':
			if depth == 0 {
				return false
			}
			for d.More() {
				if !walk(depth + 1) {
					return false
				}
			}
			end, e := d.Token()
			return e == nil && end == json.Delim(']')
		}
		return false
	}
	if !walk(0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}
