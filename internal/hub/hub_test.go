package hub

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/lainniay/AgentBridge/internal/blackboard"
)

func TestReadModesEchoFilteringAndPersistence(t *testing.T) {
	workspace := t.TempDir()
	stateDir := filepath.Join(t.TempDir(), "state")
	t.Setenv("AGENTBRIDGE_HOME", "")
	t.Setenv("AGENTBRIDGE_STATE_DIR", stateDir)
	h := openHub(t, "")
	status, err := h.Status("", "")
	if err != nil || status.Bound {
		t.Fatalf("unbound status: %+v, %v", status, err)
	}
	result, err := h.Read("", "", ReadOptions{})
	if err != nil || result.Bound {
		t.Fatalf("unbound read: %+v, %v", result, err)
	}
	assertAbsent(t, filepath.Join(workspace, ".agent"))
	assertAbsent(t, stateDir)

	capture := time.Now().UTC().Truncate(time.Second)
	room, err := h.BindRoom(workspace, []Binding{
		{Provider: "alpha", SessionID: "a", CaptureSince: capture},
		{Provider: "beta", SessionID: "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	a, b := room.Participants[0], room.Participants[1]
	ingest(t, h, room.ID, a.ID, "1")
	ingest(t, h, room.ID, b.ID, "2")
	ingest(t, h, room.ID, a.ID, "3")
	ingest(t, h, room.ID, b.ID, "4")
	ingest(t, h, room.ID, a.ID, "5")

	status, err = h.Status(room.ID, a.ID)
	if err != nil || !status.Bound || status.Unread[b.ID] != 2 || status.Participant.ReadCursor != 0 {
		t.Fatalf("initial status: %+v, %v", status, err)
	}
	assertSeqs(t, h, room.ID, a.ID, ReadOptions{Mode: "diff", Peek: true}, 2, 4)
	assertSeqs(t, h, room.ID, a.ID, ReadOptions{Mode: "full", Limit: 2}, 4, 5)

	result, err = h.Read(room.ID, a.ID, ReadOptions{Mode: "path"})
	if err != nil || !result.Bound || !filepath.IsAbs(result.Path) || filepath.Base(result.Path) != room.ID+".md" ||
		filepath.Dir(result.Path) != filepath.Join(stateDir, "rooms") || result.LatestSeq != 5 {
		t.Fatalf("path read: %+v, %v", result, err)
	}
	assertAbsent(t, filepath.Join(workspace, ".agent"))
	if _, err := os.Stat(result.Path); err != nil {
		t.Fatal(err)
	}

	paged, err := h.Read(room.ID, a.ID, ReadOptions{Limit: 1})
	if err != nil || len(paged.Entries) != 1 || paged.Entries[0].Seq != 2 || !paged.HasMore || paged.Cursor != 3 || paged.LatestSeq != 5 {
		t.Fatalf("paged diff: %+v, %v", paged, err)
	}
	status, err = h.Status(room.ID, a.ID)
	if err != nil || status.Participant.ReadCursor != 3 || status.Unread[b.ID] != 1 {
		t.Fatalf("paged status: %+v, %v", status, err)
	}
	rest, err := h.Read(room.ID, a.ID, ReadOptions{})
	if err != nil || len(rest.Entries) != 1 || rest.Entries[0].Seq != 4 || rest.HasMore || rest.Cursor != 5 {
		t.Fatalf("rest diff: %+v, %v", rest, err)
	}
	assertSeqs(t, h, room.ID, a.ID, ReadOptions{})

	// Test full mode paging with after_seq and before_seq.
	fwd, err := h.Read(room.ID, a.ID, ReadOptions{Mode: "full", AfterSeq: 1, Limit: 2})
	if err != nil || !fwd.HasMore || len(fwd.Entries) != 2 || fwd.Entries[0].Seq != 2 || fwd.Entries[1].Seq != 3 {
		t.Fatalf("forward full page: %+v, %v", fwd, err)
	}
	bwd, err := h.Read(room.ID, a.ID, ReadOptions{Mode: "full", BeforeSeq: 5, Limit: 2})
	if err != nil || !bwd.HasMore || len(bwd.Entries) != 2 || bwd.Entries[0].Seq != 3 || bwd.Entries[1].Seq != 4 {
		t.Fatalf("backward full page: %+v, %v", bwd, err)
	}
	h = openHub(t, "")
	resolvedRoom, resolved, ok := h.Resolve(workspace, "alpha", "")
	if !ok || resolvedRoom.ID != room.ID || resolved.ID != a.ID || resolved.ReadCursor != 5 || !resolved.CaptureSince.Equal(capture) {
		t.Fatalf("restored binding: %+v, %+v, %t", resolvedRoom, resolved, ok)
	}
	if _, _, ok := h.Resolve(workspace, "", ""); ok {
		t.Fatal("ambiguous workspace unexpectedly resolved")
	}
	assertSeqs(t, h, room.ID, b.ID, ReadOptions{}, 1, 3, 5)
}

func TestLateJoinBackfillDetachAndStaleCapture(t *testing.T) {
	workspace := t.TempDir()
	h := openHub(t, t.TempDir())
	room, err := h.BindRoom(workspace, []Binding{{Provider: "alpha", SessionID: "a"}})
	if err != nil {
		t.Fatal(err)
	}
	a := room.Participants[0]
	ingest(t, h, room.ID, a.ID, "1")
	older := time.Now().UTC().Add(-time.Hour)
	history := []blackboard.Entry{
		{TS: older, Role: "user", NativeTurnID: "historical", Content: "a question", Provider: "spoof"},
		{TS: older, Role: "assistant", NativeTurnID: "historical", Content: "an answer"},
	}
	room, err = h.JoinRoom(room.ID, []Binding{{Provider: "beta", SessionID: "b", Backfill: history}})
	if err != nil {
		t.Fatal(err)
	}
	b := room.Participants[1]
	assertSeqs(t, h, room.ID, a.ID, ReadOptions{}, 2, 3)
	assertSeqs(t, h, room.ID, b.ID, ReadOptions{}, 1)
	entries, err := h.Blackboard(room.ID)
	if err != nil || len(entries) != 3 || entries[1].Provider != "beta" ||
		entries[1].ParticipantID != b.ID || entries[1].Content != "a question" || entries[2].Content != "an answer" {
		t.Fatalf("backfill: %+v, %v", entries, err)
	}
	_, err = h.JoinRoom(room.ID, []Binding{{Provider: "beta", SessionID: "b"}})
	if !errors.Is(err, ErrAlreadyBound) {
		t.Fatalf("duplicate join: %v", err)
	}
	if err := h.DetachParticipant(room.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, found := h.Resolve("", "beta", "b"); found {
		t.Fatal("detached participant resolved")
	}
	status, err := h.Status(room.ID, b.ID)
	if err != nil || status.Bound {
		t.Fatalf("detached status: %+v, %v", status, err)
	}
	rejoined, err := h.JoinRoom(room.ID, []Binding{{Provider: "beta", SessionID: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if rejoined.Participants[1].ID == b.ID {
		t.Fatal("rejoining reused the old watcher identity")
	}
	err = h.IngestEntry(room.ID, b.ID, blackboard.Entry{Role: "user", NativeTurnID: "stale", Content: "stale"})
	if !errors.Is(err, ErrParticipantNotFound) {
		t.Fatalf("stale watcher ingestion: %v", err)
	}
	assertSeqs(t, h, room.ID, a.ID, ReadOptions{Mode: "full"}, 1, 2, 3)
}

func TestFailedBindDoesNotPublish(t *testing.T) {
	workspace := t.TempDir()
	homeDir := t.TempDir()
	h := openHub(t, homeDir)
	if err := os.Mkdir(h.state, 0700); err != nil {
		t.Fatal(err)
	}
	_, err := h.BindRoom(workspace, []Binding{{Provider: "alpha", SessionID: "a", Backfill: []blackboard.Entry{
		{Role: "user", NativeTurnID: "1", Content: "must not persist"},
	}}})
	if err == nil {
		t.Fatal("binding succeeded with an unusable state path")
	}
	if len(h.Rooms(workspace)) != 0 {
		t.Fatal("partially published room")
	}
	assertAbsent(t, filepath.Join(homeDir, "rooms"))
	assertAbsent(t, filepath.Join(workspace, ".agent"))
}

func TestAgentBridgeHomePrecedenceAndUnbindPrunesRoom(t *testing.T) {
	homeDir := filepath.Join(t.TempDir(), "ab-home")
	legacyDir := filepath.Join(t.TempDir(), "legacy-state")
	t.Setenv("AGENTBRIDGE_HOME", homeDir)
	t.Setenv("AGENTBRIDGE_STATE_DIR", legacyDir)

	h := openHub(t, "")
	if h.Home() != homeDir {
		t.Fatalf("Home = %q, want %q", h.Home(), homeDir)
	}
	workspace := t.TempDir()
	room, err := h.BindRoom(workspace, []Binding{
		{Provider: "alpha", SessionID: "a"},
		{Provider: "beta", SessionID: "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ingest(t, h, room.ID, room.Participants[0].ID, "t1")
	if err := h.UnbindParticipant(room.ID, room.Participants[0].ID); err != nil {
		t.Fatal(err)
	}
	if rooms := h.Rooms(workspace); len(rooms) != 0 {
		t.Fatalf("unbound room still listed: %+v", rooms)
	}
	entries, err := h.Blackboard(room.ID)
	if err != nil || len(entries) != 1 {
		t.Fatalf("archived blackboard: %+v, %v", entries, err)
	}
	assertAbsent(t, legacyDir)
	assertAbsent(t, filepath.Join(workspace, ".agent"))
}

func TestWorkspaceAliasesAndMissingWorkspace(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	alias := filepath.Join(parent, "alias")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(workspace, alias); err != nil {
		t.Fatal(err)
	}
	h := openHub(t, t.TempDir())
	room, err := h.BindRoom(alias, []Binding{{Provider: "alpha", SessionID: "a"}})
	if err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(workspace)
	if err != nil || room.Workspace != real {
		t.Fatalf("saved workspace = %q, want %q, %v", room.Workspace, real, err)
	}
	for _, path := range []string{workspace, alias} {
		if rooms := h.Rooms(path); len(rooms) != 1 || rooms[0].ID != room.ID {
			t.Fatalf("rooms at %q: %+v", path, rooms)
		}
		if _, _, ok := h.Resolve(path, "alpha", ""); !ok {
			t.Fatalf("could not resolve workspace %q", path)
		}
	}
	if err := os.Rename(real, real+"-moved"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := h.Resolve(real, "alpha", "a"); !ok {
		t.Fatal("stored workspace no longer resolves after moving its directory")
	}
}

func openHub(t *testing.T, stateDir string) *Hub {
	t.Helper()
	h, err := Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func ingest(t *testing.T, h *Hub, roomID, participantID, turn string) {
	t.Helper()
	entry := blackboard.Entry{Role: "user", NativeTurnID: turn, Content: "message " + turn}
	if err := h.IngestEntry(roomID, participantID, entry); err != nil {
		t.Fatal(err)
	}
}

func assertSeqs(t *testing.T, h *Hub, roomID, participantID string, options ReadOptions, want ...uint64) {
	t.Helper()
	result, err := h.Read(roomID, participantID, options)
	if err != nil || !result.Bound {
		t.Fatalf("read: %+v, %v", result, err)
	}
	var sequences []uint64
	for _, entry := range result.Entries {
		sequences = append(sequences, entry.Seq)
	}
	if !reflect.DeepEqual(sequences, want) {
		t.Fatalf("got sequences %v, want %v", sequences, want)
	}
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s should be absent: %v", path, err)
	}
}
