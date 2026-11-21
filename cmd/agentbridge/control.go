package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

func control(command string, args []string, out, stderr io.Writer, endpoint string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var method, path string
	var payload any
	switch command {
	case "status":
		workspace := flags.String("workspace", cwd, "workspace path")
		if err := flags.Parse(args); err != nil {
			return err
		}
		abs, err := filepath.Abs(*workspace)
		if err != nil {
			return err
		}
		method = http.MethodGet
		path = "/api/rooms?" + url.Values{"workspace": {abs}}.Encode()
	case "link":
		workspace := flags.String("workspace", cwd, "workspace path")
		agyID := flags.String("antigravity", "", "Antigravity conversation ID")
		codexID := flags.String("codex", "", "Codex session ID")
		room := flags.String("room", "", "existing room ID for late join")
		backfill := flags.Int("backfill-turns", 0, "history turns to import per session")
		latest := flags.Bool("latest", false, "bind the latest available Antigravity and Codex sessions")
		if err := flags.Parse(args); err != nil {
			return err
		}
		if *backfill < 0 {
			return errors.New("--backfill-turns must be nonnegative")
		}
		if *latest && (*agyID != "" || *codexID != "" || *room != "") {
			return errors.New("--latest cannot be combined with --antigravity, --codex, or --room")
		}
		if !*latest && *agyID == "" && *codexID == "" {
			return errors.New("link requires --antigravity, --codex, or --latest")
		}
		abs, err := filepath.Abs(*workspace)
		if err != nil {
			return err
		}
		var participants []map[string]string
		for _, p := range []struct{ provider, id string }{{"antigravity", *agyID}, {"codex", *codexID}} {
			if p.id != "" {
				participants = append(participants, map[string]string{"provider": p.provider, "session_id": p.id})
			}
		}
		method, path = http.MethodPost, "/api/rooms/bind"
		if *latest {
			payload = map[string]any{"workspace": abs, "latest": true, "backfill_turns": *backfill}
		} else {
			payload = map[string]any{"workspace": abs, "room_id": *room, "backfill_turns": *backfill, "participants": participants}
		}
	case "unlink":
		room := flags.String("room", "", "room ID")
		p := flags.String("participant", "", "participant ID")
		if err := flags.Parse(args); err != nil {
			return err
		}
		if *room == "" || *p == "" {
			return errors.New("unlink requires --room and --participant")
		}
		method, path = http.MethodPost, "/api/rooms/detach"
		payload = map[string]string{"room_id": *room, "participant_id": *p}
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected %s arguments", command)
	}
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, endpoint+path, body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := httpClient(15*time.Second, time.Second)
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var reply struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &reply) == nil && reply.Error != "" {
			return fmt.Errorf("%s: %s", resp.Status, reply.Error)
		}
		return errors.New(resp.Status)
	}
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, data, "", "  "); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, formatted.String())
	return err
}
