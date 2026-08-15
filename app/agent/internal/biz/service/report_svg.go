package service

import (
	"fmt"
	"html"
	"math"
	"strings"
)

// 内联 SVG 图表：viewBox 定坐标、width:100% 自适应宽度，
// 移动端/PC 邮件客户端（Gmail/QQ 邮箱/新版 Outlook）按容器等比缩放。
const (
	svgWidth  = 560.0
	svgHeight = 200.0
	svgPadTop = 18.0
	svgPadBot = 26.0
	svgPadLft = 6.0
	svgPadRgt = 6.0
)

// svgShell 打开 SVG 外壳（style 自适应 + 圆角卡片底）
func svgShell(b *strings.Builder) {
	b.WriteString(`<svg viewBox="0 0 560 200" xmlns="http://www.w3.org/2000/svg" style="width:100%;height:auto;display:block;background:#ffffff;border:1px solid #e5e5e5;border-radius:10px;box-sizing:border-box;"><rect x="0.5" y="0.5" width="559" height="199" fill="none" stroke="#e5e5e5" stroke-width="1" rx="10"/>`)
}

func svgClose(b *strings.Builder) {
	b.WriteString(`</svg>`)
}

// BarChartSVG 柱状图：labels 与 values 等长（缺 0 补）。柱顶标数值，超 6 根时省略柱顶标签。
func BarChartSVG(labels []string, values []int64, color string) string {
	n := len(labels)
	if n == 0 {
		return ""
	}
	if color == "" {
		color = "#171717"
	}
	maxV := int64(1)
	for _, v := range values {
		if v > maxV {
			maxV = v
		}
	}
	plotW := svgWidth - svgPadLft - svgPadRgt
	plotH := svgHeight - svgPadTop - svgPadBot
	slot := plotW / float64(n)
	barW := slot * 0.56
	if barW > 44 {
		barW = 44
	}
	showLabels := n <= 8

	var b strings.Builder
	svgShell(&b)
	for i := 0; i < n; i++ {
		v := values[i]
		if i < 0 || i >= len(values) {
			v = 0
		}
		if v < 0 {
			v = 0
		}
		x := svgPadLft + slot*float64(i) + (slot-barW)/2
		h := 0.0
		if v > 0 {
			h = math.Max(2, float64(v)/float64(maxV)*plotH)
		}
		y := svgPadTop + plotH - h
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="3" fill="%s"/>`, x, y, barW, h, color)
		if showLabels && v > 0 {
			fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" font-size="11" fill="#0a0a0a" font-weight="600">%d</text>`, x+barW/2, y-5, v)
		}
		// 日期标签：偶数位或总数少时全显示，避免挤压
		if n <= 8 || i%2 == 0 {
			fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" font-size="10" fill="#737373">%s</text>`, x+barW/2, svgHeight-svgPadBot+16, html.EscapeString(labels[i]))
		}
	}
	svgClose(&b)
	return b.String()
}

// LineChartSVG 折线图：series 每行为一条线（与 names 对应），标签取各线同下标点。
func LineChartSVG(labels []string, series [][]int64, names []string, colors []string) string {
	n := len(labels)
	if n == 0 || len(series) == 0 {
		return ""
	}
	maxV := int64(1)
	for _, s := range series {
		for _, v := range s {
			if v > maxV {
				maxV = v
			}
		}
	}
	const (
		linePadTop   = 30.0
		linePadLeft  = 38.0
		linePadRight = 28.0
	)
	plotW := svgWidth - linePadLeft - linePadRight
	plotH := svgHeight - linePadTop - svgPadBot
	step := 0.0
	if n > 1 {
		step = plotW / float64(n-1)
	}
	tickStep := (maxV + 3) / 4
	chartMax := tickStep * 4
	point := func(i int, v int64) (float64, float64) {
		x := linePadLeft + step*float64(i)
		if n == 1 {
			x = linePadLeft + plotW/2
		}
		y := linePadTop + plotH - float64(v)/float64(chartMax)*plotH
		return x, y
	}

	var b strings.Builder
	svgShell(&b)
	// Y 轴刻度与横向网格线。
	for g := 0; g <= 4; g++ {
		gy := linePadTop + plotH*float64(g)/4
		value := chartMax - int64(g)*tickStep
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#f0f0f0" stroke-width="1"/>`, linePadLeft, gy, svgWidth-linePadRight, gy)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="end" font-size="10" fill="#737373">%d</text>`, linePadLeft-7, gy+3.5, value)
	}
	fmt.Fprintf(&b, `<line data-axis="y" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#a3a3a3" stroke-width="1"/>`, linePadLeft, linePadTop, linePadLeft, linePadTop+plotH)
	fmt.Fprintf(&b, `<line data-axis="x" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#a3a3a3" stroke-width="1"/>`, linePadLeft, linePadTop+plotH, svgWidth-linePadRight, linePadTop+plotH)
	for si, s := range series {
		col := "#171717"
		if si < len(colors) && colors[si] != "" {
			col = colors[si]
		}
		// 折线
		pts := make([]string, 0, n)
		for i, v := range s {
			if i >= n {
				break
			}
			if v < 0 {
				v = 0
			}
			x, y := point(i, v)
			pts = append(pts, fmt.Sprintf("%.1f,%.1f", x, y))
		}
		name := ""
		if si < len(names) {
			name = names[si]
		}
		fmt.Fprintf(&b, `<polyline data-series="%s" points="%s" fill="none" stroke="%s" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/>`, html.EscapeString(name), strings.Join(pts, " "), col)
		// 数据点 + 值
		for i, v := range s {
			if i >= n {
				break
			}
			x, y := point(i, v)
			edge := ""
			if i == 0 {
				edge = ` data-point-edge="first"`
			} else if i == n-1 {
				edge = ` data-point-edge="last"`
			}
			fmt.Fprintf(&b, `<circle%s cx="%.1f" cy="%.1f" r="3" fill="%s"/>`, edge, x, y, col)
			if v > 0 {
				position := ""
				if v == chartMax {
					position = ` data-value-label="top"`
				}
				fmt.Fprintf(&b, `<text%s x="%.1f" y="%.1f" text-anchor="middle" font-size="10" fill="#0a0a0a">%d</text>`, position, x, y-8, v)
			}
		}
	}
	// X 轴日期标签
	for i := range labels {
		if n > 8 && i%2 != 0 {
			continue
		}
		x, _ := point(i, 0)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" font-size="10" fill="#737373">%s</text>`, x, svgHeight-svgPadBot+16, html.EscapeString(labels[i]))
	}
	// 图例
	if len(names) > 1 {
		legendY := 12.0
		x := svgPadLft + 4
		for i, nm := range names {
			col := "#171717"
			if i < len(colors) && colors[i] != "" {
				col = colors[i]
			}
			fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="8" height="8" rx="2" fill="%s"/>`, x, legendY-8, col)
			fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="10" fill="#737373">%s</text>`, x+12, legendY, html.EscapeString(nm))
			x += 12 + float64(utf8RuneCount(nm))*11 + 10
		}
	}
	svgClose(&b)
	return b.String()
}

func utf8RuneCount(s string) int {
	return len([]rune(s))
}
