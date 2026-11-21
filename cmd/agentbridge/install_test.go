package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallUninstallAndDoctor(t *testing.T) {
	abHome := filepath.Join(t.TempDir(), "ab-home")
	pluginDir := filepath.Join(t.TempDir(), "plugin")
	codexHome := filepath.Join(t.TempDir(), "codex-home")
	fakeBin := filepath.Join(t.TempDir(), "fake-agentbridge")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed a legacy sidecar folder to verify install cleans it up.
	legacySidecar := filepath.Join(pluginDir, "sidecars", "bridge")
	if err := os.MkdirAll(legacySidecar, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacySidecar, "sidecar.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("AGENTBRIDGE_HOME", abHome)
	t.Setenv("CODEX_HOME", codexHome)

	var out, stderr bytes.Buffer
	if err := installCmd([]string{
		"--plugin-dir", pluginDir,
		"--codex-home", codexHome,
		"--binary", fakeBin,
		"--skip-codex-mcp",
	}, &out, &stderr); err != nil {
		t.Fatalf("install failed: %v (stderr=%s)", err, stderr.String())
	}

	for _, rel := range []string{
		filepath.Join(pluginDir, "bin", "agentbridge"),
		filepath.Join(pluginDir, "plugin.json"),
		filepath.Join(pluginDir, "hooks.json"),
		filepath.Join(pluginDir, "mcp_config.json"),
		filepath.Join(pluginDir, "skills", "codex", "SKILL.md"),
		filepath.Join(codexHome, "skills", "agy", "SKILL.md"),
	} {
		if _, err := os.Stat(rel); err != nil {
			t.Fatalf("expected installed file %s: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(pluginDir, "sidecars")); !os.IsNotExist(err) {
		t.Fatalf("legacy sidecars dir was not removed: %v", err)
	}
	if !strings.Contains(out.String(), abHome) {
		t.Fatalf("install output missing AGENTBRIDGE_HOME %q: %s", abHome, out.String())
	}

	// Doctor against a non-listening endpoint reports FAIL for Daemon.
	out.Reset()
	if err := doctorCmd([]string{"--plugin-dir", pluginDir, "--codex-home", codexHome}, &out, &stderr, "http://127.0.0.1:1"); err != nil {
		t.Fatalf("doctor failed: %v", err)
	}
	report := out.String()
	if !strings.Contains(report, "[OK]   Plugin binary") || !strings.Contains(report, "[OK]   Codex $agy skill") || !strings.Contains(report, "[FAIL] Daemon") {
		t.Fatalf("unexpected doctor report:\n%s", report)
	}

	// Uninstall removes plugin and skill, preserves AGENTBRIDGE_HOME unless --purge.
	if err := os.MkdirAll(abHome, 0o700); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := uninstallCmd([]string{"--plugin-dir", pluginDir, "--codex-home", codexHome}, &out, &stderr); err != nil {
		t.Fatalf("uninstall failed: %v", err)
	}
	if _, err := os.Stat(pluginDir); !os.IsNotExist(err) {
		t.Fatalf("plugin dir still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(codexHome, "skills", "agy")); !os.IsNotExist(err) {
		t.Fatalf("codex skill still present: %v", err)
	}
	if _, err := os.Stat(abHome); err != nil {
		t.Fatalf("uninstall without --purge removed AGENTBRIDGE_HOME: %v", err)
	}
}
