package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/woolsen/router2api/internal/util/urlvalidator"
)

const (
	// probeStepTimeout 是四项探测中每一步的上限；总预算 15s 覆盖最坏情况。
	probeStepTimeout  = 5 * time.Second
	probeTotalTimeout = 15 * time.Second
	// probeBodyLimit 限制单个上游响应体，避免探测被超大响应拖住。
	probeBodyLimit = 2 << 20
)

// 探测失败的错误码会直接展示给申请者，必须保持稳定且可读。
var (
	ErrProbeInsecureURL       = errors.New("insecure_url")
	ErrProbeModelsEmpty       = errors.New("models_empty")
	ErrProbeReturnsNoUsage    = errors.New("upstream_returns_no_usage")
	ErrProbeStreamUnsupported = errors.New("stream_unsupported")
)

// ProbeResult 是四项探测全部通过后的结论，可直接用于建组与上架。
type ProbeResult struct {
	Models         []string
	SupportsStream bool
	ReturnsUsage   bool
	LatencyMS      int
}

// ProbeService 对商户上游做四项探测：https/公网地址、模型列表、usage、流式。
// 任一项失败即短路，错误码固定为 ErrProbe*。
type ProbeService struct {
	client *http.Client
}

// NewProbeService 创建使用默认 HTTP 客户端的探测器。
func NewProbeService() *ProbeService {
	return NewProbeServiceWithHTTPClient(&http.Client{})
}

// NewProbeServiceWithHTTPClient 允许注入 HTTP 客户端（测试用 Transport 指向本地 TLS 上游）。
func NewProbeServiceWithHTTPClient(client *http.Client) *ProbeService {
	if client == nil {
		client = &http.Client{}
	}
	return &ProbeService{client: client}
}

// Probe 依次执行四项探测并在第一个失败项处短路。
//
// baseURL 必须是 https 且不是内网/回环地址；platform 只接受 openai/anthropic。
// 模型列表中的第一个可对话模型用于 usage 与流式探测。
func (s *ProbeService) Probe(ctx context.Context, baseURL, apiKey, platform string) (ProbeResult, error) {
	started := time.Now()
	platform = strings.ToLower(strings.TrimSpace(platform))
	if platform != PlatformOpenAI && platform != PlatformAnthropic {
		return ProbeResult{}, ErrPlatformMismatch
	}

	normalized, err := urlvalidator.ValidateHTTPSURL(baseURL, urlvalidator.ValidationOptions{})
	if err != nil {
		return ProbeResult{}, ErrProbeInsecureURL
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		// 空 key 无法完成任何上游调用；按模型列表失败处理，错误码不额外扩张。
		return ProbeResult{}, ErrProbeModelsEmpty
	}

	ctx, cancel := context.WithTimeout(ctx, probeTotalTimeout)
	defer cancel()

	models, err := s.listModels(ctx, normalized, apiKey, platform)
	if err != nil || len(models) == 0 {
		return ProbeResult{}, ErrProbeModelsEmpty
	}
	result := ProbeResult{Models: models}

	model := pickProbeModel(models)
	if !s.checkReturnsUsage(ctx, normalized, apiKey, platform, model) {
		return result, ErrProbeReturnsNoUsage
	}
	result.ReturnsUsage = true

	if !s.checkStream(ctx, normalized, apiKey, platform, model) {
		return result, ErrProbeStreamUnsupported
	}
	result.SupportsStream = true
	result.LatencyMS = int(time.Since(started).Milliseconds())
	return result, nil
}

func (s *ProbeService) listModels(ctx context.Context, baseURL, apiKey, platform string) ([]string, error) {
	stepCtx, cancel := context.WithTimeout(ctx, probeStepTimeout)
	defer cancel()

	url := buildOpenAIModelsURL(baseURL)
	req, err := http.NewRequestWithContext(stepCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	setProbeAuthHeaders(req, apiKey, platform)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("models status %d", resp.StatusCode)
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, probeBodyLimit)).Decode(&payload); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(payload.Data)+len(payload.Models))
	seen := make(map[string]struct{}, cap(models))
	for _, item := range payload.Data {
		appendProbeModel(&models, seen, item.ID)
	}
	for _, item := range payload.Models {
		appendProbeModel(&models, seen, item.ID)
	}
	return models, nil
}

func appendProbeModel(models *[]string, seen map[string]struct{}, raw string) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return
	}
	key := strings.ToLower(name)
	if _, ok := seen[key]; ok {
		return
	}
	seen[key] = struct{}{}
	*models = append(*models, name)
}

// pickProbeModel 跳过明显不可对话的模型，避免模型列表恰好以 embedding 开头时误判。
func pickProbeModel(models []string) string {
	nonChatHints := []string{"embedding", "whisper", "tts", "dall-e", "moderation", "rerank"}
	for _, model := range models {
		lower := strings.ToLower(model)
		skip := false
		for _, hint := range nonChatHints {
			if strings.Contains(lower, hint) {
				skip = true
				break
			}
		}
		if !skip {
			return model
		}
	}
	return models[0]
}

func (s *ProbeService) checkReturnsUsage(ctx context.Context, baseURL, apiKey, platform, model string) bool {
	stepCtx, cancel := context.WithTimeout(ctx, probeStepTimeout)
	defer cancel()

	body, url := probeChatRequest(platform, baseURL, model, false)
	req, err := http.NewRequestWithContext(stepCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	setProbeAuthHeaders(req, apiKey, platform)

	resp, err := s.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}

	var payload map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, probeBodyLimit)).Decode(&payload); err != nil {
		return false
	}
	raw, ok := payload["usage"]
	if !ok {
		return false
	}
	var usage map[string]any
	if err := json.Unmarshal(raw, &usage); err != nil {
		return false
	}
	// 空对象等于没有计量字段，不能把 {} 当成有 usage。
	return len(usage) > 0
}

func (s *ProbeService) checkStream(ctx context.Context, baseURL, apiKey, platform, model string) bool {
	stepCtx, cancel := context.WithTimeout(ctx, probeStepTimeout)
	defer cancel()

	body, url := probeChatRequest(platform, baseURL, model, true)
	req, err := http.NewRequestWithContext(stepCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	setProbeAuthHeaders(req, apiKey, platform)

	resp, err := s.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}

	scanner := bufio.NewScanner(io.LimitReader(resp.Body, probeBodyLimit))
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		chunk := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if chunk == "" || chunk == "[DONE]" {
			continue
		}
		// 至少收到一个真实 chunk（不是只有结束标记）。
		return true
	}
	return false
}

func probeChatRequest(platform, baseURL, model string, stream bool) ([]byte, string) {
	if platform == PlatformAnthropic {
		body, _ := json.Marshal(map[string]any{
			"model":      model,
			"max_tokens": 1,
			"stream":     stream,
			"messages": []map[string]string{
				{"role": "user", "content": "ping"},
			},
		})
		return body, buildOpenAIEndpointURL(baseURL, "/v1/messages")
	}
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1,
		"stream":     stream,
		"messages": []map[string]string{
			{"role": "user", "content": "ping"},
		},
	})
	return body, buildOpenAIChatCompletionsURL(baseURL)
}

func setProbeAuthHeaders(req *http.Request, apiKey, platform string) {
	if platform == PlatformAnthropic {
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		return
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
}
