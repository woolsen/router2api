package handler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMarketGroupsStatsContractMatchesFrontendTypes 钉住前后端字段契约。
//
// 货架指标曾经因为前端读扁平 stats.requests / success_rate / avg_latency_ms、
// 后端却返回嵌套 last_hour / last_24h 而永久显示「数据不足」。这里直接读取前端
// TypeScript 接口的字段名，与后端 DTO 实际 JSON key 集合逐一比对：任何一边新增、
// 删除或改名都会让测试失败，而不是等到页面上线后才发现空白指标。
func TestMarketGroupsStatsContractMatchesFrontendTypes(t *testing.T) {
	sourceBytes, err := os.ReadFile(filepath.Join("..", "..", "..", "frontend", "src", "api", "market.ts"))
	require.NoError(t, err, "frontend market API contract file must be present")
	source := string(sourceBytes)

	cases := []struct {
		name   string
		goDTO  any
		tsType string
	}{
		{name: "market group", goDTO: marketGroup{}, tsType: "MarketGroup"},
		{name: "market group stats", goDTO: marketGroupStats{}, tsType: "MarketGroupStats"},
		{name: "market stats window", goDTO: marketStatsWindow{}, tsType: "MarketStatsWindow"},
		{name: "market peak rate", goDTO: marketPeakRate{}, tsType: "MarketPeakRate"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backendKeys := jsonObjectKeys(t, tc.goDTO)
			frontendKeys := tsInterfaceFieldNames(t, source, tc.tsType)
			require.Equalf(t, frontendKeys, backendKeys,
				"frontend interface %s is out of sync with backend DTO JSON keys", tc.tsType)
		})
	}
}

func jsonObjectKeys(t *testing.T, value any) []string {
	t.Helper()
	payload, err := json.Marshal(value)
	require.NoError(t, err)

	var object map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(payload, &object), "payload: %s", payload)

	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// tsInterfaceFieldNames 提取 `export interface Name { ... }` 顶层的字段名。
// 只支持货架契约实际使用的简单字段声明；遇到嵌套对象字面量应改用命名 interface。
func tsInterfaceFieldNames(t *testing.T, source, name string) []string {
	t.Helper()
	bodyPattern := regexp.MustCompile(`(?s)export interface\s+` + regexp.QuoteMeta(name) + `\s*\{(.*?)\n\}`)
	match := bodyPattern.FindStringSubmatch(source)
	require.Len(t, match, 2, "interface %s not found in frontend/src/api/market.ts", name)

	fieldPattern := regexp.MustCompile(`^\s*([A-Za-z_$][A-Za-z0-9_$]*)\??\s*:`)
	fields := make([]string, 0)
	for _, line := range strings.Split(match[1], "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" ||
			strings.HasPrefix(trimmed, "//") ||
			strings.HasPrefix(trimmed, "/*") ||
			strings.HasPrefix(trimmed, "*") {
			continue
		}
		field := fieldPattern.FindStringSubmatch(line)
		if field == nil {
			continue
		}
		fields = append(fields, field[1])
	}
	sort.Strings(fields)
	return fields
}
