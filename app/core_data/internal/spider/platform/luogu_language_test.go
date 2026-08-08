package platform

import "testing"

// TestLuoGuLanguageMap 锁定洛谷 language ID → 语言名映射契约。
// 数据源：洛谷 GET /_lfe/config codeLanguages（2026-08-08 实时抓取）。
func TestLuoGuLanguageMap(t *testing.T) {
	// 常见语言抽查：C++ 系列不再落 Others
	cases := []struct {
		id   int
		want string
	}{
		{2, "C"},
		{3, "C++98"},
		{11, "C++14"},
		{12, "C++17"},
		{27, "C++20"},
		{34, "C++23"},
		{7, "Python 3"},
		{25, "PyPy 3"},
		{8, "Java 8"},
		{14, "Go"},
		{15, "Rust"},
	}
	for _, c := range cases {
		got, ok := luoguLanguageName[c.id]
		if !ok {
			t.Errorf("language id %d 未映射", c.id)
			continue
		}
		if got != c.want {
			t.Errorf("language id %d = %q, want %q", c.id, got, c.want)
		}
	}

	// 未知 ID 必须走兜底（模拟调用方逻辑）
	if _, ok := luoguLanguageName[999]; ok {
		t.Error("未知 language id 999 不应有映射")
	}
}
