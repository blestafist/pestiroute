package antigravity

import (
	"bytes"
	"encoding/json"
	"io"
)

const maxRequestBytes = 1 << 20

func uniqueJSON(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if !jsonValue(d) {
		return false
	}
	return d.Decode(new(any)) == io.EOF
}

func jsonValue(d *json.Decoder) bool {
	t, err := d.Token()
	if err != nil {
		return false
	}
	switch t {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return false
			}
			key, ok := k.(string)
			if !ok || seen[key] {
				return false
			}
			seen[key] = true
			if !jsonValue(d) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim('}')
	case json.Delim('['):
		for d.More() {
			if !jsonValue(d) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim(']')
	case json.Delim('}'), json.Delim(']'):
		return false
	default:
		return true
	}
}
