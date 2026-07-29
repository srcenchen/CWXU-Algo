// Package sqllike 提供 ILIKE / LIKE 模糊搜索的 keyword 处理。
package sqllike

import "strings"

// Pattern 将用户输入整理为 ILIKE 模式。
// 去掉 % / _ / \，避免通配符被用户控制；空串返回 ""（调用方应跳过 Where）。
func Pattern(keyword string) string {
	k := strings.TrimSpace(keyword)
	if k == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(k) + 2)
	for i := 0; i < len(k); i++ {
		c := k[i]
		if c == '%' || c == '_' || c == '\\' {
			continue
		}
		b.WriteByte(c)
	}
	if b.Len() == 0 {
		return ""
	}
	return "%" + b.String() + "%"
}
