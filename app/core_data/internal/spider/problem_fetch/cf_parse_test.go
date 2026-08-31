package problem_fetch

import (
	"strings"
	"testing"
)

func TestParseCodeforcesProblemHTMLPreservesSamplesAndImages(t *testing.T) {
	html := `<div class="problem-statement">
<div class="header"><div class="title">A. Tree</div></div>
<div class="problem-statement"><p>Statement.</p></div>
<div class="input-specification"><div class="section-title">Input</div><p>A tree.</p></div>
<div class="output-specification"><div class="section-title">Output</div><p>YES or NO.</p></div>
	<div class="sample-tests"><div class="sample-test">
  <div class="input"><div class="title">Input</div><pre>1
2</pre></div>
  <div class="output"><div class="title">Output</div><pre>YES</pre></div>
  <div class="input"><div class="title">Input</div><pre>5</pre></div>
  <div class="output"><div class="title">Output</div><pre>NO</pre></div>
</div></div>
<p><img src="/gym/106671/attachments/diagram.png" alt="diagram"></p>
</div>`

	got, err := parseCodeforcesProblemHTML(html, "https://codeforces.com/gym/106671/problem/A")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.ContentMD, "**输入**\n\n```\n1\n2\n```") {
		t.Fatalf("sample input was lost:\n%s", got.ContentMD)
	}
	if !strings.Contains(got.ContentMD, "**输出**\n\n```\nYES\n```") {
		t.Fatalf("sample output was lost:\n%s", got.ContentMD)
	}
	if !strings.Contains(got.ContentMD, "**输入**\n\n```\n5\n```") || !strings.Contains(got.ContentMD, "**输出**\n\n```\nNO\n```") {
		t.Fatalf("later samples were lost:\n%s", got.ContentMD)
	}
	if !strings.Contains(got.ContentMD, "![diagram](https://codeforces.com/gym/106671/attachments/diagram.png)") {
		t.Fatalf("image URL was not preserved:\n%s", got.ContentMD)
	}
}
