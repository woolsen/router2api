package service

import (
	"context"
	"time"
)

const (
	// MerchantUsageTrendDefaultDays 是商户后台用量曲线的默认窗口（含今天）。
	MerchantUsageTrendDefaultDays = 7
	// MerchantUsageTrendMaxDays 限制商户可拉取的最大窗口，避免无界扫描 usage_logs。
	MerchantUsageTrendMaxDays = 30
)

// MerchantDashboard 是商户后台首屏所需的聚合信息。
type MerchantDashboard struct {
	MerchantID     int64
	Name           string
	CommissionRate float64
	// PayableBalance 是商户资金账本的可用余额（merchant_balance.balance）。
	PayableBalance float64
}

// MerchantDailyUsagePoint 是商户某一天的计费用量。
// 金额口径与结算一致：只统计钱包计费（billing_type=balance）且 actual_cost>0 的行；
// commission_rate 为 NULL 的行费率未知，不计入应付，避免把未知费率当成 0 抽成。
type MerchantDailyUsagePoint struct {
	Date       string
	Requests   int64
	GrossCost  float64
	Commission float64
	NetPayable float64
}

// MerchantUsageTrend 是补齐空白日期后的连续日报。
type MerchantUsageTrend struct {
	Days      int
	StartDate string
	EndDate   string
	Points    []MerchantDailyUsagePoint
}

// MerchantConsoleRepository 是商户后台的只读查询接口。
// 所有方法都以服务端注入的 merchantID 为唯一过滤条件。
type MerchantConsoleRepository interface {
	// PayableBalance 返回该商户 merchant_balance.balance（与提现可用额同口径）。
	PayableBalance(ctx context.Context, merchantID int64) (float64, error)
	// DailyUsage 汇总 [from, to) 内该商户的每日计费用量，按日期升序。
	DailyUsage(ctx context.Context, merchantID int64, from, to time.Time) ([]MerchantDailyUsagePoint, error)
}

// MerchantConsoleReader 是商户后台处理器依赖的只读用例集合。
type MerchantConsoleReader interface {
	Dashboard(ctx context.Context, merchantID int64) (*MerchantDashboard, error)
	DailyUsage(ctx context.Context, merchantID int64, days int) (*MerchantUsageTrend, error)
}

// MerchantConsoleService 只依赖已认证商户 ID，不接受任何前端传入的 merchant_id。
type MerchantConsoleService struct {
	merchants *MerchantService
	repo      MerchantConsoleRepository
	now       func() time.Time
}

func NewMerchantConsoleService(merchants *MerchantService, repo MerchantConsoleRepository) *MerchantConsoleService {
	return &MerchantConsoleService{
		merchants: merchants,
		repo:      repo,
		now:       time.Now,
	}
}

// Dashboard 返回商户名、抽成率与可用余额。
func (s *MerchantConsoleService) Dashboard(ctx context.Context, merchantID int64) (*MerchantDashboard, error) {
	if merchantID <= 0 {
		return nil, ErrMerchantNotFound
	}
	merchant, err := s.merchants.Get(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	payable, err := s.repo.PayableBalance(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	return &MerchantDashboard{
		MerchantID:     merchant.ID,
		Name:           merchant.Name,
		CommissionRate: merchant.CommissionRate,
		PayableBalance: payable,
	}, nil
}

// DailyUsage 返回最近 days 个自然日（UTC，含今天）的用量；没有用量的日期补零。
func (s *MerchantConsoleService) DailyUsage(ctx context.Context, merchantID int64, days int) (*MerchantUsageTrend, error) {
	if merchantID <= 0 {
		return nil, ErrMerchantNotFound
	}
	if days <= 0 {
		days = MerchantUsageTrendDefaultDays
	}
	if days > MerchantUsageTrendMaxDays {
		days = MerchantUsageTrendMaxDays
	}

	today := s.now().UTC().Truncate(24 * time.Hour)
	start := today.AddDate(0, 0, -(days - 1))
	end := today.AddDate(0, 0, 1)

	rows, err := s.repo.DailyUsage(ctx, merchantID, start, end)
	if err != nil {
		return nil, err
	}

	byDate := make(map[string]MerchantDailyUsagePoint, len(rows))
	for _, row := range rows {
		byDate[row.Date] = row
	}

	points := make([]MerchantDailyUsagePoint, 0, days)
	for day := start; day.Before(end); day = day.AddDate(0, 0, 1) {
		date := day.Format("2006-01-02")
		point, ok := byDate[date]
		if !ok {
			point = MerchantDailyUsagePoint{Date: date}
		}
		points = append(points, point)
	}

	return &MerchantUsageTrend{
		Days:      days,
		StartDate: start.Format("2006-01-02"),
		EndDate:   today.Format("2006-01-02"),
		Points:    points,
	}, nil
}
