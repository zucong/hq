package main

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// validateStrictJSONDocument rejects duplicate object keys at every nesting
// level and requires exactly one JSON value. Typed callers add their own
// DisallowUnknownFields pass after this structural check.
func validateStrictJSONDocument(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := consumeStrictJSONValue(decoder); err != nil {
		return err
	}
	return ensureJSONEOF(decoder)
}

func consumeStrictJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("JSON object 字段名不是字符串")
			}
			if seen[key] {
				return fmt.Errorf("重复 JSON 字段 %q", key)
			}
			seen[key] = true
			if err := consumeStrictJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("JSON object 未正确结束")
		}
		return nil
	case '[':
		for decoder.More() {
			if err := consumeStrictJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("JSON array 未正确结束")
		}
		return nil
	default:
		return fmt.Errorf("JSON 起始分隔符无效：%q", delim)
	}
}

func decodeStrictJSON(raw []byte, destination any) error {
	if err := validateStrictJSONDocument(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	return ensureJSONEOF(decoder)
}

func requireJSONFields(raw []byte, fields ...string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return err
	}
	for _, field := range fields {
		if _, ok := object[field]; !ok {
			return fmt.Errorf("缺少必填 JSON 字段 %q", field)
		}
	}
	return nil
}
