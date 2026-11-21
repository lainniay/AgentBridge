package blackboard

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestConcurrentAppendAndDeduplication(t *testing.T) {
	workspace := t.TempDir()
	board, err := Open(workspace, "rm_test")
	if err != nil {
		t.Fatal(err)
	}
	const turns = 32
	var writers sync.WaitGroup
	for number := range turns {
		writers.Add(1)
		go func() {
			defer writers.Done()
			turn := fmt.Sprintf("turn-%d", number)
			user := Entry{ParticipantID: "p1", Provider: "one", Role: "user", NativeTurnID: turn, Content: "question"}
			assistant := Entry{ParticipantID: "p1", Provider: "one", Role: "assistant", NativeTurnID: turn, Content: "answer"}
			if err := board.AppendBatch([]Entry{user, assistant, user, assistant}, nil); err != nil {
				t.Error(err)
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		writers.Wait()
		close(done)
	}()
readViews:
	for {
		view := string(readFile(t, board.Path()))
		if strings.Count(view, "<user ") != strings.Count(view, "</user>") ||
			strings.Count(view, "<one ") != strings.Count(view, "</one>") {
			t.Errorf("observed a partially written view: %q", view)
			break
		}
		select {
		case <-done:
			break readViews
		default:
		}
	}
	<-done

	entries, err := Load(workspace, "rm_test")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != turns*2 {
		t.Fatalf("found %d records, want %d", len(entries), turns*2)
	}
	for index, entry := range entries {
		if entry.Seq != uint64(index+1) || entry.RoomID != "rm_test" || entry.TS.IsZero() {
			t.Fatalf("invalid record at index %d: %+v", index, entry)
		}
	}
	if bytes.Count(readFile(t, board.jsonl), []byte("\n")) != turns*2 {
		t.Fatal("JSONL contains partial or additional records")
	}
	reopened, err := Open(workspace, "rm_test")
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.AppendBatch(entries, nil); err != nil {
		t.Fatal(err)
	}
	if len(reopened.Entries()) != turns*2 {
		t.Fatal("reopening lost the deduplication keys")
	}
}

func TestRenderingRollbackAndRecovery(t *testing.T) {
	roomsDir := filepath.Join(t.TempDir(), "rooms")

	board, err := Open(roomsDir, "rm_first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(roomsDir, "rm_second"); err != nil {
		t.Fatal(err)
	}
	if got, want := board.Path(), filepath.Join(roomsDir, "rm_first.md"); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}

	entries := []Entry{
		{ParticipantID: "p1", Provider: "one", Role: "user", NativeTurnID: "turn", Content: "first\nline"},
		{ParticipantID: "p1", Provider: "one", Role: "assistant", NativeTurnID: "turn", Content: "result", FilesTouched: []string{"a&b.go", "helper.go"}, Handoff: &Handoff{Target: "two", Mode: "execute", ReturnControl: true}},
	}
	if err := board.AppendBatch(entries, nil); err != nil {
		t.Fatal(err)
	}
	if board.LatestSeq() != 2 {
		t.Fatalf("LatestSeq = %d, want 2", board.LatestSeq())
	}
	expected := "<user seq=\"1\" channel=\"one\">\nfirst\nline\n</user>\n\n<one seq=\"2\" handoff=\"two\" return_control=\"true\" files_touched=\"a&amp;b.go,helper.go\">\nresult\n</one>\n"
	if got := string(readFile(t, board.Path())); got != expected {
		t.Fatalf("view:\n%s", got)
	}
	original := string(readFile(t, board.jsonl))

	rejected := errors.New("state save failed")
	entries[0].NativeTurnID = "failed"
	err = board.AppendBatch(entries[:1], func() error { return rejected })
	if !errors.Is(err, rejected) {
		t.Fatalf("append error = %v, want %v", err, rejected)
	}
	if got := string(readFile(t, board.jsonl)); got != original {
		t.Fatalf("rejected append persisted: %q", got)
	}
	if got := string(readFile(t, board.Path())); got != expected {
		t.Fatalf("rejected append visible in view: %q", got)
	}

	writeFile(t, board.jsonl, original+`{"seq":3,`)
	writeFile(t, board.Path(), "stale Markdown")
	recovered, err := Open(roomsDir, "rm_first")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, board.jsonl)); got != original {
		t.Fatalf("incomplete record not truncated: %q", got)
	}
	if got := string(readFile(t, recovered.Path())); got != expected {
		t.Fatalf("view not repaired: %q", got)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
