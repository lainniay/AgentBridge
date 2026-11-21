package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"time"
)

func stdio(args []string, in io.Reader, out, stderr io.Writer, endpoint string) error {
	flags := flag.NewFlagSet("mcp-stdio", flag.ContinueOnError)
	flags.SetOutput(stderr)
	provider := flags.String("provider", "", "session provider")
	workspace := flags.String("workspace", "", "workspace path for fallback routing")
	session := flags.String("session-id", "", "native session ID")
	requireSession := flags.Bool("require-session", false, "require a native session ID from the host or URL")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected mcp-stdio arguments")
	}
	query := make(url.Values)
	if *provider != "" {
		query.Set("provider", *provider)
	}
	if *workspace != "" {
		abs, err := filepath.Abs(*workspace)
		if err != nil {
			return err
		}
		query.Set("workspace", abs)
	}
	if *session != "" {
		query.Set("session_id", *session)
	}
	if *requireSession {
		query.Set("require_session", "true")
	}
	endpoint += "/mcp"
	if len(query) != 0 {
		endpoint += "?" + query.Encode()
	}

	client := httpClient(30*time.Second, time.Second)
	defer client.CloseIdleConnections()
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	version := ""
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(line, &msg)
		data, err := forward(client, endpoint, version, line)
		if err != nil {
			fmt.Fprintln(stderr, "AgentBridge MCP:", err)
			if msg.ID == nil {
				continue
			}
			data, _ = json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": msg.ID,
				"error": map[string]any{"code": -32000, "message": "Could not reach the AgentBridge MCP endpoint"},
			})
		}
		if len(data) == 0 {
			continue
		}
		if msg.Method == "initialize" {
			var reply struct {
				Result struct {
					ProtocolVersion string `json:"protocolVersion"`
				} `json:"result"`
			}
			if json.Unmarshal(data, &reply) == nil {
				version = reply.Result.ProtocolVersion
			}
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, data); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, compact.String()); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func forward(client *http.Client, endpoint, version string, line []byte) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(line))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	var envelope struct {
		JSONRPC string `json:"jsonrpc"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.JSONRPC != "2.0" {
		return nil, errors.New(resp.Status)
	}
	return data, nil
}
