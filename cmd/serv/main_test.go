package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestPrepareRuntimeDirectoriesCreatesSQLiteAndLogParents(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	configuration := "database:\n  enabled: true\n  type: sqlite\n  sqlite:\n    path: ./data/app.db\nlog:\n  filename: ./logs/app.log\n"
	if err := os.WriteFile(configPath, []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareRuntimeDirectories(configPath); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"data", "logs"} {
		info, err := os.Stat(filepath.Join(directory, name))
		if err != nil || !info.IsDir() {
			t.Fatalf("missing runtime directory %s: %v", name, err)
		}
	}
	// Exercise the reported SQLite failure, rather than only checking mkdir.
	databasePath := filepath.Join(directory, "data", "app.db")
	db, err := gorm.Open(sqlite.Open(databasePath), &gorm.Config{})
	if err != nil {
		t.Fatalf("SQLite still cannot open after preparation: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if _, err := sqlDB.Exec("CREATE TABLE startup_marker (value TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec("INSERT INTO startup_marker VALUES ('preserve')"); err != nil {
		t.Fatal(err)
	}
	if err := prepareRuntimeDirectories(configPath); err != nil {
		t.Fatal(err)
	}
	var value string
	if err := sqlDB.QueryRow("SELECT value FROM startup_marker").Scan(&value); err != nil || value != "preserve" {
		t.Fatalf("repeat preparation changed existing database: value=%q err=%v", value, err)
	}
}

func TestPrepareRuntimeDirectoriesHonorsCustomPathsAndURLPrecedence(t *testing.T) {
	directory := t.TempDir()
	absoluteLog := filepath.Join(t.TempDir(), "nested", "app.log")
	configPath := filepath.Join(directory, "config.yaml")
	configuration := fmt.Sprintf("database:\n  enabled: true\n  type: sqlite\n  url: file:custom%%20state/nested/app.db?cache=shared\n  sqlite:\n    path: ./unused/app.db\nlog:\n  filename: '%s'\n", absoluteLog)
	if err := os.WriteFile(configPath, []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareRuntimeDirectories(configPath); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{filepath.Join(directory, "custom state", "nested"), filepath.Dir(absoluteLog)} {
		if info, err := os.Stat(expected); err != nil || !info.IsDir() {
			t.Fatalf("custom directory not prepared %q: %v", expected, err)
		}
	}
	if _, err := os.Stat(filepath.Join(directory, "unused")); !os.IsNotExist(err) {
		t.Fatalf("ignored sqlite.path was created: %v", err)
	}
}

func TestPrepareRuntimeDirectoriesSkipsNonFileDatabases(t *testing.T) {
	for name, database := range map[string]string{
		"disabled":      "enabled: false\n  type: sqlite\n  sqlite:\n    path: ./unused/app.db",
		"mysql":         "enabled: true\n  type: mysql\n  sqlite:\n    path: ./unused/app.db",
		"memory":        "enabled: true\n  type: sqlite\n  url: ':memory:'",
		"memory_uri":    "enabled: true\n  type: sqlite\n  url: 'file:unused/app.db?mode=memory&cache=shared'",
		"read_only_uri": "enabled: true\n  type: sqlite\n  url: 'file:unused/app.db?mode=ro'",
	} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			configPath := filepath.Join(directory, "config.yaml")
			if err := os.WriteFile(configPath, []byte("database:\n  "+database+"\nlog:\n  filename: ''\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := prepareRuntimeDirectories(configPath); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 1 {
				t.Fatalf("unexpected runtime directory for non-file database: %v %v", entries, err)
			}
		})
	}
}

func TestPrepareRuntimeDirectoriesReportsBlockedPath(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "blocked"), []byte("existing file"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(configPath, []byte("database:\n  enabled: true\n  type: sqlite\n  sqlite:\n    path: ./blocked/data/app.db\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := prepareRuntimeDirectories(configPath)
	if err == nil || !strings.Contains(err.Error(), "SQLite database directory") || !strings.Contains(err.Error(), "write permissions") {
		t.Fatalf("expected actionable directory error, got %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(directory, "blocked")); err != nil || string(data) != "existing file" {
		t.Fatalf("existing file changed: %q %v", data, err)
	}
}

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
