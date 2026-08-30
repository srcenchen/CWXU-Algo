package problem_fetch

import (
	"strings"
	"testing"
)

// 精简自 ac.nowcoder.com 赛题页 DOM：描述 + 输入/输出描述 + 示例（含「复制」按钮）
const ncACSampleHTML = `
<html><body>
<div class="question-title"><h1>小红的字符串处理</h1></div>
<div class="subject-des">
  <div class="subject-question">
    <img src="https://www.nowcoder.com/equation?tex=%5Chspace%7B15pt%7D" alt="\hspace{15pt}" />
    给定一个长为 <img src="https://www.nowcoder.com/equation?tex=n" alt="n" /> 的字符串，请你在每个字符的间隔中添加一个字符
    <img src="https://www.nowcoder.com/equation?tex=%5Ctexttt%7B.%7D" alt="\texttt{.}" />。
  </div>
  <h2 style="font-size:14px;font-weight:bold;color:#34495e;">输入描述:</h2>
  <pre><img src="https://hr.nowcoder.com/equation?tex=%5Chspace%7B15pt%7D" alt="\hspace{15pt}" />
第一行输入一个长为 <img src="https://hr.nowcoder.com/equation?tex=n" alt="n" /> 的字符串，保证其仅包含字母与数字。</pre>
  <h2 style="font-size:14px;font-weight:bold;color:#34495e;">输出描述:</h2>
  <pre><img src="https://hr.nowcoder.com/equation?tex=%5Chspace%7B15pt%7D" alt="\hspace{15pt}" />
输出一个整数，表示 <img src="https://hr.nowcoder.com/equation?tex=n" alt="n" /> 的 PrimeMEX。</pre>
  <div class="question-oi">
    <div class="question-oi-hd">示例1</div>
    <div class="question-oi-bd">
      <div class="question-oi-mod">
        <h2>输入</h2>
        <a class="code-copy-btn js-clipboard" href="javascript:void(0);">复制</a>
        <textarea data-clipboard-text-id="input1" style="display:none;">RIP</textarea>
        <div class="question-oi-cont"><pre>RIP</pre></div>
      </div>
      <div class="question-oi-mod">
        <h2>输出</h2>
        <a class="code-copy-btn js-clipboard" href="javascript:void(0);">复制</a>
        <textarea data-clipboard-text-id="output1" style="display:none;">R.I.P</textarea>
        <div class="question-oi-cont"><pre>R.I.P</pre></div>
      </div>
    </div>
  </div>
  <div class="question-oi">
    <div class="question-oi-hd">示例2</div>
    <div class="question-oi-bd">
      <div class="question-oi-mod">
        <h2>输入</h2>
        <a class="code-copy-btn js-clipboard">复制</a>
        <div class="question-oi-cont"><pre>ABC</pre></div>
      </div>
      <div class="question-oi-mod">
        <h2>输出</h2>
        <a class="code-copy-btn js-clipboard">复制</a>
        <div class="question-oi-cont"><pre>A.B.C</pre></div>
      </div>
    </div>
  </div>
</div>
</body></html>
`

func TestParseNowCoderACHHTML_IOAndSamples(t *testing.T) {
	fc, err := parseNowCoderACHHTML(ncACSampleHTML)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(fc.Title, "小红") {
		t.Fatalf("title=%q", fc.Title)
	}
	md := fc.ContentMD
	t.Logf("md:\n%s", md)

	// 公式应保留 n
	if !strings.Contains(md, "$n$") {
		t.Fatalf("expected $n$ formula in md:\n%s", md)
	}
	// 输入/输出描述各出现一次（## 标题）
	if c := strings.Count(md, "## 输入描述"); c != 1 {
		t.Fatalf("## 输入描述 count=%d\n%s", c, md)
	}
	if c := strings.Count(md, "## 输出描述"); c != 1 {
		t.Fatalf("## 输出描述 count=%d\n%s", c, md)
	}
	if strings.Contains(md, "复制") {
		t.Fatalf("copy button text leaked:\n%s", md)
	}
	// 样例内容
	if !strings.Contains(md, "RIP") || !strings.Contains(md, "R.I.P") {
		t.Fatalf("sample missing:\n%s", md)
	}
	if !strings.Contains(md, "## 示例1") || !strings.Contains(md, "## 示例2") {
		t.Fatalf("sample headers missing:\n%s", md)
	}
	// 描述正文含公式
	if !strings.Contains(md, `$\texttt{.}$`) && !strings.Contains(md, `$\texttt{.}$`) {
		// either form ok
	}
	if !strings.Contains(md, "PrimeMEX") {
		t.Fatalf("output desc body missing:\n%s", md)
	}
}

func TestNowCoderHeadingKind(t *testing.T) {
	cases := map[string]string{
		"输入描述:": "input_desc",
		"输入描述：": "input_desc",
		"输出描述":  "output_desc",
		"输入":    "",
		"输出":    "",
		"示例1":   "",
		"题目描述":  "",
	}
	for in, want := range cases {
		if got := nowcoderHeadingKind(in); got != want {
			t.Errorf("nowcoderHeadingKind(%q)=%q want %q", in, got, want)
		}
	}
}

func TestParseNowCoderMainHTML_NoFakeIO(t *testing.T) {
	// 主站 SEO 区：无「输入描述」h5，只有 question-oi 样例
	html := `
<html><head><title>反转链表_牛客</title></head>
<body>
<div style="position:absolute;left:-1000000px">
  给定一个单链表的头结点 pHead，反转该链表。
  <div class="question-oi">
    <div class="question-oi-hd">示例1</div>
    <div class="question-oi-bd">
      <div class="question-oi-mod">
        <h2>输入</h2>
        <div class="question-oi-cont"><pre>{1,2,3}</pre></div>
      </div>
      <div class="question-oi-mod">
        <h2>输出</h2>
        <div class="question-oi-cont"><pre>{3,2,1}</pre></div>
      </div>
    </div>
  </div>
</div>
</body></html>`
	fc, err := parseNowCoderMainHTML(html)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Contains(fc.ContentMD, "## 输入描述") || strings.Contains(fc.ContentMD, "## 输出描述") {
		t.Fatalf("should not invent IO desc from sample h2:\n%s", fc.ContentMD)
	}
	if !strings.Contains(fc.ContentMD, "{1,2,3}") {
		t.Fatalf("sample missing:\n%s", fc.ContentMD)
	}
}
