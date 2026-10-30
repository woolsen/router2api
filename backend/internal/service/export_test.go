package service

import (
	"net/http"
	"time"
)

// 本文件只编译进测试二进制，供外部测试包（service_test）注入时钟与
// 探测校验。生产构建不会看到这些函数。

// SetMetricsAggregatorClockForTest 替换聚合器的“当前时间”。
func SetMetricsAggregatorClockForTest(a *MetricsAggregator, now func() time.Time) {
	if a != nil {
		a.now = now
	}
}

// SetSettlementServiceClockForTest 替换结算生成服务的“当前时间”。
func SetSettlementServiceClockForTest(s *SettlementService, now func() time.Time) {
	if s != nil {
		s.now = now
	}
}

// SetSettlementOpsClockForTest 替换结算状态机的“当前时间”。
func SetSettlementOpsClockForTest(o *SettlementOps, now func() time.Time) {
	if o != nil {
		o.now = now
	}
}

// SetWithdrawalServiceClockForTest 替换提现服务的“当前时间”。
func SetWithdrawalServiceClockForTest(s *WithdrawalService, now func() time.Time) {
	if s != nil {
		s.now = now
	}
}

// SetDisputeServiceClockForTest 替换申诉服务的“当前时间”。
func SetDisputeServiceClockForTest(s *DisputeService, now func() time.Time) {
	if s != nil {
		s.now = now
	}
}

// NewProbeServiceForTest 构造一个仍强制 https、但把“解析后 IP 是否安全”的
// 判断交给测试的探测器。端到端测试用真实 TLS 桩，Transport 把公网域名
// 拨到本地，因此不需要再做 DNS 解析校验。
func NewProbeServiceForTest(client *http.Client) *ProbeService {
	return newProbeService(client, func(string) error { return nil })
}
