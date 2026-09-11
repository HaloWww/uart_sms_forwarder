package main

import (
	"os"
	"path/filepath"
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
