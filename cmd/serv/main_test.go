package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPrepareConfigPathChangesToConfigDirectory(t *testing.T) {
	originalDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(originalDirectory); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	}()

	directory := t.TempDir()
	configPath := filepath.Join(directory, "custom.yaml")
	if err := os.WriteFile(configPath, []byte("App: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := prepareConfigPath([]string{"-config", configPath})
	if err != nil {
		t.Fatal(err)
	}
	if got != "custom.yaml" {
		t.Fatalf("config path = %q, want custom.yaml", got)
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if workingDirectory != directory {
		t.Fatalf("working directory = %q, want %q", workingDirectory, directory)
	}
}

func TestPrepareConfigPathRejectsMissingFile(t *testing.T) {
	_, err := prepareConfigPath([]string{"-config", filepath.Join(t.TempDir(), "missing.yaml")})
	if err == nil {
		t.Fatal("expected a missing config error")
	}
}

func TestParseCommandOptionsInstallService(t *testing.T) {
	options, err := parseCommandOptions([]string{"-install-service", "-config", "/opt/uart sms/config.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if !options.installService {
		t.Fatal("install-service was not parsed")
	}
	if options.configPath != "/opt/uart sms/config.yaml" {
		t.Fatalf("config path = %q", options.configPath)
	}
}

func TestBuildSystemdUnit(t *testing.T) {
	unit, err := buildSystemdUnit(`/opt/uart sms/uart_sms_forwarder`, `/opt/uart sms/config.yaml`)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`WorkingDirectory=/opt/uart sms`,
		`ExecStart="/opt/uart sms/uart_sms_forwarder" -config "/opt/uart sms/config.yaml"`,
		`Restart=always`,
		`WantedBy=multi-user.target`,
	} {
		if !strings.Contains(unit, expected) {
			t.Errorf("unit does not contain %q:\n%s", expected, unit)
		}
	}
}

func TestBuildSystemdUnitEscapesSpecifiers(t *testing.T) {
	unit, err := buildSystemdUnit(`/opt/100%/$app`, `/opt/100%/$config/config.yaml`)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`WorkingDirectory=/opt/100%%/$config`,
		`ExecStart="/opt/100%%/$$app" -config "/opt/100%%/$$config/config.yaml"`,
	} {
		if !strings.Contains(unit, expected) {
			t.Errorf("unit does not contain %q:\n%s", expected, unit)
		}
	}
}

func TestBuildSystemdUnitRejectsNewline(t *testing.T) {
	if _, err := buildSystemdUnit("/opt/app\ninvalid", "/opt/config.yaml"); err == nil {
		t.Fatal("expected newline path to be rejected")
	}
}

func TestBuildSystemdUnitPassesSystemdAnalyze(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd-analyze verification only runs on Linux")
	}
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		t.Skip("systemd-analyze is not installed")
	}
	unit, err := buildSystemdUnit("/bin/true", "/tmp/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(t.TempDir(), "uart_sms_forwarder-test.service")
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("systemd-analyze", "verify", unitPath).CombinedOutput(); err != nil {
		t.Fatalf("systemd unit verification failed: %v\n%s", err, output)
	}
}
