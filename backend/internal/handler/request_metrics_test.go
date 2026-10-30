package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/woolsen/router2api/internal/pkg/ctxkey"
	middleware2 "github.com/woolsen/router2api/internal/server/middleware"
	"github.com/woolsen/router2api/internal/service"

	"github.com/gin-gonic/gin"
)

type recorderTestMetricRepo struct {
	mu   sync.Mutex
	rows []service.RequestMetric
}

func (r *recorderTestMetricRepo) InsertRequestMetrics(_ context.Context, metrics []service.RequestMetric) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, metrics...)
	return nil
}

func (r *recorderTestMetricRepo) rowsSnapshot() []service.RequestMetric {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]service.RequestMetric, len(r.rows))
	copy(out, r.rows)
	return out
}

func newRecorderTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c, writer
}

func newRecorderTestAPIKey() *service.APIKey {
	groupID := int64(7)
	merchantID := int64(99)
	return &service.APIKey{
		ID:      5,
		GroupID: &groupID,
		Group: &service.Group{
			ID:         groupID,
			MerchantID: &merchantID,
		},
	}
}

func flushRecorderMetrics(t *testing.T, svc *service.MetricsService, repo *recorderTestMetricRepo) []service.RequestMetric {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := svc.Close(ctx); err != nil {
		t.Fatalf("close metrics service: %v", err)
	}
	return repo.rowsSnapshot()
}

func TestRequestMetricRecorderUsesWrittenStatus(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		write        bool
		markStream   func(*gin.Context)
		wantStatus   int
		wantUpstream bool
	}{
		{name: "success", status: http.StatusOK, write: true, wantStatus: 200, wantUpstream: false},
		{name: "user error", status: http.StatusBadRequest, write: true, wantStatus: 400, wantUpstream: false},
		{name: "upstream failure", status: http.StatusBadGateway, write: true, wantStatus: 502, wantUpstream: true},
		{name: "timeout before write", status: http.StatusOK, write: false, wantStatus: 0, wantUpstream: true},
		{
			name:   "stream failure after 200 ping",
			status: http.StatusOK,
			write:  true,
			markStream: func(c *gin.Context) {
				service.MarkOpsStreamFailure(c, "upstream_error", "stream_failed", "upstream stream failed", http.StatusBadGateway)
			},
			wantStatus:   200,
			wantUpstream: true,
		},
		{
			name:   "in-band upstream error after 200 data",
			status: http.StatusOK,
			write:  true,
			markStream: func(c *gin.Context) {
				// handleStreamingAwareErrorWithCode 走 MarkOpsStreamError（非 SLA 版本）。
				service.MarkOpsStreamError(c, "upstream_error", "upstream stream failed", http.StatusBadGateway)
			},
			wantStatus:   200,
			wantUpstream: true,
		},
		{
			name:   "in-band user error after 200 ping",
			status: http.StatusOK,
			write:  true,
			markStream: func(c *gin.Context) {
				service.MarkOpsStreamError(c, "invalid_request_error", "bad request", http.StatusBadRequest)
			},
			wantStatus:   200,
			wantUpstream: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &recorderTestMetricRepo{}
			svc := service.NewMetricsService(repo, service.MetricsOptions{Buffer: 4, FlushInterval: time.Millisecond})
			svc.Start()

			c, _ := newRecorderTestContext()
			recorder := newRequestMetricRecorder()
			recorder.Configure(svc, newRecorderTestAPIKey(), 11)
			recorder.SetModel("claude-sonnet-4-5")
			if tt.write {
				c.Status(tt.status)
				c.Writer.WriteHeaderNow()
			}
			if tt.markStream != nil {
				tt.markStream(c)
			}
			recorder.Finish(c)

			rows := flushRecorderMetrics(t, svc, repo)
			if len(rows) != 1 {
				t.Fatalf("want 1 metric, got %d", len(rows))
			}
			row := rows[0]
			if row.StatusCode != tt.wantStatus {
				t.Fatalf("status_code=%d, want %d", row.StatusCode, tt.wantStatus)
			}
			if row.UpstreamError != tt.wantUpstream {
				t.Fatalf("upstream_error=%v, want %v", row.UpstreamError, tt.wantUpstream)
			}
			if row.GroupID != 7 || row.MerchantID == nil || *row.MerchantID != 99 {
				t.Fatalf("ownership snapshot wrong: %+v", row)
			}
			if row.UserID != 11 || row.APIKeyID != 5 {
				t.Fatalf("identity wrong: %+v", row)
			}
			if row.Model != "claude-sonnet-4-5" {
				t.Fatalf("model=%q", row.Model)
			}
		})
	}
}

func TestRequestMetricRecorderObservesForwardResult(t *testing.T) {
	repo := &recorderTestMetricRepo{}
	svc := service.NewMetricsService(repo, service.MetricsOptions{Buffer: 4, FlushInterval: time.Millisecond})
	svc.Start()

	c, _ := newRecorderTestContext()
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.ClientRequestID, "client-abc"))
	recorder := newRequestMetricRecorder()
	recorder.Configure(svc, newRecorderTestAPIKey(), 11)
	firstTokenMS := 250
	recorder.ObserveForward(&service.ForwardResult{
		RequestID:    "upstream-request-1",
		Duration:     1500 * time.Millisecond,
		FirstTokenMs: &firstTokenMS,
		Usage: service.ClaudeUsage{
			InputTokens:              1000,
			OutputTokens:             200,
			CacheReadInputTokens:     700,
			CacheCreationInputTokens: 30,
		},
	})
	c.Status(http.StatusOK)
	c.Writer.WriteHeaderNow()
	recorder.Finish(c)

	rows := flushRecorderMetrics(t, svc, repo)
	if len(rows) != 1 {
		t.Fatalf("want 1 metric, got %d", len(rows))
	}
	row := rows[0]
	if row.RequestID != "client:client-abc" {
		t.Fatalf("request_id=%q, want usage_logs-compatible client:client-abc", row.RequestID)
	}
	if row.UpstreamRequestID != "upstream-request-1" {
		t.Fatalf("upstream_request_id=%q", row.UpstreamRequestID)
	}
	if row.DurationMS != 1500 || row.FirstTokenMS != 250 {
		t.Fatalf("latency snapshot wrong: %+v", row)
	}
	if row.InputTokens != 1000 || row.OutputTokens != 200 || row.CacheReadTokens != 700 || row.CacheCreationTokens != 30 {
		t.Fatalf("token snapshot wrong: %+v", row)
	}
}

func TestRequestMetricRecorderSetGroupFollowsFallback(t *testing.T) {
	repo := &recorderTestMetricRepo{}
	svc := service.NewMetricsService(repo, service.MetricsOptions{Buffer: 4, FlushInterval: time.Millisecond})
	svc.Start()

	c, _ := newRecorderTestContext()
	recorder := newRequestMetricRecorder()
	recorder.Configure(svc, newRecorderTestAPIKey(), 11)

	fallbackGroupID := int64(8)
	fallbackMerchantID := int64(100)
	recorder.SetGroup(&service.APIKey{
		ID:      6,
		GroupID: &fallbackGroupID,
		Group:   &service.Group{ID: fallbackGroupID, MerchantID: &fallbackMerchantID},
	})
	c.Status(http.StatusOK)
	c.Writer.WriteHeaderNow()
	recorder.Finish(c)

	rows := flushRecorderMetrics(t, svc, repo)
	if len(rows) != 1 {
		t.Fatalf("want 1 metric, got %d", len(rows))
	}
	if rows[0].GroupID != 8 || rows[0].MerchantID == nil || *rows[0].MerchantID != 100 {
		t.Fatalf("fallback ownership not applied: %+v", rows[0])
	}
}

func TestRequestMetricRecorderWithoutServiceIsNoop(t *testing.T) {
	c, _ := newRecorderTestContext()
	recorder := newRequestMetricRecorder()
	recorder.Configure(nil, newRecorderTestAPIKey(), 11)
	c.Status(http.StatusInternalServerError)
	c.Writer.WriteHeaderNow()
	recorder.Finish(c) // 不得 panic
}

// TestGatewayResponsesRecordsRequestMetric 覆盖 Anthropic 平台分组 /v1/responses
// 入口：空请求体在鉴权后立即返回 400，但 recorder 仍应落一行可归因指标。
func TestGatewayResponsesRecordsRequestMetric(t *testing.T) {
	repo := &recorderTestMetricRepo{}
	svc := service.NewMetricsService(repo, service.MetricsOptions{Buffer: 4, FlushInterval: time.Millisecond})
	svc.Start()

	h := &GatewayHandler{metricsService: svc}
	c, _ := newRecorderTestContext()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), newRecorderTestAPIKey())
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 11})

	h.Responses(c)

	rows := flushRecorderMetrics(t, svc, repo)
	if len(rows) != 1 {
		t.Fatalf("want 1 metric from GatewayHandler.Responses, got %d", len(rows))
	}
	row := rows[0]
	if row.StatusCode != http.StatusBadRequest || row.UpstreamError {
		t.Fatalf("unexpected outcome: %+v", row)
	}
	if row.UserID != 11 || row.APIKeyID != 5 || row.GroupID != 7 || row.MerchantID == nil || *row.MerchantID != 99 {
		t.Fatalf("ownership snapshot wrong: %+v", row)
	}
}
