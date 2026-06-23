package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"
)

func TestBInstallArgs(t *testing.T) {
	got := strings.Join(bInstallArgs(), " ")
	for _, want := range []string{"install", "--asset", llamafileAssetGlob, llamafileRepo} {
		if !strings.Contains(got, want) {
			t.Errorf("bInstallArgs = %q, missing %q", got, want)
		}
	}
	if h := bInstallHint(); !strings.HasPrefix(h, "b install ") {
		t.Errorf("bInstallHint = %q, want `b install …`", h)
	}
}

// The asset glob must select ONLY the versioned engine, not the .zip / -thin /
// bench / sibling tools (real asset names from a Mozilla-Ocho/llamafile release).
func TestLlamafileAssetGlobUnambiguous(t *testing.T) {
	assets := []string{
		"diffusionfile-0.10.3", "llamafile-0.10.3", "llamafile-0.10.3-thin",
		"llamafile-0.10.3.zip", "whisperfile-0.10.3", "zipalign-0.10.3",
	}
	var matched []string
	for _, a := range assets {
		if ok, _ := path.Match(llamafileAssetGlob, a); ok {
			matched = append(matched, a)
		}
	}
	if len(matched) != 1 || matched[0] != "llamafile-0.10.3" {
		t.Errorf("glob %q matched %v, want exactly [llamafile-0.10.3]", llamafileAssetGlob, matched)
	}
}

func TestModelPresent(t *testing.T) {
	list := []string{"gemma4:e2b", "qwen2.5-coder:14b", "llama3:latest"}
	cases := []struct {
		m    string
		want bool
	}{
		{"gemma4:e2b", true},
		{"qwen2.5-coder:14b", true},
		{"llama3", true},        // matches :latest
		{"llama3:latest", true}, // exact
		{"gemma4", true},        // family prefix gemma4:
		{"mistral", false},
	}
	for _, c := range cases {
		if got := modelPresent(list, c.m); got != c.want {
			t.Errorf("modelPresent(%q) = %v, want %v", c.m, got, c.want)
		}
	}
}

func ollamaServer(models ...string) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		parts := make([]string, len(models))
		for i, m := range models {
			parts[i] = fmt.Sprintf(`{"name":%q}`, m)
		}
		fmt.Fprintf(w, `{"models":[%s]}`, strings.Join(parts, ","))
	})
	return httptest.NewServer(mux)
}

func TestCheckBackendOllama(t *testing.T) {
	s := ollamaServer("gemma4:e2b", "qwen2.5-coder:14b")
	defer s.Close()

	ready := &httpBackend{name: "local", cfg: &BackendConfig{API: "ollama", BaseURL: s.URL + "/v1", Model: "gemma4:e2b"}}
	if r := checkBackend("local", ready); !r.ok {
		t.Errorf("expected ready, got not-ok: %q", r.detail)
	}

	missing := &httpBackend{name: "x", cfg: &BackendConfig{API: "ollama", BaseURL: s.URL + "/v1", Model: "missing:1b"}}
	r := checkBackend("x", missing)
	if r.ok {
		t.Fatal("missing model should be not-ok")
	}
	if len(r.fix) == 0 || !strings.Contains(r.fix[0], "ollama pull missing:1b") {
		t.Errorf("expected `ollama pull` fix, got %v", r.fix)
	}
}

func TestCheckBackendUnreachable(t *testing.T) {
	be := &httpBackend{name: "local", cfg: &BackendConfig{API: "ollama", BaseURL: "http://127.0.0.1:1/v1", Model: "x"}}
	if r := checkBackend("local", be); r.ok {
		t.Error("unreachable server must be not-ok")
	}
}

func TestCheckBackendOpenAILenient(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"some-loaded.gguf"}]}`)
	})
	s := httptest.NewServer(mux)
	defer s.Close()
	// the configured model name doesn't match what's loaded, but OpenAI-compatible
	// servers (llamafile) serve whatever is loaded -> still ready.
	be := &httpBackend{name: "lf", cfg: &BackendConfig{API: "openai", BaseURL: s.URL + "/v1", Model: "local"}}
	if r := checkBackend("lf", be); !r.ok {
		t.Errorf("openai any-model should be ready, got %q", r.detail)
	}
}

func TestNeedsModelPull(t *testing.T) {
	s := ollamaServer("gemma4:e2b")
	defer s.Close()

	present := &httpBackend{name: "a", cfg: &BackendConfig{API: "ollama", BaseURL: s.URL + "/v1", Model: "gemma4:e2b"}}
	if _, need := needsModelPull(present); need {
		t.Error("present model should not need a pull")
	}
	missing := &httpBackend{name: "b", cfg: &BackendConfig{API: "ollama", BaseURL: s.URL + "/v1", Model: "absent:1b"}}
	if m, need := needsModelPull(missing); !need || m != "absent:1b" {
		t.Errorf("missing model should need pull, got (%q,%v)", m, need)
	}
	down := &httpBackend{name: "c", cfg: &BackendConfig{API: "ollama", BaseURL: "http://127.0.0.1:1/v1", Model: "x"}}
	if _, need := needsModelPull(down); need {
		t.Error("server down: nothing to pull through (no daemon)")
	}
	oai := &httpBackend{name: "d", cfg: &BackendConfig{API: "openai", BaseURL: s.URL + "/v1", Model: "x"}}
	if _, need := needsModelPull(oai); need {
		t.Error("non-ollama backend never needs an ollama pull")
	}
}

func TestCheckBackendCLIMissing(t *testing.T) {
	be := &cliBackend{name: "ghost", cfg: &BackendConfig{Type: "cli", Command: []string{"definitely-not-real-xyz-123"}}}
	if r := checkBackend("ghost", be); r.ok {
		t.Error("missing CLI must be not-ok")
	}
}

func TestPreflightFallsBackToLocalNotCloud(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"m"}]}`)
	}))
	defer up.Close()

	dead := &httpBackend{name: "dead", cfg: &BackendConfig{API: "ollama", BaseURL: "http://127.0.0.1:1/v1", Model: "x"}}
	alive := &httpBackend{name: "alive", cfg: &BackendConfig{API: "openai", BaseURL: up.URL + "/v1", Model: "m"}}
	cloud := &cliBackend{name: "claude", cfg: &BackendConfig{Type: "cli", Command: []string{"definitely-not-real-xyz-123"}}}
	backends := map[string]Backend{"dead": dead, "alive": alive, "claude": cloud}

	got, name := preflightConductor("dead", dead, backends)
	if name != "alive" || got != Backend(alive) {
		t.Errorf("expected fallback to local 'alive', got %q", name)
	}
}

func TestPreflightNoLocalAltStaysPut(t *testing.T) {
	dead := &httpBackend{name: "dead", cfg: &BackendConfig{API: "ollama", BaseURL: "http://127.0.0.1:1/v1", Model: "x"}}
	cloud := &cliBackend{name: "claude", cfg: &BackendConfig{Type: "cli", Command: []string{"definitely-not-real-xyz-123"}}}
	backends := map[string]Backend{"dead": dead, "claude": cloud}
	// no ready local alternative -> must NOT silently switch to the cloud CLI
	got, name := preflightConductor("dead", dead, backends)
	if name != "dead" || got != Backend(dead) {
		t.Errorf("must stay on 'dead' (no auto-route off-machine), got %q", name)
	}
}
