package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type Msg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Backend: a conductor brain. HTTP backends are "agentic" (driven through the
// route/execute loop); CLI backends are a direct handoff.
type Backend interface {
	Label() string
	Available() bool
	Agentic() bool
	Complete(msgs []Msg) (string, error)             // full response (routing)
	Stream(msgs []Msg) (<-chan string, <-chan error) // token stream (answer)
}

func makeBackend(name string, b *BackendConfig) Backend {
	if b.Type == "cli" {
		return &cliBackend{name: name, cfg: b}
	}
	return &httpBackend{name: name, cfg: b}
}

// --------------------------------------------------------------------------
type httpBackend struct {
	name     string
	cfg      *BackendConfig
	mu       sync.Mutex
	probed   bool
	probedAt time.Time
}

func (h *httpBackend) Label() string { return h.name + " (" + h.cfg.Model + ")" }
func (h *httpBackend) Agentic() bool { return true }

func (h *httpBackend) ollama() bool         { return h.cfg.API == "ollama" }
func (h *httpBackend) endpointBase() string { return strings.TrimSuffix(h.cfg.BaseURL, "/v1") }

// Available probes the server (result cached briefly) so /models, cycling and
// the startup preflight reflect reality without hammering the endpoint.
func (h *httpBackend) Available() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.probedAt.IsZero() && time.Since(h.probedAt) < 3*time.Second {
		return h.probed
	}
	_, err := h.listModels(700 * time.Millisecond)
	h.probed, h.probedAt = err == nil, time.Now()
	return h.probed
}

// listModels returns the model ids the server currently serves (ollama
// /api/tags, or OpenAI-compatible /v1/models for llamafile/vLLM/LM Studio/etc.).
func (h *httpBackend) listModels(timeout time.Duration) ([]string, error) {
	url := h.cfg.BaseURL + "/models"
	if h.ollama() {
		url = h.endpointBase() + "/api/tags"
	}
	req, _ := http.NewRequest("GET", url, nil)
	if h.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+h.cfg.APIKey)
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if h.ollama() {
		var o struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		json.NewDecoder(resp.Body).Decode(&o)
		ns := make([]string, 0, len(o.Models))
		for _, m := range o.Models {
			ns = append(ns, m.Name)
		}
		return ns, nil
	}
	var o struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&o)
	ns := make([]string, 0, len(o.Data))
	for _, m := range o.Data {
		ns = append(ns, m.ID)
	}
	return ns, nil
}

func (h *httpBackend) body(msgs []Msg, stream bool) (string, map[string]any) {
	if h.ollama() {
		opts := map[string]any{"temperature": h.cfg.Temperature}
		if h.cfg.NumCtx > 0 {
			opts["num_ctx"] = h.cfg.NumCtx
		}
		b := map[string]any{"model": h.cfg.Model, "messages": msgs, "stream": stream, "options": opts}
		if h.cfg.Think != nil {
			b["think"] = *h.cfg.Think
		}
		return strings.TrimSuffix(h.cfg.BaseURL, "/v1") + "/api/chat", b
	}
	return h.cfg.BaseURL + "/chat/completions",
		map[string]any{"model": h.cfg.Model, "messages": msgs, "stream": stream, "temperature": h.cfg.Temperature}
}

func (h *httpBackend) post(url string, body map[string]any, streaming bool) (*http.Response, error) {
	bs, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	if h.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+h.cfg.APIKey)
	}
	to := h.cfg.Timeout
	if to == 0 {
		to = 300
	}
	if streaming {
		to = 600
	}
	resp, err := (&http.Client{Timeout: time.Duration(to) * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("%d: %s", resp.StatusCode, string(b[:min(len(b), 300)]))
	}
	return resp, nil
}

func (h *httpBackend) Complete(msgs []Msg) (string, error) {
	url, body := h.body(msgs, false)
	resp, err := h.post(url, body, false)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if h.ollama() {
		var out struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		json.NewDecoder(resp.Body).Decode(&out)
		return out.Message.Content, nil
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Choices) > 0 {
		return out.Choices[0].Message.Content, nil
	}
	return "", nil
}

func (h *httpBackend) Stream(msgs []Msg) (<-chan string, <-chan error) {
	toks := make(chan string)
	errc := make(chan error, 1)
	go func() {
		defer close(toks)
		defer close(errc)
		url, body := h.body(msgs, true)
		resp, err := h.post(url, body, true)
		if err != nil {
			errc <- err
			return
		}
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 8<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			if h.ollama() {
				var ch struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
					Done bool `json:"done"`
				}
				if json.Unmarshal([]byte(line), &ch) != nil {
					continue
				}
				if ch.Message.Content != "" {
					toks <- ch.Message.Content
				}
				if ch.Done {
					return
				}
			} else {
				if !strings.HasPrefix(line, "data:") {
					continue
				}
				data := strings.TrimSpace(line[5:])
				if data == "[DONE]" {
					return
				}
				var ch struct {
					Choices []struct {
						Delta struct {
							Content string `json:"content"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if json.Unmarshal([]byte(data), &ch) != nil {
					continue
				}
				if len(ch.Choices) > 0 && ch.Choices[0].Delta.Content != "" {
					toks <- ch.Choices[0].Delta.Content
				}
			}
		}
	}()
	return toks, errc
}

// --------------------------------------------------------------------------
type cliBackend struct {
	name string
	cfg  *BackendConfig
}

func (c *cliBackend) Label() string {
	return c.name + " (cli: " + strings.Join(c.cfg.Command, " ") + ")"
}
func (c *cliBackend) Agentic() bool { return false }

func (c *cliBackend) detect() string {
	if c.cfg.Detect != "" {
		return c.cfg.Detect
	}
	if len(c.cfg.Command) > 0 {
		return c.cfg.Command[0]
	}
	return ""
}

func (c *cliBackend) Available() bool {
	_, err := exec.LookPath(c.detect())
	return err == nil
}

func (c *cliBackend) Complete(msgs []Msg) (string, error) {
	var sb strings.Builder
	toks, errc := c.Stream(msgs)
	for t := range toks {
		sb.WriteString(t)
	}
	return sb.String(), <-errc
}

func (c *cliBackend) Stream(msgs []Msg) (<-chan string, <-chan error) {
	toks := make(chan string)
	errc := make(chan error, 1)
	go func() {
		defer close(toks)
		defer close(errc)
		cmd := exec.Command(c.cfg.Command[0], c.cfg.Command[1:]...)
		cmd.Stdin = strings.NewReader(flatten(msgs))
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			errc <- err
			return
		}
		if err := cmd.Start(); err != nil {
			errc <- err
			return
		}
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 8<<20)
		for sc.Scan() {
			toks <- sc.Text() + "\n"
		}
		cmd.Wait()
	}()
	return toks, errc
}

func flatten(msgs []Msg) string {
	var sb strings.Builder
	for _, m := range msgs {
		if m.Role == "system" {
			sb.WriteString(m.Content)
		} else {
			role := strings.ToUpper(m.Role[:1]) + m.Role[1:]
			sb.WriteString(role + ": " + m.Content)
		}
		sb.WriteString("\n\n")
	}
	return sb.String()
}
