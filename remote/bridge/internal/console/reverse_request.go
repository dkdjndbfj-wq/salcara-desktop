package console

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
)

// Reverse-account controls use exact field names and one bounded document.
// encoding/json alone accepts duplicate and case-confusable struct fields.
func readReverseJSON(r *http.Request, dst any, limit int64) error {
	errInvalid := errors.New("请求格式有误")
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || len(raw) == 0 || int64(len(raw)) > limit {
		return errInvalid
	}
	t := reflect.TypeOf(dst)
	if t == nil || t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct {
		return errInvalid
	}
	t = t.Elem()
	allowed := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		name := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			allowed[name] = true
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	budget := 32768
	if reverseJSONValue(dec, 0, &budget, allowed) != nil {
		return errInvalid
	}
	if _, err = dec.Token(); err != io.EOF {
		return errInvalid
	}
	if json.Unmarshal(raw, dst) != nil {
		return errInvalid
	}
	return nil
}

func reverseJSONValue(dec *json.Decoder, depth int, budget *int, allowed map[string]bool) error {
	invalid := errors.New("invalid reverse control JSON")
	if depth > 32 || *budget <= 0 {
		return invalid
	}
	*budget--
	token, err := dec.Token()
	if err != nil || token == nil && depth == 1 {
		return invalid
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		if depth == 0 {
			return invalid
		}
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			keyToken, err := dec.Token()
			key, ok := keyToken.(string)
			if err != nil || !ok || seen[key] || depth == 0 && !allowed[key] {
				return invalid
			}
			seen[key] = true
			if reverseJSONValue(dec, depth+1, budget, nil) != nil {
				return invalid
			}
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim('}') {
			return invalid
		}
	case '[':
		if depth == 0 {
			return invalid
		}
		for dec.More() {
			if reverseJSONValue(dec, depth+1, budget, nil) != nil {
				return invalid
			}
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim(']') {
			return invalid
		}
	default:
		return invalid
	}
	return nil
}
