package main

import (
	"errors"
	"strings"
)

var errCodexSafetyBufferingVisible = errors.New("Codex safety-buffering 等待界面暂时遮住 runtime footer")

// terminalShowsCodexSafetyBuffering intentionally requires the whole chooser,
// not one generic sentence. The terminal transcript can contain task prose that
// mentions waiting or models; Esc is sent only when the current detection view
// carries every stable control in Codex's safety-buffering surface.
func terminalShowsCodexSafetyBuffering(raw []byte) bool {
	text := strings.Join(strings.Fields(strings.ReplaceAll(string(raw), "\r", "")), " ")
	const headline = "Our systems are thinking a bit more about this request before responding."
	index := strings.LastIndex(text, headline)
	if index < 0 {
		return false
	}
	tail := text[index:]
	for _, marker := range []string{
		"1. Retry with a faster model",
		"2. Dismiss and keep waiting",
		"3. Learn more",
		"No action is required. Codex will keep waiting",
	} {
		if !strings.Contains(tail, marker) {
			return false
		}
	}
	return true
}
