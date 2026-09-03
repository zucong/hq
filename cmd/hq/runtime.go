package main

import (
	"fmt"
	"os"
	"path/filepath"
)

type runtimePaths struct {
	Office     string
	HQRoot     string
	DataDir    string
	ConfigPath string
	HerdrBin   string
}

func resolveProductionRuntime(options globalOptions) (runtimePaths, error) {
	paths, err := resolveProductionPaths(options)
	if err != nil {
		return runtimePaths{}, err
	}
	paths.HerdrBin, err = resolveHerdrExecutable(paths.HerdrBin)
	if err != nil {
		return runtimePaths{}, err
	}
	return paths, nil
}

// resolveProductionPaths validates the fixed company-instance filesystem contract
// without resolving optional command dependencies. Dependency-free read-only
// commands use it so a missing Herdr installation cannot block config/ledger
// inspection before those commands reach their own logic.
func resolveProductionPaths(options globalOptions) (runtimePaths, error) {
	if options.Data != "" || options.Config != "" || options.Herdr != "" {
		return runtimePaths{}, fmt.Errorf("正式实例命令不接受 --data/--config/--herdr 覆盖；测试必须显式构造并注入 fake 依赖")
	}
	office, err := discoverOffice(options.Office)
	if err != nil {
		return runtimePaths{}, err
	}
	office, err = canonicalExistingDirectory(office, "ceo-office")
	if err != nil {
		return runtimePaths{}, err
	}
	paths := runtimePaths{
		Office:     office,
		HQRoot:     filepath.Dir(office),
		DataDir:    filepath.Join(office, "records"),
		ConfigPath: defaultConfigPath(office),
		HerdrBin:   "herdr",
	}
	if err := validateProductionRuntime(paths); err != nil {
		return runtimePaths{}, err
	}
	return paths, nil
}

func validateProductionRuntime(paths runtimePaths) error {
	office, err := canonicalExistingDirectory(paths.Office, "ceo-office")
	if err != nil {
		return err
	}
	if office != paths.Office || paths.HQRoot != filepath.Dir(office) {
		return fmt.Errorf("ceo-office/HQRoot 不是固定 canonical 根")
	}
	expectedConfig := defaultConfigPath(office)
	if paths.ConfigPath != expectedConfig {
		return fmt.Errorf("config 必须固定为 %s", expectedConfig)
	}
	if _, err := canonicalExistingRegularFile(expectedConfig, "HQ config"); err != nil {
		return err
	}
	expectedData := filepath.Join(office, "records")
	if paths.DataDir != expectedData {
		return fmt.Errorf("data 必须固定为 %s", expectedData)
	}
	if info, err := os.Lstat(expectedData); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("HQ data 必须是非 symlink 普通目录：%s", expectedData)
		}
		canonical, err := filepath.EvalSymlinks(expectedData)
		if err != nil || canonical != expectedData {
			return fmt.Errorf("HQ data 必须是 canonical 非 symlink 目录：%s", expectedData)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("检查 HQ data：%w", err)
	}
	return nil
}

func defaultConfigPath(office string) string {
	return filepath.Join(office, "tools", "hq", "config.yaml")
}

func canonicalExistingDirectory(path, label string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	info, err := os.Lstat(abs)
	if err != nil {
		return "", fmt.Errorf("%s 不可用：%w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("%s 必须是非 symlink 普通目录：%s", label, abs)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("canonicalize %s：%w", label, err)
	}
	if canonical != abs {
		return "", fmt.Errorf("%s 路径含 symlink 或不是 canonical 路径：%s", label, abs)
	}
	return abs, nil
}

func canonicalExistingRegularFile(path, label string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	info, err := os.Lstat(abs)
	if err != nil {
		return "", fmt.Errorf("%s 不可用：%w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s 必须是非 symlink 普通文件：%s", label, abs)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("canonicalize %s：%w", label, err)
	}
	if canonical != abs {
		return "", fmt.Errorf("%s 路径含 symlink 或不是 canonical 路径：%s", label, abs)
	}
	return abs, nil
}
