package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeploymentContractUpHelpMakesSafeSyntheticPathDiscoverable(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := execute([]string{"up", "--help"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if errOut.Len() != 0 {
		t.Fatalf("up help wrote stderr: %s", errOut.String())
	}
	help := out.String()
	for _, want := range []string{
		"--config、--data、--herdr", "exit 70", "README", "fake Herdr",
		"--office 只选择", "canonical 校验", "不通过运行参数替换正式依赖",
		"首次连接 Herdr", "can_manage_staff",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("up help missing %q:\n%s", want, help)
		}
	}
}

func TestDeploymentContractREADMEPublishesOfficialFakeUpFixture(t *testing.T) {
	raw, err := os.ReadFile(repositoryPath("README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := string(raw)
	for _, want := range []string{
		"需要验证完整 `up` 编排时",
		"go test -count=1 -run '^TestRegistryPortabilityInitUpAndEnvelope$' -v ./...",
		"fake identity、fake", "fake Herdr", "不连接实际 workspace",
		"`--office` 是公司实例选择器", "不借命令行参数伪装正式实例",
	} {
		if !strings.Contains(readme, want) {
			t.Fatalf("README missing %q", want)
		}
	}
}

func TestDeploymentContractProductionUpOverridesStayRejectedAndOfficeStaysValid(t *testing.T) {
	office := productionOfficeFixture(t)
	for _, test := range []struct {
		name string
		args []string
	}{
		{"config", []string{"--config", filepath.Join(canonicalTestTempDir(t), "config.yaml")}},
		{"data", []string{"--data", filepath.Join(canonicalTestTempDir(t), "records")}},
		{"herdr", []string{"--herdr", filepath.Join(canonicalTestTempDir(t), "herdr")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"--office", office}, test.args...)
			args = append(args, "up")
			err := execute(args, &bytes.Buffer{}, &bytes.Buffer{})
			if err == nil || exitCodeForError(err) != exitInternal {
				t.Fatalf("production up override was not rejected with exit 70: err=%v code=%d", err, exitCodeForError(err))
			}
			if !strings.Contains(err.Error(), "--data/--config/--herdr") {
				t.Fatalf("production up override error lost frozen contract: %v", err)
			}
		})
	}
	if _, err := resolveProductionRuntime(globalOptions{Office: office}); err != nil {
		t.Fatalf("valid --office production root selector regressed: %v", err)
	}
}
