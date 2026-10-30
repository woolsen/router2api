package service

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// probeTestBaseURL 是探测测试使用的公网域名。httptest 的本地证书同时签发了
// *.example.com，配合注入的 Transport 把连接重定向到本地 TLS server，即可同时
// 满足 “https + 公网地址” 的校验与真实 TLS 握手。
const probeTestBaseURL = "https://upstream.example.com"

// newProbeTestServer 启动一个真实的 TLS 上游，并返回把公网域名解析到该上游的
// ProbeService。http://127.0.0.1 会被 urlvalidator 按设计拒绝，不能当桩。
func newProbeTestServer(t *testing.T, handler http.Handler) *ProbeService {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)

	baseTransport, ok := srv.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("httptest client transport = %T, want *http.Transport", srv.Client().Transport)
	}
	transport := baseTransport.Clone()
	target := srv.Listener.Addr().String()
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, target)
	}
	return NewProbeServiceWithHTTPClient(&http.Client{Transport: transport})
}

// fakeOpenAIUpstream 模拟 OpenAI 兼容上游。usage / sse 控制四项探测中的
// “返回 usage” 与 “流式首个 chunk”。
func fakeOpenAIUpstream(t *testing.T, usage, sse bool) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-probe" {
			t.Errorf("Authorization = %q, want Bearer sk-probe", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"gpt-4o-mini"},{"id":"text-embedding-3-small"}]}`)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode probe body: %v", err)
		}
		if body.Stream {
			if !sse {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"pong"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"p\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		if usage {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"pong"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"pong"}}]}`)
	})
	return mux
}

func TestProbeAcceptsMeasurableStreamingUpstream(t *testing.T) {
	probe := newProbeTestServer(t, fakeOpenAIUpstream(t, true, true))
	res, err := probe.Probe(context.Background(), probeTestBaseURL, "sk-probe", PlatformOpenAI)
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if len(res.Models) != 2 || res.Models[0] != "gpt-4o-mini" {
		t.Fatalf("models = %v, want upstream model list", res.Models)
	}
	if !res.ReturnsUsage {
		t.Fatal("ReturnsUsage = false, want true")
	}
	if !res.SupportsStream {
		t.Fatal("SupportsStream = false, want true")
	}
	if res.LatencyMS < 0 {
		t.Fatalf("LatencyMS = %d, want >= 0", res.LatencyMS)
	}
}

func TestProbeRejectsUpstreamWithoutUsage(t *testing.T) {
	probe := newProbeTestServer(t, fakeOpenAIUpstream(t, false, true))
	res, err := probe.Probe(context.Background(), probeTestBaseURL, "sk-probe", PlatformOpenAI)
	if err == nil || err.Error() != "upstream_returns_no_usage" {
		t.Fatalf("want upstream_returns_no_usage, got %v / %+v", err, res)
	}
}

func TestProbeRejectsEmptyModelList(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[]}`)
	})
	probe := newProbeTestServer(t, mux)

	res, err := probe.Probe(context.Background(), probeTestBaseURL, "sk-probe", PlatformOpenAI)
	if err == nil || err.Error() != "models_empty" {
		t.Fatalf("want models_empty, got %v / %+v", err, res)
	}
}

func TestProbeRejectsNonStreamingUpstream(t *testing.T) {
	probe := newProbeTestServer(t, fakeOpenAIUpstream(t, true, false))
	res, err := probe.Probe(context.Background(), probeTestBaseURL, "sk-probe", PlatformOpenAI)
	if err == nil || err.Error() != "stream_unsupported" {
		t.Fatalf("want stream_unsupported, got %v / %+v", err, res)
	}
}

func TestProbeRejectsNonHTTPSAndPrivateIP(t *testing.T) {
	probe := NewProbeService()
	cases := []string{
		"http://127.0.0.1:8080",
		"https://127.0.0.1:8443",
		"https://10.0.0.8",
		"https://localhost:8443",
		"https://[::1]:8443",
	}
	for _, baseURL := range cases {
		if _, err := probe.Probe(context.Background(), baseURL, "k", PlatformOpenAI); err == nil || err.Error() != "insecure_url" {
			t.Fatalf("Probe(%q) error = %v, want insecure_url", baseURL, err)
		}
	}
}

func TestProbeRejectsUnknownPlatform(t *testing.T) {
	probe := NewProbeService()
	_, err := probe.Probe(context.Background(), probeTestBaseURL, "k", "gemini")
	if err == nil {
		t.Fatal("unknown platform must be rejected before any network call")
	}
}

func TestProbeTrimsTrailingSlashAndQuotesAPIKey(t *testing.T) {
	var gotPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"data":[{"id":"gpt-4o-mini"}]}`)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"p\"}}]}\n\n")
			return
		}
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	})
	probe := newProbeTestServer(t, mux)
	if _, err := probe.Probe(context.Background(), probeTestBaseURL+"/", " sk-probe ", PlatformOpenAI); err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if gotPath != "/v1/models" {
		t.Fatalf("models path = %q, want /v1/models", gotPath)
	}
}

func TestProbeAcceptsBaseURLWithV1Suffix(t *testing.T) {
	var paths []string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = io.WriteString(w, `{"data":[{"id":"gpt-4o-mini"}]}`)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"p\"}}]}\n\n")
			return
		}
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	})
	probe := newProbeTestServer(t, mux)
	if _, err := probe.Probe(context.Background(), probeTestBaseURL+"/v1", "sk-probe", PlatformOpenAI); err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	for _, path := range paths {
		if strings.Contains(path, "/v1/v1") {
			t.Fatalf("path = %q, duplicated version segment", path)
		}
	}
}

func TestProbeAnthropicUsesMessagesAPI(t *testing.T) {
	var chatCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "sk-ant" {
			t.Errorf("x-api-key = %q, want sk-ant", got)
		}
		if got := r.Header.Get("anthropic-version"); got == "" {
			t.Error("anthropic-version header is required")
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"claude-sonnet-4-5"}]}`)
	})
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		chatCalls++
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
			return
		}
		_, _ = io.WriteString(w, `{"type":"message","usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	probe := newProbeTestServer(t, mux)
	res, err := probe.Probe(context.Background(), probeTestBaseURL, "sk-ant", PlatformAnthropic)
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if !res.ReturnsUsage || !res.SupportsStream {
		t.Fatalf("result = %+v, want usage+stream", res)
	}
	if chatCalls != 2 {
		t.Fatalf("messages calls = %d, want 2", chatCalls)
	}
}
