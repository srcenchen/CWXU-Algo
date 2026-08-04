package problem_fetch

import (
	"strings"
	"testing"
)

// 精简自 poj.org/problem?id=1000 的真实结构（去掉顶栏导航）。
const samplePOJ1000HTML = `<html><head>
<meta http-equiv="Content-Type" content="text/html; charset=utf-8">
<title>1000 -- A+B Problem</title>
</head><body>
<div class="ptt" lang="en-US">A+B Problem</div>
<div class="plm"><table align="center"><tr>
<td><b>Time Limit:</b> 1000MS</td><td width="10px"></td>
<td><b>Memory Limit:</b> 10000K</td></tr>
<tr><td><b>Total Submissions:</b> 587082</td><td width="10px"></td>
<td><b>Accepted:</b> 330506</td></tr></table></div>
<p class="pst">Description</p>
<div class="ptx" lang="en-US">Calculate a+b </div>
<p class="pst">Input</p>
<div class="ptx" lang="en-US">Two integer a,b (0&lt;=a,b&lt;=10)</div>
<p class="pst">Output</p>
<div class="ptx" lang="en-US">Output a+b</div>
<p class="pst">Sample Input</p>
<pre class="sio">1 2</pre>
<p class="pst">Sample Output</p>
<pre class="sio">3</pre>
<p class="pst">Hint</p>
<div class="ptx" lang="en-US">Q: Where are the input and the output?
<br><br>A: Your program shall always <font color=red>read input from stdin</font>.
<br><pre>#include &lt;iostream>
int main(){ return 0; }</pre></div>
<p class="pst">Source</p>
<div class="ptx" lang="en-US"><a href="searchproblem?field=source&key=OOI">OOI</a></div>
</body></html>`

// 中文题 + 图片（结构仿 3984 / 1654）
const samplePOJChineseHTML = `<html><head><title>3984 -- 迷宫问题</title></head><body>
<div class="ptt" lang="en-US">迷宫问题</div>
<div class="plm"><table><tr>
<td><b>Time Limit:</b> 1000MS</td>
<td><b>Memory Limit:</b> 65536K</td>
</tr></table></div>
<p class="pst">Description</p>
<div class="ptx" lang="en-US">定义一个二维数组：
<br><pre>
int maze[5][5] = {
0, 1, 0, 0, 0,
};
</pre>
<br>它表示一个迷宫，其中的1表示墙壁。
<br><center><img src=images/1654_1.jpg></center></div>
<p class="pst">Input</p>
<div class="ptx" lang="en-US">一个5 &times; 5的二维数组。</div>
<p class="pst">Output</p>
<div class="ptx" lang="en-US">最短路径。</div>
<p class="pst">Sample Input</p>
<pre class="sio">0 1 0 0 0
0 0 0 1 0</pre>
<p class="pst">Sample Output</p>
<pre class="sio">(0, 0)
(1, 0)</pre>
<p class="pst">Source</p>
<div class="ptx" lang="en-US"><a href="searchproblem?field=source&key="></a></div>
</body></html>`

const samplePOJErrorHTML = `<html><head><title>Error</title></head>
<body>Can not find problem 999999.</body></html>`

func TestNormalizePOJTitle(t *testing.T) {
	cases := []struct {
		raw, id, want string
	}{
		{"A+B Problem", "1000", "#1000. A+B Problem"},
		{"1000 -- A+B Problem", "1000", "#1000. A+B Problem"},
		{"#1000. A+B Problem", "1000", "#1000. A+B Problem"},
		{"迷宫问题", "3984", "#3984. 迷宫问题"},
		{"Error", "1000", "#1000"},
		{"", "1000", "#1000"},
		{"Area", "1654", "#1654. Area"},
	}
	for _, c := range cases {
		got := normalizePOJTitle(c.raw, c.id)
		if got != c.want {
			t.Fatalf("normalizePOJTitle(%q,%q)=%q want %q", c.raw, c.id, got, c.want)
		}
	}
}

func TestParsePOJProblemHTML_1000(t *testing.T) {
	fc, err := parsePOJProblemHTML(samplePOJ1000HTML, "1000")
	if err != nil {
		t.Fatal(err)
	}
	if fc.Title != "#1000. A+B Problem" {
		t.Fatalf("title=%q", fc.Title)
	}
	md := fc.ContentMD
	if !strings.HasPrefix(md, "# 1000. A+B Problem\n") {
		head := md
		if len(head) > 120 {
			head = head[:120]
		}
		t.Fatalf("md h1 wrong:\n%s", head)
	}
	if strings.Contains(md, "# #1000") {
		t.Fatal("double hash in markdown h1")
	}
	mustContain := []string{
		"**Time Limit:** 1000MS",
		"**Memory Limit:** 10000K",
		"## 题目描述",
		"Calculate a+b",
		"## 输入",
		"0<=a,b<=10",
		"## 输出",
		"Output a+b",
		"## 样例输入",
		"```\n1 2\n```",
		"## 样例输出",
		"```\n3\n```",
		"## 提示",
		"read input from stdin",
		"## 来源",
		"[OOI](http://poj.org/searchproblem?field=source&key=OOI)",
	}
	for _, s := range mustContain {
		if !strings.Contains(md, s) {
			t.Fatalf("missing %q in:\n%s", s, md)
		}
	}
	// 统计数不应进 limits（只 Time/Memory）
	if strings.Contains(md, "Total Submissions") {
		t.Fatal("should skip submission stats")
	}
}

func TestParsePOJProblemHTML_ChineseAndImage(t *testing.T) {
	fc, err := parsePOJProblemHTML(samplePOJChineseHTML, "3984")
	if err != nil {
		t.Fatal(err)
	}
	if fc.Title != "#3984. 迷宫问题" {
		t.Fatalf("title=%q", fc.Title)
	}
	md := fc.ContentMD
	if !strings.Contains(md, "迷宫") {
		t.Fatalf("missing chinese body:\n%s", md)
	}
	if !strings.Contains(md, "![image](http://poj.org/images/1654_1.jpg)") {
		t.Fatalf("image not absolutized:\n%s", md)
	}
	if !strings.Contains(md, "5 × 5") && !strings.Contains(md, "5 × 5") {
		// &times; → × via goquery
		if !strings.Contains(md, "×") {
			t.Fatalf("entity not decoded: %s", md)
		}
	}
	// 空 Source 应跳过
	if strings.Contains(md, "## 来源") {
		t.Fatalf("empty source should be skipped:\n%s", md)
	}
	if !strings.Contains(md, "```\n0 1 0 0 0\n0 0 0 1 0\n```") {
		t.Fatalf("sample input block:\n%s", md)
	}
}

func TestParsePOJProblemHTML_Error(t *testing.T) {
	_, err := parsePOJProblemHTML(samplePOJErrorHTML, "999999")
	if err == nil {
		t.Fatal("expected error for missing problem")
	}
}

func TestAbsolutizePOJURL(t *testing.T) {
	if got := absolutizePOJURL("images/1654_1.jpg"); got != "http://poj.org/images/1654_1.jpg" {
		t.Fatalf("got %s", got)
	}
	if got := absolutizePOJURL("http://poj.org/x"); got != "http://poj.org/x" {
		t.Fatalf("got %s", got)
	}
	if got := absolutizePOJURL("/images/a.jpg"); got != "http://poj.org/images/a.jpg" {
		t.Fatalf("got %s", got)
	}
}

func TestFetchPOJ_Live1000(t *testing.T) {
	if testing.Short() {
		t.Skip("skip live poj")
	}
	fc, err := Fetch("POJ", "1000", "")
	if err != nil {
		t.Fatal(err)
	}
	if fc.Title != "#1000. A+B Problem" {
		t.Fatalf("title=%q", fc.Title)
	}
	if !strings.Contains(fc.ContentMD, "Calculate a+b") {
		t.Fatalf("body:\n%s", fc.ContentMD)
	}
	if !strings.Contains(fc.ContentMD, "## 样例输入") {
		t.Fatalf("sections:\n%s", fc.ContentMD)
	}
	t.Logf("live 1000 title=%s len(md)=%d", fc.Title, len(fc.ContentMD))
}

func TestFetchPOJ_LiveChinese3984(t *testing.T) {
	if testing.Short() {
		t.Skip("skip live poj")
	}
	fc, err := Fetch("POJ", "3984", "http://poj.org/problem?id=3984")
	if err != nil {
		t.Fatal(err)
	}
	if fc.Title != "#3984. 迷宫问题" {
		t.Fatalf("title=%q want #3984. 迷宫问题", fc.Title)
	}
	if !strings.Contains(fc.ContentMD, "迷宫") {
		t.Fatalf("missing 迷宫 in body")
	}
	t.Logf("live 3984 ok len=%d", len(fc.ContentMD))
}

func TestFetchPOJ_LiveImage1654(t *testing.T) {
	if testing.Short() {
		t.Skip("skip live poj")
	}
	fc, err := Fetch("POJ", "1654", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fc.Title, "Area") {
		t.Fatalf("title=%q", fc.Title)
	}
	if !strings.Contains(fc.ContentMD, "http://poj.org/images/1654_1.jpg") {
		t.Fatalf("expected absolute image url:\n%s", fc.ContentMD)
	}
	t.Logf("live 1654 title=%s", fc.Title)
}

