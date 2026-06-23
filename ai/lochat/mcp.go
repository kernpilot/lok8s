package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// lockedBuf is a concurrency-safe sink for the child's stderr so we can quote
// it back in an error if `lo mcp` dies during startup.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) tail() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := strings.TrimSpace(l.b.String())
	if len(s) > 400 {
		s = "…" + s[len(s)-400:]
	}
	return s
}

type rpcMsg struct {
	ID     *int            `json:"id,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

// MCP speaks newline-delimited JSON-RPC 2.0 to `lo mcp` over stdio.
type MCP struct {
	cfg     MCPConfig
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stderr  *lockedBuf
	exited  chan struct{} // closed when the child process exits
	mu      sync.Mutex
	nextID  int
	pending map[int]chan rpcMsg
	timeout time.Duration
}

func newMCP(cfg MCPConfig) *MCP {
	return &MCP{cfg: cfg, pending: map[int]chan rpcMsg{},
		stderr: &lockedBuf{}, exited: make(chan struct{}),
		timeout: time.Duration(cfg.Timeout) * time.Second}
}

func (m *MCP) Start() error {
	if len(m.cfg.Command) == 0 {
		return fmt.Errorf("mcp.command is empty")
	}
	cwd, _ := filepath.Abs(m.cfg.Cwd)
	cmd := exec.Command(m.cfg.Command[0], m.cfg.Command[1:]...)
	cmd.Dir = cwd
	env := os.Environ()
	if len(m.cfg.ExtraPath) > 0 {
		var pref []string
		for _, p := range m.cfg.ExtraPath {
			if !filepath.IsAbs(p) {
				p = filepath.Join(cwd, p)
			}
			pref = append(pref, p)
		}
		env = append(env, "PATH="+strings.Join(pref, ":")+":"+os.Getenv("PATH"))
	}
	for k, v := range m.cfg.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	// Capture the child's stderr so a startup failure (e.g. a missing
	// argsh.so making `mcp` an unknown command) is reported, not swallowed.
	if os.Getenv("LOCHAT_DEBUG") != "" {
		cmd.Stderr = io.MultiWriter(m.stderr, os.Stderr)
	} else {
		cmd.Stderr = m.stderr
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	m.cmd, m.stdin = cmd, stdin
	go m.reader(stdout)
	go func() { _ = cmd.Wait(); close(m.exited) }()

	if _, err := m.request("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "lo-chat", "version": "0.1"},
	}); err != nil {
		return fmt.Errorf("handshake with `%s` failed: %w", strings.Join(m.cfg.Command, " "), err)
	}
	m.notify("notifications/initialized", nil)
	return nil
}

func (m *MCP) reader(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if os.Getenv("LOCHAT_DEBUG") != "" {
			s := line
			if len(s) > 90 {
				s = s[:90]
			}
			fmt.Fprintln(os.Stderr, "[mcp<]", s)
		}
		var msg rpcMsg
		if json.Unmarshal([]byte(line), &msg) != nil || msg.ID == nil {
			continue // log line or server notification
		}
		m.mu.Lock()
		ch := m.pending[*msg.ID]
		delete(m.pending, *msg.ID)
		m.mu.Unlock()
		if ch != nil {
			ch <- msg
		}
	}
}

func (m *MCP) send(v any) error {
	b, _ := json.Marshal(v)
	if os.Getenv("LOCHAT_DEBUG") != "" {
		s := string(b)
		if len(s) > 90 {
			s = s[:90]
		}
		fmt.Fprintln(os.Stderr, "[mcp>]", s)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err := m.stdin.Write(append(b, '\n'))
	return err
}

func (m *MCP) request(method string, params any) (json.RawMessage, error) {
	m.mu.Lock()
	m.nextID++
	id := m.nextID
	ch := make(chan rpcMsg, 1)
	m.pending[id] = ch
	m.mu.Unlock()
	if err := m.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case msg := <-ch:
		if len(msg.Error) > 0 {
			return nil, fmt.Errorf("%s: %s", method, string(msg.Error))
		}
		return msg.Result, nil
	case <-m.exited:
		// drain a response that may have raced in with the exit
		select {
		case msg := <-ch:
			if len(msg.Error) == 0 {
				return msg.Result, nil
			}
		default:
		}
		if errOut := m.stderr.tail(); errOut != "" {
			if strings.Contains(errOut, "Invalid command: mcp") {
				errOut += "\n  hint: `lo mcp` is an argsh.so builtin — run `argsh builtins install` to add it next to argsh"
			}
			return nil, fmt.Errorf("%s: lo mcp exited: %s", method, errOut)
		}
		return nil, fmt.Errorf("%s: lo mcp exited before responding", method)
	case <-time.After(m.timeout):
		return nil, fmt.Errorf("timeout waiting for %s", method)
	}
}

func (m *MCP) notify(method string, params any) {
	_ = m.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (m *MCP) Close() {
	if m.cmd != nil && m.cmd.Process != nil {
		_ = m.cmd.Process.Kill()
	}
}

func (m *MCP) ListTools() ([]Tool, error) {
	var tools []Tool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := m.request("tools/list", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		_ = json.Unmarshal(res, &page)
		for _, t := range page.Tools {
			tools = append(tools, Tool{Name: t.Name, Description: t.Description})
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return tools, nil
}

func (m *MCP) CallTool(name string, args map[string]any) string {
	if args == nil {
		args = map[string]any{}
	}
	res, err := m.request("tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "[error] " + err.Error()
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	_ = json.Unmarshal(res, &out)
	var parts []string
	for _, c := range out.Content {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	text := strings.Join(parts, "\n")
	if out.IsError {
		text = "[tool error] " + text
	}
	if text == "" {
		return "(tool produced no text output)"
	}
	return text
}
