package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"gopkg.in/yaml.v3"
)

type configFileMetadata struct {
	mode os.FileMode
	uid  int
	gid  int
}

type configWriteOptions struct {
	context        context.Context
	dryRun         bool
	failpoint      func(string) error
	authorize      func([]byte, *Config) error
	candidateGuard func(*Config) (func(), error)
}

// configMutationProcessMu complements the advisory filesystem lock below.
// BSD flock locks are process-associated on some supported platforms, so two
// goroutines in one gateway process must not rely on separate directory file
// descriptors to serialize config replacement.
var configMutationProcessMu sync.RWMutex

func loadConfig(path string) (Config, error) {
	raw, err := readConfigFile(path)
	if err != nil {
		return Config{}, err
	}
	if err := requireYAMLConfigPath(path); err != nil {
		return Config{}, err
	}
	cfg, err := decodeCurrentConfig(raw)
	if err != nil {
		return Config{}, fmt.Errorf("解析配置 %s：%w", path, err)
	}
	return cfg, nil
}

func requireYAMLConfigPath(path string) error {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".yaml" && ext != ".yml" {
		return fmt.Errorf("HQ 注册表必须使用 .yaml 或 .yml，拒绝 JSON 合同：%s", path)
	}
	return nil
}

func readConfigFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s：%w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("读取配置 %s：必须是非 symlink 普通文件", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s：%w", path, err)
	}
	return raw, nil
}

func decodeCurrentConfig(raw []byte) (Config, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return Config{}, fmt.Errorf("YAML 注册表为空")
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		return Config{}, fmt.Errorf("HQ 注册表不得使用 JSON 文档")
	}
	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("严格 YAML 解码失败：%w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Config{}, fmt.Errorf("YAML 注册表只能包含一个文档")
		}
		return Config{}, fmt.Errorf("读取 YAML 尾部：%w", err)
	}
	if cfg.Version != registrySchemaVersion {
		return Config{}, fmt.Errorf("不支持的 YAML 注册表版本 %d；当前版本=%d", cfg.Version, registrySchemaVersion)
	}
	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func mutateConfig(path string, mutate func(*Config) error) (Config, error) {
	return mutateConfigWithOptions(path, configWriteOptions{}, mutate)
}

func mutateConfigWithOptions(path string, options configWriteOptions, mutate func(*Config) error) (Config, error) {
	ctx := nonNilContext(options.context)
	if err := lockRWMutexContext(ctx, &configMutationProcessMu); err != nil {
		return Config{}, fmt.Errorf("等待 config mutation process lease：%w", err)
	}
	defer configMutationProcessMu.Unlock()

	dir := filepath.Dir(path)
	// The config parent already exists and is a controlled part of the runtime
	// root. Lock its open file description so rejected approvals and dry-runs do
	// not have to create a persistent sidecar before authorization succeeds.
	lock, err := os.Open(dir)
	if err != nil {
		return Config{}, err
	}
	defer func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}()
	if err := flockContext(ctx, int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return Config{}, err
	}

	raw, metadata, err := readConfigForWrite(path)
	if err != nil {
		return Config{}, err
	}
	if err := runConfigFailpoint(options.failpoint, "config_after_read"); err != nil {
		return Config{}, err
	}
	if err := requireYAMLConfigPath(path); err != nil {
		return Config{}, err
	}
	cfg, err := decodeCurrentConfig(raw)
	if err != nil {
		return Config{}, err
	}
	if err := runConfigFailpoint(options.failpoint, "config_after_decode"); err != nil {
		return Config{}, err
	}
	if mutate != nil {
		if err := mutate(&cfg); err != nil {
			return Config{}, err
		}
	}
	if options.authorize != nil {
		if err := options.authorize(raw, &cfg); err != nil {
			return Config{}, err
		}
	}
	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}
	if err := runConfigFailpoint(options.failpoint, "config_after_validate"); err != nil {
		return Config{}, err
	}
	if options.candidateGuard != nil {
		release, err := options.candidateGuard(&cfg)
		if err != nil {
			return Config{}, err
		}
		if release == nil {
			return Config{}, fmt.Errorf("候选配置 guard 未返回 release 函数")
		}
		defer release()
	}
	if options.dryRun {
		return cfg, nil
	}
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		return Config{}, err
	}
	backupPath, err := writeConfigBackup(dir, path, raw, metadata, options.failpoint)
	if err != nil {
		return Config{}, err
	}
	if err := replaceConfigFile(dir, path, encoded, raw, metadata, backupPath, options.failpoint); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func readConfigForWrite(path string) ([]byte, configFileMetadata, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, configFileMetadata{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, configFileMetadata{}, fmt.Errorf("配置必须是非 symlink 普通文件：%s", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, configFileMetadata{}, fmt.Errorf("无法读取配置 uid/gid：%s", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, configFileMetadata{}, err
	}
	return raw, configFileMetadata{mode: info.Mode().Perm(), uid: int(stat.Uid), gid: int(stat.Gid)}, nil
}

func writeConfigBackup(dir, path string, raw []byte, metadata configFileMetadata, failpoint func(string) error) (string, error) {
	sum := sha256.Sum256(raw)
	backupPath := path + ".bak." + hex.EncodeToString(sum[:8])
	if existing, err := os.ReadFile(backupPath); err == nil {
		if digestText(string(existing)) != digestText(string(raw)) {
			return "", fmt.Errorf("配置 backup digest 冲突：%s", backupPath)
		}
		return backupPath, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".hq-config-backup-*.tmp")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return "", err
	}
	if err := runConfigFailpoint(failpoint, "config_backup_write"); err != nil {
		tmp.Close()
		return "", err
	}
	if err := applyConfigMetadata(tmp, metadata); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := runConfigFailpoint(failpoint, "config_backup_fsync"); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, backupPath); err != nil {
		return "", err
	}
	if err := runConfigFailpoint(failpoint, "config_backup_rename"); err != nil {
		return "", err
	}
	if err := syncDirectory(dir); err != nil {
		return "", err
	}
	if err := runConfigFailpoint(failpoint, "config_backup_parent_fsync"); err != nil {
		return "", err
	}
	return backupPath, nil
}

func replaceConfigFile(dir, path string, encoded, oldRaw []byte, metadata configFileMetadata, backupPath string, failpoint func(string) error) error {
	tmp, err := os.CreateTemp(dir, ".hq-config-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(encoded); err != nil {
		tmp.Close()
		return err
	}
	if err := runConfigFailpoint(failpoint, "config_temp_write"); err != nil {
		tmp.Close()
		return err
	}
	if err := applyConfigMetadata(tmp, metadata); err != nil {
		tmp.Close()
		return err
	}
	if err := runConfigFailpoint(failpoint, "config_temp_metadata"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := runConfigFailpoint(failpoint, "config_temp_fsync"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	postRenameErr := runConfigFailpoint(failpoint, "config_rename")
	if postRenameErr == nil {
		postRenameErr = syncDirectory(dir)
	}
	if postRenameErr == nil {
		postRenameErr = runConfigFailpoint(failpoint, "config_parent_fsync")
	}
	if postRenameErr != nil {
		if restoreErr := restoreConfigFile(dir, path, oldRaw, metadata); restoreErr != nil {
			return fmt.Errorf("配置写入失败且从 backup %s 恢复失败：write=%v restore=%v", backupPath, postRenameErr, restoreErr)
		}
		return fmt.Errorf("配置写入失败，已从受控 backup 恢复旧配置：%w", postRenameErr)
	}
	return nil
}

func restoreConfigFile(dir, path string, raw []byte, metadata configFileMetadata) error {
	tmp, err := os.CreateTemp(dir, ".hq-config-restore-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := applyConfigMetadata(tmp, metadata); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func applyConfigMetadata(file *os.File, metadata configFileMetadata) error {
	if err := file.Chmod(metadata.mode); err != nil {
		return err
	}
	if err := file.Chown(metadata.uid, metadata.gid); err != nil {
		return err
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func runConfigFailpoint(failpoint func(string) error, name string) error {
	if failpoint == nil {
		return nil
	}
	if err := failpoint(name); err != nil {
		return fmt.Errorf("failpoint %s: %w", name, err)
	}
	return nil
}
