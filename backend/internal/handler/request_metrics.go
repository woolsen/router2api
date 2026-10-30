package handler

import (
	"context"
	"time"

	"github.com/woolsen/router2api/internal/service"

	"github.com/gin-gonic/gin"
)

// requestMetricRecorder 在 handler 内采集一次请求的结果。
//
// 用法：handler 入口创建（defer Finish 必须早于/伴随 recover defer 注册，
// 保证 panic 恢复写出的状态码也能被观察到），拿到 apiKey/subject 后 Configure，
// 解析出模型后 SetModel，转发成功（或拿到部分结果）后 Observe*，
// handler 返回时 Finish 非阻塞投递到 MetricsService。
type requestMetricRecorder struct {
	svc *service.MetricsService

	startedAt time.Time
	userID    int64
	apiKeyID  int64
	groupID   int64
	// merchantID 是分组商户快照；nil 表示平台自营。
	merchantID *int64
	model      string

	requestID           string
	durationMS          int
	firstTokenMS        int
	inputTokens         int
	outputTokens        int
	cacheReadTokens     int
	cacheCreationTokens int
}

func newRequestMetricRecorder() *requestMetricRecorder {
	return &requestMetricRecorder{startedAt: time.Now()}
}

// Configure 绑定指标服务与请求归属。apiKey 为空时保持无归属，Finish 直接跳过。
func (r *requestMetricRecorder) Configure(svc *service.MetricsService, apiKey *service.APIKey, userID int64) {
	if r == nil {
		return
	}
	r.svc = svc
	if apiKey == nil {
		return
	}
	r.userID = userID
	r.apiKeyID = apiKey.ID
	r.applyGroup(apiKey)
}

// SetGroup 在请求切到兜底分组后刷新归属，保证指标落在实际服务的分组上。
func (r *requestMetricRecorder) SetGroup(apiKey *service.APIKey) {
	if r == nil || apiKey == nil {
		return
	}
	r.apiKeyID = apiKey.ID
	r.applyGroup(apiKey)
}

func (r *requestMetricRecorder) applyGroup(apiKey *service.APIKey) {
	r.groupID = 0
	if apiKey.GroupID != nil {
		r.groupID = *apiKey.GroupID
	}
	r.merchantID = nil
	if apiKey.Group != nil && apiKey.Group.MerchantID != nil {
		merchantID := *apiKey.Group.MerchantID
		r.merchantID = &merchantID
	}
}

// SetModel 记录客户端请求的模型名。
func (r *requestMetricRecorder) SetModel(model string) {
	if r == nil {
		return
	}
	r.model = model
}

// ObserveForward 采集 Anthropic 协议转发结果中的 request_id、耗时、首包与 token。
func (r *requestMetricRecorder) ObserveForward(result *service.ForwardResult) {
	if r == nil || result == nil {
		return
	}
	if result.RequestID != "" {
		r.requestID = result.RequestID
	}
	if result.Duration > 0 {
		r.durationMS = int(result.Duration.Milliseconds())
	}
	if result.FirstTokenMs != nil {
		r.firstTokenMS = *result.FirstTokenMs
	}
	r.inputTokens = result.Usage.InputTokens
	r.outputTokens = result.Usage.OutputTokens
	r.cacheReadTokens = result.Usage.CacheReadInputTokens
	r.cacheCreationTokens = result.Usage.CacheCreationInputTokens
}

// ObserveOpenAIForward 采集 OpenAI 协议转发结果中的 request_id、耗时、首包与 token。
func (r *requestMetricRecorder) ObserveOpenAIForward(result *service.OpenAIForwardResult) {
	if r == nil || result == nil {
		return
	}
	if result.RequestID != "" {
		r.requestID = result.RequestID
	}
	if result.Duration > 0 {
		r.durationMS = int(result.Duration.Milliseconds())
	}
	if result.FirstTokenMs != nil {
		r.firstTokenMS = *result.FirstTokenMs
	}
	r.inputTokens = result.Usage.InputTokens
	r.outputTokens = result.Usage.OutputTokens
	r.cacheReadTokens = result.Usage.CacheReadInputTokens
	r.cacheCreationTokens = result.Usage.CacheCreationInputTokens
}

// Finish 在响应写完后投递指标。非阻塞：MetricsService.Record 只入队。
func (r *requestMetricRecorder) Finish(c *gin.Context) {
	if r == nil || r.svc == nil || c == nil {
		return
	}
	// 状态码以实际写出为准：未写出（超时/响应前断开）记为 0，由分类器计入上游失败。
	statusCode := 0
	if c.Writer != nil && c.Writer.Written() {
		statusCode = c.Writer.Status()
	}
	durationMS := r.durationMS
	if durationMS <= 0 {
		durationMS = int(time.Since(r.startedAt).Milliseconds())
		if durationMS < 0 {
			durationMS = 0
		}
	}
	ctx := context.Background()
	if c.Request != nil {
		ctx = c.Request.Context()
	}
	r.svc.Record(ctx, service.RequestMetric{
		RequestID:           r.requestID,
		UserID:              r.userID,
		APIKeyID:            r.apiKeyID,
		GroupID:             r.groupID,
		MerchantID:          r.merchantID,
		Model:               r.model,
		StatusCode:          statusCode,
		UpstreamError:       service.ClassifyUpstreamFailure(statusCode),
		DurationMS:          durationMS,
		FirstTokenMS:        r.firstTokenMS,
		InputTokens:         r.inputTokens,
		OutputTokens:        r.outputTokens,
		CacheReadTokens:     r.cacheReadTokens,
		CacheCreationTokens: r.cacheCreationTokens,
	})
}
