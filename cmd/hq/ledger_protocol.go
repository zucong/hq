package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const genesisEventHash = "0000000000000000000000000000000000000000000000000000000000000000"

var (
	ledgerIDPattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,199}$`)
	sha256Pattern          = regexp.MustCompile(`^[0-9a-f]{64}$`)
	monthlyFilenamePattern = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])\.jsonl$`)
)

func canonicalEventBytes(event Event) ([]byte, error) {
	raw, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	delete(object, "event_hash")
	return json.Marshal(object)
}

func hashEvent(event Event) (string, error) {
	canonical, err := canonicalEventBytes(event)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func digestText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func stableCommandID(kind string, parts ...string) string {
	material := strings.Join(append([]string{kind}, parts...), "\x00")
	sum := sha256.Sum256([]byte(material))
	return kind + ":" + hex.EncodeToString(sum[:16])
}

func stableDeliveryID(commandID, target string) string {
	sum := sha256.Sum256([]byte(commandID + "\x00" + target))
	return "delivery:" + hex.EncodeToString(sum[:16])
}

func validateLedgerID(label, value string) error {
	if !ledgerIDPattern.MatchString(value) {
		return fmt.Errorf("%s 必须是 1-200 字符的稳定 ASCII 标识", label)
	}
	return nil
}

func validateDigest(label, value string) error {
	if !sha256Pattern.MatchString(value) {
		return fmt.Errorf("%s 必须是小写 SHA-256", label)
	}
	return nil
}

func decodeStrictEvent(raw []byte, event *Event) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return fmt.Errorf("事件必须是单个 JSON object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return fmt.Errorf("事件字段名不是字符串")
		}
		if seen[key] {
			return fmt.Errorf("重复 JSON 字段 %q", key)
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return err
	}

	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err := strict.Decode(event); err != nil {
		return err
	}
	return ensureJSONEOF(strict)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("JSON 后存在第二个值")
	}
	return err
}
