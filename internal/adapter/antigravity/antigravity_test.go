package antigravity

import (
	"bufio"
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/transcript.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeFixture(t *testing.T, root, id string, data []byte) string {
	t.Helper()
	path := filepath.Join(root, id, ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadHistorySanitizesNativeTranscript(t *testing.T) {
	root := t.TempDir()
	path := writeFixture(t, root, "sample", fixture(t))
	a := New(root)

	entries, err := a.ReadHistory("sample", 0)
	if err != nil {
		t.Fatal(err)
	}
	wantText := []string{"Update the sample.", "The sample was updated.", "Check the result.", "The result is correct."}
	wantID := []string{"step:0", "step:0", "step:5", "step:5"}
	if len(entries) != len(wantText) {
		t.Fatalf("got %d entries, want %d", len(entries), len(wantText))
	}
	for i, entry := range entries {
		wantRole := []string{"user", "assistant"}[i%2]
		if entry.Content != wantText[i] || entry.NativeTurnID != wantID[i] || entry.Role != wantRole || entry.Provider != a.Provider() || entry.TS.IsZero() {
			t.Fatalf("unexpected entry %d: %+v", i, entry)
		}
	}
	if want := []string{"example.go", "new.txt", "old.txt"}; !reflect.DeepEqual(entries[1].FilesTouched, want) {
		t.Fatalf("file changes %v, want %v", entries[1].FilesTouched, want)
	}
	latest, err := a.ReadTranscript(path, 1)
	if err != nil || !reflect.DeepEqual(latest, entries[2:]) {
		t.Fatalf("latest turn %+v, error %v", latest, err)
	}
}

func TestHookEntriesUseNativeTurnAndRejectStaleOrCanceled(t *testing.T) {
	data := fixture(t)
	// A large tool output exercises the reverse-reader block boundaries.
	middle := "{\"step_index\":5,\"type\":\"GENERIC\",\"content\":\"" + strings.Repeat("x", 100_000) + "\"}\n"
	data = []byte(strings.Replace(string(data), "bad json\n", middle, 1))
	path := writeFixture(t, t.TempDir(), "sample", data)
	a := New(t.TempDir())
	input := HookInput{ConversationID: "sample", TranscriptPath: path, LastUserInput: "Check the result.", TerminationReason: "NO_TOOL_CALL"}

	user, err := a.HookUser(input)
	if err != nil || user.NativeTurnID != "step:5" || user.Content != input.LastUserInput {
		t.Fatalf("hook user %+v, error %v", user, err)
	}
	all, err := a.HookEntries(input)
	if err != nil || len(all) != 2 || !reflect.DeepEqual(all[0], user) || all[1].Content != "The result is correct." {
		t.Fatalf("hook entries %+v, error %v", all, err)
	}
	input.LastUserInput = "A newer prompt not in the log"
	all, err = a.HookEntries(input)
	if err != nil || len(all) != 0 {
		t.Fatalf("stale hook entries %+v, error %v", all, err)
	}
	input.LastUserInput = user.Content
	input.TerminationReason = "USER_CANCELED"
	all, err = a.HookEntries(input)
	if err != nil || len(all) != 0 {
		t.Fatalf("canceled hook entries %+v, error %v", all, err)
	}
}

func TestQueuedRepeatedUserInputUsesNextIndex(t *testing.T) {
	root := t.TempDir()
	path := writeFixture(t, root, "sample", fixture(t))
	a := New(root)
	initialSteps := 8
	input := HookInput{ConversationID: "sample", LastUserInput: "Check the result.", InitialNumSteps: &initialSteps}
	queued, err := a.HookUser(input)
	if err != nil || queued.NativeTurnID != "step:7" {
		t.Fatalf("queued entry %+v, error %v", queued, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"step_index\":7,\"type\":\"USER_INPUT\",\"content\":\"Check the result.\"}\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	actual, err := a.HookUser(input)
	if err != nil || actual.NativeTurnID != queued.NativeTurnID {
		t.Fatalf("persisted input %+v, error %v", actual, err)
	}
}

func TestListSessionsWithNativeWorkspaceIndex(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("optional SQLite executable unavailable")
	}
	base := t.TempDir()
	root := filepath.Join(base, "brain")
	db := filepath.Join(base, "conversation_summaries.db")
	here, other := t.TempDir(), t.TempDir()
	writeFixture(t, root, "here", fixture(t))
	writeFixture(t, root, "other", fixture(t))
	writeFixture(t, root, "unindexed", fixture(t))
	uri := func(path string) string {
		u := url.URL{Scheme: "file", Path: path}
		return u.String()
	}
	query := "CREATE TABLE conversation_summaries (conversation_id TEXT, title TEXT, workspace_uris TEXT);" +
		" INSERT INTO conversation_summaries VALUES ('here','Current','[\"" + uri(here) + "\"]'), ('other','Other','[\"" + uri(other) + "\"]');"
	if out, err := exec.Command(sqlite, db, query).CombinedOutput(); err != nil {
		t.Fatalf("create summary fixture: %v %s", err, out)
	}
	sessions, err := New(root).ListSessions(here)
	if err != nil || len(sessions) != 2 {
		t.Fatalf("sessions %+v, error %v", sessions, err)
	}
	for _, s := range sessions {
		switch s.ID {
		case "here":
			if s.Title != "Current" || s.Workspace != here {
				t.Fatalf("wrong workspace metadata: %+v", s)
			}
		case "unindexed":
			if s.Workspace != "" {
				t.Fatalf("assigned an unindexed workspace: %+v", s)
			}
		default:
			t.Fatalf("unrelated workspace session: %+v", s)
		}
	}
}

func TestDiscoverSessionBeforeWALCheckpointAndTranscript(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("optional SQLite executable unavailable")
	}
	base, workspace := t.TempDir(), t.TempDir()
	db := filepath.Join(base, "conversation_summaries.db")
	root := filepath.Join(base, "brain")
	if out, err := exec.Command(sqlite, db, "CREATE TABLE conversation_summaries (conversation_id TEXT, title TEXT, workspace_uris TEXT);").CombinedOutput(); err != nil {
		t.Fatalf("create summary fixture: %v %s", err, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	writer := exec.CommandContext(ctx, sqlite, "-batch", db)
	stdin, err := writer.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := writer.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = writer.Wait()
	}()
	const id = "b47a7812-2b55-48b9-a5f5-580efff71c11"
	uri := url.URL{Scheme: "file", Path: workspace}
	query := "PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;\n" +
		"INSERT INTO conversation_summaries VALUES ('" + id + "','Just created','[\"" + uri.String() + "\"]');\n" +
		".print committed\n"
	if _, err := stdin.Write([]byte(query)); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() && scanner.Text() != "committed" {
	}
	if scanner.Text() != "committed" {
		t.Fatalf("SQLite writer did not commit: %v", scanner.Err())
	}
	wal, err := os.Stat(db + "-wal")
	if err != nil || wal.Size() <= 32 {
		t.Fatalf("fixture is missing committed WAL frames: %v", err)
	}
	a := New(root)
	sessions, err := a.ListSessions(workspace)
	if err != nil || len(sessions) != 1 || sessions[0].ID != id || !sessions[0].Pending || sessions[0].Title != "Just created" {
		t.Fatalf("new indexed conversation: %+v, %v", sessions, err)
	}
	other := t.TempDir()
	if a.CanBindPending(id, other) || !a.CanBindPending(id, workspace) {
		t.Fatal("pending bind did not honor the WAL workspace association")
	}
	writeFixture(t, root, id, fixture(t))
	sessions, err = a.ListSessions(workspace)
	if err != nil || len(sessions) != 1 || sessions[0].ID != id || sessions[0].Pending {
		t.Fatalf("ready conversation after transcript creation: %+v, %v", sessions, err)
	}
}

func TestUnwrapUserRequestEnvelope(t *testing.T) {
	root := t.TempDir()
	raw := `{"step_index":10,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-20T12:25:10Z","content":"<USER_REQUEST>\n你现在查看一下了?\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-11-20T20:25:10+08:00.\n</ADDITIONAL_METADATA>"}` + "\n" +
		`{"step_index":11,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T12:25:15Z","content":"已查看。"}` + "\n"
	path := writeFixture(t, root, "wrapped", []byte(raw))
	a := New(root)

	history, err := a.ReadHistory("wrapped", 0)
	if err != nil || len(history) != 2 || history[0].Content != "你现在查看一下了?" || history[1].Content != "已查看。" {
		t.Fatalf("wrapped history: %+v, %v", history, err)
	}

	input := HookInput{
		ConversationID:    "wrapped",
		TranscriptPath:    path,
		LastUserInput:     "你现在查看一下了?",
		TerminationReason: "NO_TOOL_CALL",
	}
	user, err := a.HookUser(input)
	if err != nil || user.NativeTurnID != "step:10" || user.Content != "你现在查看一下了?" {
		t.Fatalf("wrapped hook user: %+v, %v", user, err)
	}
	entries, err := a.HookEntries(input)
	if err != nil || len(entries) != 2 || entries[0].Content != "你现在查看一下了?" || entries[1].Content != "已查看。" {
		t.Fatalf("wrapped hook entries: %+v, %v", entries, err)
	}
}

func TestFallbackTitleAndImmutableSummaryFallback(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("optional SQLite executable unavailable")
	}
	base := t.TempDir()
	root := filepath.Join(base, "brain")
	db := filepath.Join(base, "conversation_summaries.db")
	here, other := t.TempDir(), t.TempDir()

	writeFixture(t, root, "here-unindexed", []byte(
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>\n  第一条用户提问用于标题兜底\n</USER_REQUEST>"}`+"\n",
	))
	writeFixture(t, root, "other-ws", fixture(t))

	uri := func(path string) string { return (&url.URL{Scheme: "file", Path: path}).String() }
	query := "PRAGMA journal_mode=WAL; " +
		"CREATE TABLE conversation_summaries (conversation_id TEXT, title TEXT, workspace_uris TEXT);" +
		" INSERT INTO conversation_summaries VALUES ('other-ws','Other Title','[\"" + uri(other) + "\"]');" +
		" PRAGMA wal_checkpoint(TRUNCATE);"
	if out, err := exec.Command(sqlite, db, query).CombinedOutput(); err != nil {
		t.Fatalf("create summary fixture: %v %s", err, out)
	}
	// Remove -shm and lock directory read-only or make -wal non-empty without -shm
	// so mode=ro cannot open/recover WAL, exercising the immutable=1 fallback.
	_ = os.Remove(db + "-shm")
	_ = os.WriteFile(db+"-wal", []byte("stale-wal-header"), 0o600)

	sessions, err := New(root).ListSessions(here)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != "here-unindexed" || sessions[0].Title != "第一条用户提问用于标题兜底" {
		t.Fatalf("got sessions %+v, want only here-unindexed with fallback title", sessions)
	}
}

func TestCapturePlanArtifactAndTerminalStepStop(t *testing.T) {
	root := t.TempDir()
	planPath := filepath.Join(root, "plan-conv", "implementation_plan.md")
	if err := os.MkdirAll(filepath.Dir(planPath), 0o700); err != nil {
		t.Fatal(err)
	}
	diskPlan := "# Implementation Plan\n\n1. Update parser\n2. Add tests\n"
	if err := os.WriteFile(planPath, []byte(diskPlan), 0o600); err != nil {
		t.Fatal(err)
	}

	// Turn 1: write_file with double-encoded string args + RequestFeedback=true, followed by a brief prose reply.
	// Turn 2: plan written via write_file with no trailing prose reply, stopped with TERMINAL_STEP_TYPE (inline Content fallback when file missing).
	missingPlan := filepath.Join(root, "plan-conv", "draft_plan.md")
	raw := strings.Join([]string{
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-20T12:00:00Z","content":"<USER_REQUEST>\n/plan 实现同步功能\n</USER_REQUEST>"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T12:00:05Z","tool_calls":[{"name":"write_file","args":{"Path":"` + strings.ReplaceAll(`"`+planPath+`"`, `"`, `\"`) + `","Content":"old draft","ArtifactMetadata":{"RequestFeedback":true,"UserFacing":true,"Summary":"Plan"}}}]}`,
		`{"step_index":2,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T12:00:06Z","content":"方案已写入，请审阅。"}`,
		`{"step_index":3,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-20T12:01:00Z","content":"/plan 仅生成计划文件"}`,
		`{"step_index":4,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T12:01:05Z","tool_calls":[{"name":"write_file","args":{"Path":"` + missingPlan + `","Content":"# Draft Plan\n- Step A","ArtifactMetadata":{"RequestFeedback":true}}}]}`,
	}, "\n") + "\n"

	transcript := writeFixture(t, root, "plan-conv", []byte(raw))
	a := New(root)

	entries, err := a.ReadHistory("plan-conv", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("got %d entries, want 4: %+v", len(entries), entries)
	}
	if !strings.Contains(entries[1].Content, "方案已写入，请审阅。") || !strings.Contains(entries[1].Content, "[Plan: implementation_plan.md]\n# Implementation Plan") {
		t.Fatalf("turn 1 assistant missing merged disk plan: %q", entries[1].Content)
	}
	if strings.Contains(entries[1].Content, "old draft") {
		t.Fatalf("turn 1 should prefer latest on-disk plan over initial draft: %q", entries[1].Content)
	}
	if entries[3].Role != "assistant" || entries[3].Content != "[Plan: draft_plan.md]\n# Draft Plan\n- Step A" {
		t.Fatalf("turn 2 assistant should emit fallback inline plan: %+v", entries[3])
	}

	// Hook Stop with TERMINAL_STEP_TYPE should collect the plan-only turn, while USER_CANCELED stays rejected.
	input := HookInput{
		ConversationID:    "plan-conv",
		TranscriptPath:    transcript,
		LastUserInput:     "/plan 仅生成计划文件",
		TerminationReason: "TERMINAL_STEP_TYPE",
	}
	if !input.IsCollectibleStop() {
		t.Fatal("TERMINAL_STEP_TYPE should be collectible")
	}
	got, err := a.HookEntries(input)
	if err != nil || len(got) != 2 || got[1].Content != "[Plan: draft_plan.md]\n# Draft Plan\n- Step A" {
		t.Fatalf("TERMINAL_STEP_TYPE hook entries: %+v, err=%v", got, err)
	}
	input.TerminationReason = "USER_CANCELED"
	if input.IsCollectibleStop() {
		t.Fatal("USER_CANCELED must not be collectible")
	}
}
