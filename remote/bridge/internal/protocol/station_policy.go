package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// UnmarshalJSON keeps a future or malformed policy from silently becoming a
// weaker older scope. Validate URLs and class boundaries in config as well.
func (p *StationPolicy) UnmarshalJSON(raw []byte) error {
	fail := errors.New("incompatible station policy")
	d := json.NewDecoder(bytes.NewReader(raw))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return fail
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return fail
		}
		name, ok := token.(string)
		if !ok || fields[name] != nil {
			return fail
		}
		switch name {
		case "version", "apiBases", "externalApiBases", "reverseProxyApiBases", "allowOfficialAccounts", "allowAllAccounts":
		default:
			return fail
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return fail
		}
		fields[name] = value
	}
	if last, err := d.Token(); err != nil || last != json.Delim('}') {
		return fail
	}
	if d.Decode(new(any)) != io.EOF {
		return fail
	}
	type plain StationPolicy
	var next plain
	if json.Unmarshal(raw, &next) != nil || (next.Version != 1 && next.Version != 2) {
		return fail
	}
	if next.Version == 1 && (fields["externalApiBases"] != nil || fields["reverseProxyApiBases"] != nil) || next.Version == 2 && fields["allowAllAccounts"] != nil {
		return fail
	}
	for name, value := range fields {
		// Older embedded Hubs encode an empty API scope as null. It grants
		// nothing and remains supported only for the existing v1 contract.
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) && !(next.Version == 1 && name == "apiBases") {
			return fail
		}
	}
	*p = StationPolicy(next)
	return nil
}
