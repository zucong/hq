package main

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
)

// Release builds override these two variables with -ldflags. Development
// builds deliberately remain explicit and reproducible: no wall-clock field is
// embedded or inferred.
var (
	buildVersion = "dev"
	buildCommit  = "unknown"
)

type versionInfo struct {
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	Go       string `json:"go"`
	Platform string `json:"platform"`
}

func currentVersionInfo() versionInfo {
	version := strings.TrimSpace(buildVersion)
	if version == "" {
		version = "dev"
	}
	commit := strings.TrimSpace(buildCommit)
	if commit == "" {
		commit = "unknown"
	}
	return versionInfo{Version: version, Commit: commit, Go: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH}
}

func (a *App) cmdVersion(args []string) error {
	if len(args) != 0 {
		return usagef("不接受位置参数\n用法：hq version [--json]")
	}
	info := currentVersionInfo()
	if a.JSON {
		return json.NewEncoder(a.Out).Encode(info)
	}
	_, err := fmt.Fprintf(a.Out, "hq %s\ncommit %s\ngo %s\nplatform %s\n", info.Version, info.Commit, info.Go, info.Platform)
	return err
}
