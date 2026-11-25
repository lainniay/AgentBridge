package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lainniay/AgentBridge/internal/blackboard"
)

func rolloutFixture(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/rollout.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "2026", "11", "20")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout.jsonl")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNativeRolloutDiscoveryAndSanitization(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	rolloutFixture(t, filepath.Join(home, "sessions"))
	a := New("")
	sessions, err := a.ListSessions("/project/example")
	if err != nil || len(sessions) != 1 || sessions[0].ID != "sample-thread" || sessions[0].Title != "Example" || sessions[0].Provider != "codex" {
		t.Fatalf("sessions %+v, error %v", sessions, err)
	}
	sessions, err = a.ListSessions("/project/other")
	if err != nil || len(sessions) != 0 {
		t.Fatalf("unrelated workspace sessions %+v, error %v", sessions, err)
	}
	if _, err := a.ReadHistory("parent-thread", 0); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("embedded parent metadata resolved as a session: %v", err)
	}

	entries, err := a.ReadHistory("sample-thread", 0)
	if err != nil {
		t.Fatal(err)
	}
	wantText := []string{"Update the sample.", "The sample was updated.", "Do another pass.", "Check the result.", "The result is correct."}
	wantID := []string{"turn-one", "turn-one", "turn-aborted", "turn-three", "turn-three"}
	wantRole := []string{"user", "assistant", "user", "user", "assistant"}
	if len(entries) != len(wantText) {
		t.Fatalf("entries %+v", entries)
	}
	for i, entry := range entries {
		if entry.Content != wantText[i] || entry.Role != wantRole[i] || entry.NativeTurnID != wantID[i] || entry.TS.IsZero() {
			t.Fatalf("unexpected entry %d: %+v", i, entry)
		}
	}
	if want := []string{"example.go", "new.txt", "old.txt"}; !reflect.DeepEqual(entries[1].FilesTouched, want) {
		t.Fatalf("file changes %v, want %v", entries[1].FilesTouched, want)
	}
	last, err := a.ReadHistory("sample-thread", 1)
	if err != nil || !reflect.DeepEqual(last, entries[3:]) {
		t.Fatalf("late join last turn %+v, error %v", last, err)
	}
}

func TestWatchSinceReplaysPostBindAndTailsCompleteLines(t *testing.T) {
	root := t.TempDir()
	path := rolloutFixture(t, root)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	write := func(content string) {
		t.Helper()
		if _, err := f.WriteString(content); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"timestamp":"2026-11-20T09:59:58Z","type":"event_msg","payload":{"type":"task_started","turn_id":"crosses-binding"}}
{"timestamp":"2026-11-20T09:59:58Z","type":"event_msg","payload":{"type":"user_message","message":"Pre-bind user."}}
{"timestamp":"2026-11-20T09:59:59Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Recovered final."}]}}
{"timestamp":"2026-11-20T10:00:01Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"crosses-binding"}}
`)
	a := New(root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan blackboard.Entry, 8)
	finished := make(chan error, 1)
	since := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	go func() {
		finished <- a.WatchSince(ctx, "sample-thread", since, func(entry blackboard.Entry) error {
			events <- entry
			return nil
		})
	}()
	expect := func(role, content string) {
		t.Helper()
		select {
		case entry := <-events:
			if entry.Role != role || entry.Content != content {
				t.Fatalf("watch entry %+v, want %s %q", entry, role, content)
			}
		case err := <-finished:
			t.Fatalf("watch exited early: %v", err)
		case <-time.After(discoveryInterval + 3*time.Second):
			t.Fatalf("watch did not emit %q", content)
		}
	}
	expect("assistant", "Recovered final.")
	write(`{"timestamp":"2026-11-20T10:00:02Z","type":"event_msg","payload":{"type":"task_started","turn_id":"live"}}
{"type":"event_msg","payload":{"type":"user_message","message":"Live input."}}`)
	select {
	case entry := <-events:
		t.Fatalf("watch emitted unterminated line: %+v", entry)
	case <-time.After(320 * time.Millisecond):
	}
	write("\n")
	expect("user", "Live input.")

	// A resumed native thread can append to a distinct rollout segment.
	next := filepath.Join(filepath.Dir(path), "resumed.jsonl")
	if err := os.WriteFile(next, []byte(`{"type":"session_meta","payload":{"id":"sample-thread","cwd":"/project/example"}}
{"timestamp":"2026-11-20T10:00:03Z","type":"event_msg","payload":{"type":"task_started","turn_id":"resumed-turn"}}
{"timestamp":"2026-11-20T10:00:04Z","type":"event_msg","payload":{"type":"user_message","message":"Resumed input."}}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	expect("user", "Resumed input.")
	history, err := a.ReadHistory("sample-thread", 0)
	if err != nil || len(history) != 9 || history[len(history)-1].Content != "Resumed input." {
		t.Fatalf("merged history %+v, error %v", history, err)
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stop watch: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch ignored cancellation")
	}
}

func TestSessionCatalogRefreshAndInvalidation(t *testing.T) {
	root := t.TempDir()
	a := New(root)
	path := filepath.Join(root, "rollout.jsonl")
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	meta := func(id string) string {
		return fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":"/project/example"}}`+"\n", id)
	}
	list := func(want ...string) {
		t.Helper()
		sessions, err := a.ListSessions("/project/example")
		if err != nil || len(sessions) != len(want) {
			t.Fatalf("sessions %+v, error %v, want %v", sessions, err, want)
		}
		for _, id := range want {
			found := false
			for _, session := range sessions {
				found = found || session.ID == id
			}
			if !found {
				t.Errorf("session %q missing from %+v", id, sessions)
			}
		}
	}

	partial := meta("original")
	write(path, partial[:len(partial)-10])
	list()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(partial[len(partial)-10:] + "{}\n")
	if closeErr := f.Close(); err != nil || closeErr != nil {
		t.Fatalf("finish partial rollout: write %v, close %v", err, closeErr)
	}
	list("original")

	second := filepath.Join(root, "second.jsonl")
	write(second, meta("new"))
	snapshot, err := a.sessions(false)
	if err != nil || len(snapshot["original"]) != 1 || len(snapshot["new"]) != 0 {
		t.Fatalf("background discovery ignored shared snapshot: %+v, error %v", snapshot, err)
	}
	list("original", "new") // On-demand discovery sees new files immediately.

	write(path, meta("short")) // Truncating the same inode invalidates its header.
	list("short", "new")
	replacement := filepath.Join(root, "replacement.tmp")
	write(replacement, meta("replacement-with-a-longer-header"))
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	list("replacement-with-a-longer-header", "new")
	if err := os.Remove(second); err != nil {
		t.Fatal(err)
	}
	list("replacement-with-a-longer-header")
	if _, ok := a.metadata[second]; ok {
		t.Fatal("removed rollout still in header cache")
	}
}

func TestWatchRejectsAnotherSessionReplacingCachedPath(t *testing.T) {
	root := t.TempDir()
	path := rolloutFixture(t, root)
	a := New(root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entries := make(chan blackboard.Entry, 16)
	stopped := make(chan error, 1)
	go func() {
		stopped <- a.WatchSince(ctx, "sample-thread", time.Time{}, func(entry blackboard.Entry) error {
			select {
			case entries <- entry:
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil
		})
	}()
	for range 5 {
		select {
		case <-entries:
		case err := <-stopped:
			t.Fatalf("watch exited before reading fixture: %v", err)
		case <-time.After(3 * time.Second):
			t.Fatal("watch did not read fixture")
		}
	}

	other := filepath.Join(root, "replacement.tmp")
	if err := os.WriteFile(other, []byte(`{"type":"session_meta","payload":{"id":"other-thread"}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"other-turn"}}
{"type":"event_msg","payload":{"type":"user_message","message":"Unrelated prompt."}}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, path); err != nil {
		t.Fatal(err)
	}
	select {
	case entry := <-entries:
		t.Fatalf("foreign session entry emitted: %+v", entry)
	case err := <-stopped:
		t.Fatalf("watch exited before background discovery: %v", err)
	case <-time.After(350 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stop watch: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch ignored cancellation")
	}
}

func TestSessionIndexThreadNameAndSubagentFiltering(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	sessionsRoot := filepath.Join(home, "sessions", "2026", "11", "20")
	if err := os.MkdirAll(sessionsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	const ws = "/project/example"
	writeRollout := func(name string, lines ...string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(sessionsRoot, name), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Main user thread (should appear and get thread_name from session_index.jsonl)
	writeRollout("main.jsonl",
		fmt.Sprintf(`{"type":"session_meta","payload":{"id":"main-thread","cwd":%q,"source":"vscode","thread_source":"user"}}`, ws),
		`{"type":"event_msg","payload":{"type":"user_message","message":"Very long initial prompt that should be overridden by thread_name"}}`,
	)
	// User-forked thread without session_index entry yet (should appear with fallback prompt title)
	writeRollout("fork.jsonl",
		fmt.Sprintf(`{"type":"session_meta","payload":{"id":"user-fork","forked_from_id":"main-thread","cwd":%q,"source":"vscode","thread_source":"user"}}`, ws),
		`{"type":"event_msg","payload":{"type":"user_message","message":"Forked thread prompt"}}`,
	)
	// Internal subagent & guardian review threads (must be filtered out)
	writeRollout("subagent.jsonl",
		fmt.Sprintf(`{"type":"session_meta","payload":{"id":"sub-1","parent_thread_id":"main-thread","cwd":%q,"source":{"subagent":{"thread_spawn":{"parent_thread_id":"main-thread"}}},"thread_source":"subagent"}}`, ws),
		`{"type":"event_msg","payload":{"type":"user_message","message":"Subagent prompt"}}`,
	)
	writeRollout("guardian.jsonl",
		fmt.Sprintf(`{"type":"session_meta","payload":{"id":"sub-2","parent_thread_id":"main-thread","cwd":%q,"source":{"subagent":{"other":"guardian"}},"thread_source":"guardian_review"}}`, ws),
	)

	indexPath := filepath.Join(home, "session_index.jsonl")
	if err := os.WriteFile(indexPath, []byte(`{"id":"main-thread","thread_name":"实现 AgentBridge Phase 1"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := New("")
	sessions, err := a.ListSessions(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2 (subagents filtered): %+v", len(sessions), sessions)
	}
	titles := make(map[string]string, len(sessions))
	for _, s := range sessions {
		titles[s.ID] = s.Title
	}
	if titles["main-thread"] != "实现 AgentBridge Phase 1" {
		t.Fatalf("main-thread title = %q, want thread_name", titles["main-thread"])
	}
	if titles["user-fork"] != "Forked thread prompt" {
		t.Fatalf("user-fork title = %q, want fallback prompt", titles["user-fork"])
	}

	// Updating session_index.jsonl (e.g. thread rename) refreshes title without touching rollout.
	if err := os.WriteFile(indexPath, []byte(
		`{"id":"main-thread","thread_name":"实现 AgentBridge Phase 1"}`+"\n"+
			`{"id":"main-thread","thread_name":"已重命名的线程"}`+"\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	sessions, err = a.ListSessions(ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		if s.ID == "main-thread" && s.Title != "已重命名的线程" {
			t.Fatalf("renamed thread title = %q", s.Title)
		}
	}
}

func TestCapturePlanItemsAndDeduplication(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	sessionsRoot := filepath.Join(home, "sessions", "2026", "11", "20")
	if err := os.MkdirAll(sessionsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"timestamp":"2026-11-20T10:00:00Z","type":"session_meta","payload":{"id":"plan-thread","cwd":"/project/example"}}`,
		// Turn 1: Plan item only (no separate final_answer).
		`{"timestamp":"2026-11-20T10:00:01Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}`,
		`{"timestamp":"2026-11-20T10:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"/plan 设计方案"}}`,
		`{"timestamp":"2026-11-20T10:00:02Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"Plan","text":"# Plan 1\n1. Do A\n2. Do B"}}}`,
		`{"timestamp":"2026-11-20T10:00:03Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}`,
		// Turn 2: Short final_answer plus distinct Plan item -> both merged.
		`{"timestamp":"2026-11-20T10:01:01Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t2"}}`,
		`{"timestamp":"2026-11-20T10:01:01Z","type":"event_msg","payload":{"type":"user_message","message":"补充计划"}}`,
		`{"timestamp":"2026-11-20T10:01:02Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"t2","item":{"type":"proposed_plan","content":[{"type":"text","text":"# Plan 2\n- Step X"}]}}}`,
		`{"timestamp":"2026-11-20T10:01:03Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"请确认以下计划："}]}}`,
		`{"timestamp":"2026-11-20T10:01:04Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t2"}}`,
		// Turn 3: final_answer already contains the plan text -> no duplication.
		`{"timestamp":"2026-11-20T10:02:01Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t3"}}`,
		`{"timestamp":"2026-11-20T10:02:01Z","type":"event_msg","payload":{"type":"user_message","message":"再输出一次"}}`,
		`{"timestamp":"2026-11-20T10:02:02Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"t3","item":{"type":"Plan","text":"# Plan 3\n- Same text"}}}`,
		`{"timestamp":"2026-11-20T10:02:03Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"<proposed_plan>\n# Plan 3\n- Same text\n</proposed_plan>"}]}}`,
		`{"timestamp":"2026-11-20T10:02:04Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t3"}}`,
	}
	if err := os.WriteFile(filepath.Join(sessionsRoot, "plan.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := New("").ReadHistory("plan-thread", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 6 {
		t.Fatalf("got %d entries, want 6: %+v", len(entries), entries)
	}
	if entries[1].Role != "assistant" || entries[1].Content != "# Plan 1\n1. Do A\n2. Do B" {
		t.Fatalf("turn 1 assistant = %+v", entries[1])
	}
	if entries[3].Role != "assistant" || entries[3].Content != "请确认以下计划：\n\n# Plan 2\n- Step X" {
		t.Fatalf("turn 2 assistant = %+v", entries[3])
	}
	if entries[5].Role != "assistant" || strings.Count(entries[5].Content, "# Plan 3") != 1 {
		t.Fatalf("turn 3 assistant should deduplicate plan text: %q", entries[5].Content)
	}
}
