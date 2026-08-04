package spider

import (
	"github.com/go-kratos/kratos/v2/log"
)

var (
	NowCoder   = "NowCoder"
	AtCoder    = "AtCoder"
	LuoGu      = "LuoGu"
	CodeForces = "CodeForces"
	QOJ        = "QOJ"
	LeetCode   = "LeetCode"
	LOJ        = "LOJ"
	UOJ        = "UOJ"
	POJ        = "POJ"
)
var registry = map[string]Provider{}

// Register 注册 provider
//
// 参数:
//   - p Provider 提供器
func Register(p Provider) {
	if _, ok := registry[p.Name()]; ok {
		log.Error("爬虫Provider重复注册：", p.Name())
	}
	registry[p.Name()] = p
}

// Get 获取 Provider
//
// 参数:
//   - name string Provider 名称
//
// 返回值:
//   - Provider
//   - bool 是否找到相关Provider
func Get(name string) (Provider, bool) {
	if _, ok := registry[name]; ok {
		return registry[name], true
	}
	return nil, false
}
