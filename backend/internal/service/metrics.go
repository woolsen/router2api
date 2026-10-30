package service

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/woolsen/router2api/internal/pkg/logger"
	"go.uber.org/zap"
)

const (
	// defaultRequestMetricBuffer 是请求指标缓冲队列的默认容量。
	// 队列满时直接丢弃并计数，绝不阻塞网关响应。
	defaultRequestMetricBuffer = 8192
	// defaultRequestMetricBatchSize 是单次落库的最大行数。
	defaultRequestMetricBatchSize = 128
	// defaultRequestMetricFlushInterval 是批次的最长等待时间。
	defaultRequestMetricFlushInterval = 500 * time.Millisecond
	// defaultRequestMetricInsertTimeout 是单次落库的超时时间。
	defaultRequestMetricInsertTimeout = 5 * time.Second

	requestMetricRequestIDMaxLen         = 64
	requestMetricUpstreamRequestIDMaxLen = 128
	requestMetricModelMaxLen             = 128
)

// RequestMetric 是一次网关请求的结果快照（成功与失败都记录）。
//
// 该表独立于 usage_logs：usage_logs 只记录成功请求，无法诚实计算
// 分组成败率；request_metrics 为每个进入网关的已鉴权请求落一行。
type RequestMetric struct {
	RequestID string
	// UpstreamRequestID 是上游响应声明的请求标识，单独保存；
	// RequestID 统一由 resolveUsageBillingRequestID 生成，与 usage_logs 一致。
	UpstreamRequestID string
	UserID            int64
	APIKeyID          int64
	GroupID           int64
	// MerchantID 是分组归属商户的快照；nil 表示平台自营。
	MerchantID *int64
	Model      string
	StatusCode int
	// UpstreamError 由 ClassifyUpstreamFailure 判定：5xx、未写出响应（status=0，
	// 超时/响应前断开），以及 2xx 已提交后带内流失败为 true；
	// 4xx 属于用户错误，不计入商户侧失败。
	UpstreamError       bool
	DurationMS          int
	FirstTokenMS        int
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
}

// RequestMetricRepository 负责把请求指标批次写入存储。
type RequestMetricRepository interface {
	InsertRequestMetrics(ctx context.Context, metrics []RequestMetric) error
}

// MetricsOptions 控制指标缓冲与落库节流。
type MetricsOptions struct {
	// Buffer 是缓冲队列容量；<= 0 时使用默认值。
	Buffer int
	// BatchSize 是单批最大行数；<= 0 时使用默认值。
	BatchSize int
	// FlushInterval 是批次最长等待时间；<= 0 时使用默认值。
	FlushInterval time.Duration
}

// MetricsService 以有界队列异步落库请求指标。
//
// Record 只做非阻塞入队：队列满或服务已关闭时丢弃并累加 Dropped，
// 调用方（网关热路径）不会因为指标存储变慢或故障而阻塞。
type MetricsService struct {
	repo          RequestMetricRepository
	ch            chan RequestMetric
	batchSize     int
	flushInterval time.Duration
	insertTimeout time.Duration

	dropped  atomic.Int64
	failed   atomic.Int64
	inserted atomic.Int64
	started  atomic.Bool
	closed   atomic.Bool

	startOnce sync.Once
	stopCh    chan struct{}
	stopOnce  sync.Once
	wg        sync.WaitGroup

	lastDropLog atomic.Int64
}

// NewMetricsService 创建指标服务。调用 Start 后开始异步落库。
func NewMetricsService(repo RequestMetricRepository, opts MetricsOptions) *MetricsService {
	buffer := opts.Buffer
	if buffer <= 0 {
		buffer = defaultRequestMetricBuffer
	}
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = defaultRequestMetricBatchSize
	}
	if batchSize > buffer {
		batchSize = buffer
	}
	flushInterval := opts.FlushInterval
	if flushInterval <= 0 {
		flushInterval = defaultRequestMetricFlushInterval
	}
	return &MetricsService{
		repo:          repo,
		ch:            make(chan RequestMetric, buffer),
		batchSize:     batchSize,
		flushInterval: flushInterval,
		insertTimeout: defaultRequestMetricInsertTimeout,
		stopCh:        make(chan struct{}),
	}
}

// Start 启动后台落库 worker。重复调用无副作用。
func (s *MetricsService) Start() {
	if s == nil || s.repo == nil {
		return
	}
	s.startOnce.Do(func() {
		s.started.Store(true)
		s.wg.Add(1)
		go s.run()
	})
}

// Record 非阻塞地提交一条请求指标。
//
// ctx 只用于补齐 request_id（与 usage_logs 使用同一套 client/local 规则），
// 落库使用独立上下文，不受请求取消影响。
func (s *MetricsService) Record(ctx context.Context, m RequestMetric) {
	if s == nil {
		return
	}
	if s.closed.Load() {
		s.dropped.Add(1)
		return
	}
	if m.RequestID == "" {
		if ctx == nil {
			ctx = context.Background()
		}
		// 与 usage_logs 使用同一解析规则：ctx 的 client/local ID 优先，
		// 上游 ID 仅在无 ctx 标识或 forced durable ID 时胜出。
		m.RequestID = resolveUsageBillingRequestID(ctx, m.UpstreamRequestID)
	}
	m.RequestID = truncateMetricString(m.RequestID, requestMetricRequestIDMaxLen)
	m.UpstreamRequestID = truncateMetricString(m.UpstreamRequestID, requestMetricUpstreamRequestIDMaxLen)
	m.Model = truncateMetricString(m.Model, requestMetricModelMaxLen)
	select {
	case s.ch <- m:
	default:
		s.dropped.Add(1)
		s.logDrop()
	}
}

// Dropped 返回因队列满或服务关闭而丢弃的指标条数。
func (s *MetricsService) Dropped() int64 {
	if s == nil {
		return 0
	}
	return s.dropped.Load()
}

// Failed 返回落库失败的指标条数。
func (s *MetricsService) Failed() int64 {
	if s == nil {
		return 0
	}
	return s.failed.Load()
}

// Inserted 返回成功落库的指标条数。
func (s *MetricsService) Inserted() int64 {
	if s == nil {
		return 0
	}
	return s.inserted.Load()
}

// Close 停止接收新指标，尽量落完缓冲区后返回。
// 未启动的实例直接返回；ctx 超时只影响等待，不影响已入队数据的丢弃语义。
func (s *MetricsService) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.closed.Store(true)
	if !s.started.Load() {
		return nil
	}
	s.stopOnce.Do(func() { close(s.stopCh) })

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *MetricsService) run() {
	defer s.wg.Done()

	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()

	batch := make([]RequestMetric, 0, s.batchSize)
	for {
		select {
		case m := <-s.ch:
			batch = append(batch, m)
			if len(batch) >= s.batchSize {
				s.flush(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				s.flush(batch)
				batch = batch[:0]
			}
		case <-s.stopCh:
			for {
				select {
				case m := <-s.ch:
					batch = append(batch, m)
					if len(batch) >= s.batchSize {
						s.flush(batch)
						batch = batch[:0]
					}
				default:
					if len(batch) > 0 {
						s.flush(batch)
					}
					return
				}
			}
		}
	}
}

func (s *MetricsService) flush(batch []RequestMetric) {
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.insertTimeout)
	defer cancel()
	if err := s.repo.InsertRequestMetrics(ctx, batch); err != nil {
		s.failed.Add(int64(len(batch)))
		logger.L().Warn("metrics.request_metric_insert_failed",
			zap.Int("count", len(batch)),
			zap.Error(err),
		)
		return
	}
	s.inserted.Add(int64(len(batch)))
}

func (s *MetricsService) logDrop() {
	now := time.Now().UnixNano()
	last := s.lastDropLog.Load()
	if now-last < int64(30*time.Second) {
		return
	}
	if !s.lastDropLog.CompareAndSwap(last, now) {
		return
	}
	logger.L().Warn("metrics.request_metric_dropped",
		zap.Int64("dropped_total", s.dropped.Load()),
	)
}

// ClassifyUpstreamFailure 判定状态码是否应归因于上游。
//
// status 0 表示网关没有写出响应（超时或客户端在响应前断开），按上游失败处理；
// 5xx 是上游故障；4xx 是用户请求错误，不能计入商户指标。
func ClassifyUpstreamFailure(statusCode int) bool {
	return statusCode == 0 || statusCode >= 500
}

// truncateMetricString 按字节截断到列宽，同时避免切坏多字节字符。
func truncateMetricString(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}
