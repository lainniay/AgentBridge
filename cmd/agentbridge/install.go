package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lainniay/AgentBridge/internal/api"
	"github.com/lainniay/AgentBridge/internal/hub"
	"github.com/lainniay/AgentBridge/plugins"
)

func defaultPluginDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "config", "plugins", "agentbridge"), nil
}

func defaultCodexHome() (string, error) {
	if v := strings.TrimSpace(os.Getenv("CODEX_HOME")); v != "" {
		return filepath.Abs(v)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

func installCmd(args []string, out, stderr io.Writer) error {
	defPlugin, err := defaultPluginDir()
	if err != nil {
		return err
	}
	defCodex, err := defaultCodexHome()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	flags.SetOutput(stderr)
	pluginDir := flags.String("plugin-dir", defPlugin, "Antigravity plugin install directory")
	codexHome := flags.String("codex-home", defCodex, "Codex home directory (defaults to $CODEX_HOME or ~/.codex)")
	binaryPath := flags.String("binary", "", "source agentbridge binary (defaults to current executable)")
	skipCodexMCP := flags.Bool("skip-codex-mcp", false, "skip registering agentbridge MCP in Codex CLI")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected install arguments")
	}

	srcBin := *binaryPath
	if srcBin == "" {
		srcBin, err = os.Executable()
		if err != nil {
			return fmt.Errorf("locate executable: %w", err)
		}
	}
	srcBin, err = filepath.Abs(srcBin)
	if err != nil {
		return err
	}
	dstPlugin, err := filepath.Abs(*pluginDir)
	if err != nil {
		return err
	}
	dstCodex, err := filepath.Abs(*codexHome)
	if err != nil {
		return err
	}

	dstBin := filepath.Join(dstPlugin, "bin", "agentbridge")
	if err := copyBinaryAtomically(srcBin, dstBin); err != nil {
		return fmt.Errorf("install binary: %w", err)
	}
	if err := extractTree(plugins.Assets, "antigravity", dstPlugin); err != nil {
		return fmt.Errorf("install Antigravity plugin: %w", err)
	}
	// Remove legacy sidecar panel directory if upgrading from an older version.
	_ = os.RemoveAll(filepath.Join(dstPlugin, "sidecars"))

	codexSkillDst := filepath.Join(dstCodex, "skills", "agy", "SKILL.md")
	skillBytes, err := fs.ReadFile(plugins.Assets, "codex/skills/agy/SKILL.md")
	if err != nil {
		return fmt.Errorf("read embedded Codex skill: %w", err)
	}
	if err := writeFileAtomically(codexSkillDst, skillBytes, 0o644); err != nil {
		return fmt.Errorf("install Codex skill: %w", err)
	}

	mcpStatus := "skipped (--skip-codex-mcp)"
	if !*skipCodexMCP {
		mcpStatus = configureCodexMCP(dstCodex, dstBin)
	}

	abHome, _ := hub.HomeDir("")
	fmt.Fprintf(out, "Installed AgentBridge:\n")
	fmt.Fprintf(out, "  Binary:             %s\n", dstBin)
	fmt.Fprintf(out, "  Antigravity plugin: %s\n", dstPlugin)
	fmt.Fprintf(out, "  Codex skill ($agy): %s\n", codexSkillDst)
	fmt.Fprintf(out, "  Codex MCP:          %s\n", mcpStatus)
	fmt.Fprintf(out, "  AGENTBRIDGE_HOME:   %s\n", abHome)
	fmt.Fprintf(out, "\nStart daemon (or add to your own service manager):\n  %q daemon\n", dstBin)
	return nil
}

func uninstallCmd(args []string, out, stderr io.Writer) error {
	defPlugin, err := defaultPluginDir()
	if err != nil {
		return err
	}
	defCodex, err := defaultCodexHome()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	flags.SetOutput(stderr)
	pluginDir := flags.String("plugin-dir", defPlugin, "Antigravity plugin install directory")
	codexHome := flags.String("codex-home", defCodex, "Codex home directory")
	purge := flags.Bool("purge", false, "also delete AGENTBRIDGE_HOME state and blackboards")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected uninstall arguments")
	}

	dstPlugin, err := filepath.Abs(*pluginDir)
	if err != nil {
		return err
	}
	dstCodex, err := filepath.Abs(*codexHome)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dstPlugin); err != nil {
		return fmt.Errorf("remove plugin directory: %w", err)
	}
	if err := os.RemoveAll(filepath.Join(dstCodex, "skills", "agy")); err != nil {
		return fmt.Errorf("remove Codex skill: %w", err)
	}
	if codexBin, err := exec.LookPath("codex"); err == nil {
		cmd := exec.Command(codexBin, "mcp", "remove", "agentbridge")
		cmd.Env = withEnv(os.Environ(), "CODEX_HOME", dstCodex)
		_ = cmd.Run()
	}
	fmt.Fprintf(out, "Removed Antigravity plugin (%s) and Codex $agy skill.\n", dstPlugin)
	if *purge {
		abHome, err := hub.HomeDir("")
		if err != nil {
			return err
		}
		if err := os.RemoveAll(abHome); err != nil {
			return fmt.Errorf("purge AGENTBRIDGE_HOME: %w", err)
		}
		fmt.Fprintf(out, "Purged AGENTBRIDGE_HOME (%s).\n", abHome)
	}
	return nil
}

func doctorCmd(args []string, out, stderr io.Writer, endpoint string) error {
	defPlugin, err := defaultPluginDir()
	if err != nil {
		return err
	}
	defCodex, err := defaultCodexHome()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	pluginDir := flags.String("plugin-dir", defPlugin, "Antigravity plugin install directory")
	codexHome := flags.String("codex-home", defCodex, "Codex home directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected doctor arguments")
	}

	abHome, err := hub.HomeDir("")
	if err != nil {
		return err
	}
	printCheck := func(level, label, detail string) {
		fmt.Fprintf(out, "%-6s %-22s %s\n", "["+level+"]", label, detail)
	}

	printCheck("OK", "AGENTBRIDGE_HOME", abHome)
	if _, err := os.Stat(filepath.Join(*pluginDir, "bin", "agentbridge")); err == nil {
		printCheck("OK", "Plugin binary", filepath.Join(*pluginDir, "bin", "agentbridge"))
	} else {
		printCheck("FAIL", "Plugin binary", "missing (run `agentbridge install`)")
	}
	if _, err := os.Stat(filepath.Join(*pluginDir, "hooks.json")); err == nil {
		printCheck("OK", "Antigravity plugin", *pluginDir)
	} else {
		printCheck("FAIL", "Antigravity plugin", "hooks.json missing")
	}
	if _, err := os.Stat(filepath.Join(*codexHome, "skills", "agy", "SKILL.md")); err == nil {
		printCheck("OK", "Codex $agy skill", filepath.Join(*codexHome, "skills", "agy", "SKILL.md"))
	} else {
		printCheck("WARN", "Codex $agy skill", "missing (run `agentbridge install`)")
	}
	if _, err := os.Stat(filepath.Join(*codexHome, "sessions")); err == nil {
		detail := filepath.Join(*codexHome, "sessions")
		if _, idxErr := os.Stat(filepath.Join(*codexHome, "session_index.jsonl")); idxErr == nil {
			detail += " (session_index.jsonl present)"
		}
		printCheck("OK", "Codex sessions", detail)
	} else {
		printCheck("WARN", "Codex sessions", fmt.Sprintf("%s not found (check CODEX_HOME)", filepath.Join(*codexHome, "sessions")))
	}
	if sqlitePath, err := exec.LookPath("sqlite3"); err == nil {
		printCheck("OK", "sqlite3", sqlitePath)
	} else {
		printCheck("WARN", "sqlite3", "not in PATH (Antigravity workspace titles unavailable)")
	}

	client := httpClient(2*time.Second, 500*time.Millisecond)
	defer client.CloseIdleConnections()
	resp, err := client.Get(endpoint + "/healthz")
	if err != nil {
		printCheck("FAIL", "Daemon", fmt.Sprintf("not listening at %s (%v)", endpoint, err))
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		printCheck("FAIL", "Daemon", fmt.Sprintf("%s returned %s", endpoint, resp.Status))
		return nil
	}
	var health struct {
		Status   string              `json:"status"`
		Home     string              `json:"home"`
		Rooms    int                 `json:"rooms"`
		Watchers []api.WatcherStatus `json:"watchers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		printCheck("WARN", "Daemon", fmt.Sprintf("listening at %s (legacy /healthz payload)", endpoint))
		return nil
	}
	printCheck("OK", "Daemon", fmt.Sprintf("listening at %s (home=%s, rooms=%d, watchers=%d)", endpoint, health.Home, health.Rooms, len(health.Watchers)))
	for _, w := range health.Watchers {
		if w.LastError != "" {
			printCheck("WARN", "Watcher "+w.Provider, fmt.Sprintf("session=%s error=%s", w.SessionID, w.LastError))
		}
	}
	return nil
}

func configureCodexMCP(codexHome, binPath string) string {
	codexBin, err := exec.LookPath("codex")
	if err != nil {
		return fmt.Sprintf("codex CLI not in PATH; add manually to %s:\n    [mcp_servers.agentbridge]\n    command = %q\n    args = [\"mcp-stdio\", \"--provider\", \"codex\", \"--workspace\", \".\"]",
			filepath.Join(codexHome, "config.toml"), binPath)
	}
	env := withEnv(os.Environ(), "CODEX_HOME", codexHome)
	rm := exec.Command(codexBin, "mcp", "remove", "agentbridge")
	rm.Env = env
	_ = rm.Run()

	add := exec.Command(codexBin, "mcp", "add", "agentbridge", "--", binPath, "mcp-stdio", "--provider", "codex", "--workspace", ".")
	add.Env = env
	if out, err := add.CombinedOutput(); err != nil {
		return fmt.Sprintf("failed (%v: %s)", err, strings.TrimSpace(string(out)))
	}
	return "registered via `codex mcp add`"
}

func withEnv(base []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(base)+1)
	for _, entry := range base {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
}

func copyBinaryAtomically(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	if dstInfo, err := os.Stat(dst); err == nil && os.SameFile(srcInfo, dstInfo) {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeFileAtomically(dst, data, 0o755)
}

func extractTree(src fs.FS, root, dstDir string) error {
	return fs.WalkDir(src, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := fs.ReadFile(src, path)
		if err != nil {
			return err
		}
		return writeFileAtomically(filepath.Join(dstDir, rel), data, 0o644)
	})
}

func writeFileAtomically(dst string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(data)
	err = errors.Join(writeErr, tmp.Chmod(mode), tmp.Sync(), tmp.Close())
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
