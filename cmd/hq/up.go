package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

func (a *App) cmdUp(args []string) error {
	return a.runUp(args)
}

func paneIDFromJSON(raw json.RawMessage) (string, error) {
	var pane struct {
		ID string `json:"pane_id"`
	}
	if err := json.Unmarshal(raw, &pane); err == nil && pane.ID != "" {
		return pane.ID, nil
	}
	var id string
	if err := json.Unmarshal(raw, &id); err == nil && id != "" {
		return id, nil
	}
	return "", fmt.Errorf("herdr 未返回 root_pane.pane_id")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
