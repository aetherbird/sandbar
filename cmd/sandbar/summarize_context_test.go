package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aetherbird/sandbar/internal/config"
)

func summaryStringPtr(value string) *string { return &value }

func summaryTestConfig(baseURL string) *config.Config {
	return &config.Config{
		Database: "/path/that/must/not/be/opened/sandbar.db",
		Compression: config.CompressionConfig{
			Enabled: true,
			Model:   "configured/model-must-not-win",
		},
		Providers: []config.ProviderConfig{
			{
				Name:    "candidate-provider",
				BaseURL: baseURL,
				APIKey:  "test-key",
				Models: map[string]config.ModelConfig{
					"candidate": {ModelID: summaryStringPtr("candidate-model-id")},
				},
			},
		},
	}
}

func validSummaryCLIRequest() string {
	return `{
  "contract": "sandbar-compression-json/1",
  "messages": [{"role":"user","content":"Inspect src/app.go and preserve the failing test."}],
  "max_output_tokens": 512,
  "minimum_useful_tokens": 0,
  "retry_short": true,
  "timeout_seconds": 5
}`
}

func decodeSummaryEvents(t *testing.T, output string) []map[string]interface{} {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(output))
	var events []map[string]interface{}
	for {
		var event map[string]interface{}
		if err := dec.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode event: %v\noutput: %s", err, output)
		}
		events = append(events, event)
	}
	return events
}

func TestRunSummarizeContextEmitsResultThenDoneWithoutAgentTurn(t *testing.T) {
	var calls atomic.Int32
	var captured map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode provider request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"summary-1","object":"chat.completion","created":1,"model":"candidate-model-id","choices":[{"index":0,"message":{"role":"assistant","content":"TASK AND CONSTRAINTS\nPreserve the failing test.\nNEXT STEPS\nInspect src/app.go."},"finish_reason":"stop"}],"usage":{"prompt_tokens":60,"completion_tokens":15,"total_tokens":75}}`)
	}))
	defer server.Close()

	var output bytes.Buffer
	err := runSummarizeContext(
		summaryTestConfig(server.URL+"/v1"),
		"candidate-provider/candidate",
		nil,
		strings.NewReader(validSummaryCLIRequest()),
		&output,
	)
	if err != nil {
		t.Fatalf("runSummarizeContext: %v\noutput: %s", err, output.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls = %d, want one standalone summary call", calls.Load())
	}
	if captured["model"] != "candidate-model-id" {
		t.Fatalf("explicit candidate did not reach provider: %#v", captured)
	}
	if _, ok := captured["tools"]; ok {
		t.Fatalf("standalone summary unexpectedly exposed tools: %#v", captured)
	}

	events := decodeSummaryEvents(t, output.String())
	if len(events) != 2 {
		t.Fatalf("events = %#v, want summary_result then done", events)
	}
	result := events[0]
	if result["type"] != "summary_result" || result["contract"] != compressionJSONContract {
		t.Fatalf("result event = %#v", result)
	}
	if result["model_alias"] != "candidate-provider/candidate" || result["model_id"] != "candidate-model-id" {
		t.Fatalf("model provenance = %#v", result)
	}
	if result["total_tokens"] != float64(75) || result["summary_call_count"] != float64(1) || result["summary_usage_call_count"] != float64(1) {
		t.Fatalf("usage provenance = %#v", result)
	}
	for _, field := range []string{"content", "local_summary_tokens", "retried", "elapsed_ms", "pruned_tool_outputs", "minimum_useful_tokens_used"} {
		if _, ok := result[field]; !ok {
			t.Fatalf("result omitted required field %q: %#v", field, result)
		}
	}
	if events[1]["type"] != "done" || events[1]["contract"] != compressionJSONContract {
		t.Fatalf("done event = %#v", events[1])
	}
}

func TestRunSummarizeContextRejectsUnknownInputWithoutProviderCall(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	request := strings.TrimSuffix(validSummaryCLIRequest(), "}") + ",\n  \"unexpected\": true\n}"
	var output bytes.Buffer
	err := runSummarizeContext(summaryTestConfig(server.URL+"/v1"), "candidate-provider/candidate", nil, strings.NewReader(request), &output)
	if err == nil {
		t.Fatal("unknown request field unexpectedly succeeded")
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid request reached provider %d times", calls.Load())
	}
	events := decodeSummaryEvents(t, output.String())
	if len(events) != 1 || events[0]["type"] != "error" || events[0]["contract"] != compressionJSONContract {
		t.Fatalf("error events = %#v", events)
	}
}

func TestRunSummarizeContextRequiresExplicitModelAndStdinOnly(t *testing.T) {
	for _, tc := range []struct {
		name       string
		model      string
		positional []string
		want       string
	}{
		{name: "missing model", want: "--model is required"},
		{name: "positional prompt", model: "candidate-provider/candidate", positional: []string{"do not accept this"}, want: "only on stdin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			err := runSummarizeContext(summaryTestConfig("http://127.0.0.1:1/v1"), tc.model, tc.positional, strings.NewReader(validSummaryCLIRequest()), &output)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			events := decodeSummaryEvents(t, output.String())
			if len(events) != 1 || events[0]["type"] != "error" {
				t.Fatalf("events = %#v", events)
			}
		})
	}
}

func TestSummarizeContextCLIBypassesDatabaseAndAgentTurn(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode provider request: %v", err)
		}
		if request["model"] != "candidate-model-id" {
			t.Errorf("provider model = %#v, want explicit candidate", request["model"])
		}
		if _, ok := request["tools"]; ok {
			t.Errorf("standalone CLI exposed agent tools: %#v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"summary-cli","object":"chat.completion","created":1,"model":"candidate-model-id","choices":[{"index":0,"message":{"role":"assistant","content":"TASK AND CONSTRAINTS\nContinue safely.\nNEXT STEPS\nRun the focused test."},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":12,"total_tokens":62}}`)
	}))
	defer server.Close()

	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "must-not-exist.db")
	configPath := filepath.Join(tmp, "config.yaml")
	configText := fmt.Sprintf(`
database: %q
workspace: %q
providers:
  - name: candidate-provider
    base_url: %q
    api_key: test-key
    models:
      candidate:
        model_id: candidate-model-id
      configured-summary:
        model_id: must-not-win
compression:
  enabled: true
  threshold: 0.7
  target_ratio: 0.4
  post_compression_ratio: 0.5
  min_summary_tokens: 1000
  max_summary_tokens: 12000
  timeout_seconds: 400
  model: candidate-provider/configured-summary
`, dbPath, tmp, server.URL+"/v1")
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestSummarizeContextCLIHelper$", "--",
		"--summarize-context", "--json",
		"-model", "candidate-provider/candidate",
		"-config", configPath,
	)
	cmd.Env = append(os.Environ(), "SANDBAR_SUMMARY_CLI_HELPER=1")
	cmd.Stdin = strings.NewReader(validSummaryCLIRequest())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("summary CLI: %v\nstderr: %s\nstdout: %s", err, stderr.String(), stdout.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls = %d, want exactly one summary call", calls.Load())
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("standalone CLI touched database path %q: %v", dbPath, err)
	}
	events := decodeSummaryEvents(t, stdout.String())
	if len(events) != 2 || events[0]["type"] != "summary_result" || events[1]["type"] != "done" {
		t.Fatalf("CLI events = %#v", events)
	}
}

func TestSummarizeContextCLIFailureEmitsErrorAndExitsNonzero(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "must-not-exist.db")
	configPath := filepath.Join(tmp, "config.yaml")
	configText := fmt.Sprintf(`
database: %q
workspace: %q
providers:
  - name: candidate-provider
    base_url: http://127.0.0.1:1/v1
    api_key: test-key
    models:
      candidate:
        model_id: candidate-model-id
`, dbPath, tmp)
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestSummarizeContextCLIHelper$", "--",
		"--summarize-context", "--json", "-config", configPath,
	)
	cmd.Env = append(os.Environ(), "SANDBAR_SUMMARY_CLI_HELPER=1")
	cmd.Stdin = strings.NewReader(validSummaryCLIRequest())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
		t.Fatalf("summary CLI error = %v, want nonzero exit\nstderr: %s\nstdout: %s", err, stderr.String(), stdout.String())
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("failed standalone CLI touched database path %q: %v", dbPath, err)
	}
	events := decodeSummaryEvents(t, stdout.String())
	if len(events) != 1 || events[0]["type"] != "error" || events[0]["contract"] != compressionJSONContract {
		t.Fatalf("failure events = %#v", events)
	}
}

func TestSummarizeContextCLIHelper(t *testing.T) {
	if os.Getenv("SANDBAR_SUMMARY_CLI_HELPER") != "1" {
		return
	}
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 {
		fmt.Fprintln(os.Stderr, "missing helper argument separator")
		os.Exit(2)
	}
	flag.CommandLine = flag.NewFlagSet("sandbar", flag.ContinueOnError)
	os.Args = append([]string{"sandbar"}, os.Args[separator+1:]...)
	main()
	os.Exit(0)
}
