package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/woolsen/router2api/internal/pkg/ctxkey"
)

// fakeMetricRepo 记录 MetricsService 后台落库的批次，供测试断言。
type fakeMetricRepo struct {
	mu      sync.Mutex
	rows    []RequestMetric
	inserts int
	err     error
}

func newFakeMetricRepo() *fakeMetricRepo {
	return &fakeMetricRepo{}
}

func (r *fakeMetricRepo) InsertRequestMetrics(_ context.Context, metrics []RequestMetric) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.inserts++
	r.rows = append(r.rows, metrics...)
	return nil
}

func (r *fakeMetricRepo) rowsSnapshot() []RequestMetric {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RequestMetric, len(r.rows))
	copy(out, r.rows)
	return out
}

func TestRecordMetricIsNonBlockingAndBounded(t *testing.T) {
	svc := NewMetricsService(newFakeMetricRepo(), MetricsOptions{Buffer: 2})
	defer func() { _ = svc.Close(context.Background()) }()

	start := time.Now()
	for i := 0; i < 10; i++ {
		svc.Record(context.Background(), RequestMetric{RequestID: fmt.Sprintf("r%d", i)})
	}
	// 不阻塞、不 panic、丢弃计数可观测
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Record blocked for %s", elapsed)
	}
	if svc.Dropped() < 8 {
		t.Fatalf("want dropped >= 8, got %d", svc.Dropped())
	}
}

func TestMetricClassifiesUserErrorVsUpstreamFailure(t *testing.T) {
	if !ClassifyUpstreamFailure(502) {
		t.Fatal("502 must be upstream failure")
	}
	if ClassifyUpstreamFailure(400) {
		t.Fatal("400 is a user error, not upstream failure")
	}
	if !ClassifyUpstreamFailure(0) {
		t.Fatal("timeout (0) must be upstream failure")
	}
	if ClassifyUpstreamFailure(429) {
		t.Fatal("429 is a client-side error, not upstream failure")
	}
	if ClassifyUpstreamFailure(401) {
		t.Fatal("401 is a user error, not upstream failure")
	}
	if ClassifyUpstreamFailure(200) {
		t.Fatal("200 is not a failure")
	}
	if !ClassifyUpstreamFailure(500) {
		t.Fatal("500 must be upstream failure")
	}
}

func TestMetricsServicePersistsBufferedMetrics(t *testing.T) {
	repo := newFakeMetricRepo()
	svc := NewMetricsService(repo, MetricsOptions{
		Buffer:        16,
		FlushInterval: 5 * time.Millisecond,
	})
	svc.Start()

	merchantID := int64(42)
	svc.Record(context.Background(), RequestMetric{
		RequestID:           "req-ok",
		UserID:              1,
		APIKeyID:            2,
		GroupID:             3,
		MerchantID:          &merchantID,
		Model:               "claude-sonnet-4-5",
		StatusCode:          200,
		DurationMS:          1200,
		FirstTokenMS:        300,
		InputTokens:         1000,
		OutputTokens:        200,
		CacheReadTokens:     800,
		CacheCreationTokens: 50,
	})
	svc.Record(context.Background(), RequestMetric{
		RequestID:     "req-fail",
		UserID:        1,
		APIKeyID:      2,
		GroupID:       3,
		Model:         "claude-sonnet-4-5",
		StatusCode:    502,
		UpstreamError: true,
		DurationMS:    900,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := svc.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rows := repo.rowsSnapshot()
	if len(rows) != 2 {
		t.Fatalf("want 2 persisted rows, got %d", len(rows))
	}
	byID := make(map[string]RequestMetric, len(rows))
	for _, row := range rows {
		byID[row.RequestID] = row
	}
	ok, exists := byID["req-ok"]
	if !exists {
		t.Fatal("req-ok not persisted")
	}
	if ok.GroupID != 3 || ok.MerchantID == nil || *ok.MerchantID != merchantID {
		t.Fatalf("unexpected ownership snapshot: %+v", ok)
	}
	if ok.DurationMS != 1200 || ok.FirstTokenMS != 300 {
		t.Fatalf("unexpected latency: %+v", ok)
	}
	if ok.InputTokens != 1000 || ok.OutputTokens != 200 || ok.CacheReadTokens != 800 || ok.CacheCreationTokens != 50 {
		t.Fatalf("unexpected token snapshot: %+v", ok)
	}
	fail, exists := byID["req-fail"]
	if !exists {
		t.Fatal("req-fail not persisted")
	}
	if !fail.UpstreamError || fail.StatusCode != 502 {
		t.Fatalf("unexpected failure classification: %+v", fail)
	}
}

func TestMetricsServiceFillsRequestIDFromContext(t *testing.T) {
	repo := newFakeMetricRepo()
	svc := NewMetricsService(repo, MetricsOptions{Buffer: 8, FlushInterval: 5 * time.Millisecond})
	svc.Start()

	ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "client-abc")
	svc.Record(ctx, RequestMetric{GroupID: 7, StatusCode: 200})

	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := svc.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rows := repo.rowsSnapshot()
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].RequestID != "client:client-abc" {
		t.Fatalf("want request id filled from context, got %q", rows[0].RequestID)
	}
}

func TestMetricsServiceCountsDroppedAfterClose(t *testing.T) {
	svc := NewMetricsService(newFakeMetricRepo(), MetricsOptions{Buffer: 2})
	if err := svc.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	svc.Record(context.Background(), RequestMetric{RequestID: "late"})
	if svc.Dropped() != 1 {
		t.Fatalf("want dropped=1 after close, got %d", svc.Dropped())
	}
}

func TestMetricsServiceCountsFailedInserts(t *testing.T) {
	repo := newFakeMetricRepo()
	repo.err = errors.New("db down")
	svc := NewMetricsService(repo, MetricsOptions{Buffer: 4, FlushInterval: 5 * time.Millisecond})
	svc.Start()
	svc.Record(context.Background(), RequestMetric{RequestID: "boom"})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := svc.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if svc.Failed() == 0 {
		t.Fatal("want failed counter to be observable")
	}
}
