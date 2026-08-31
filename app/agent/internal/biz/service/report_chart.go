package service

import (
	"fmt"
	"html"
	"strings"
)

// EmailBarChart renders a chart with email-safe tables and inline CSS only.
// It deliberately avoids SVG, canvas, external CSS, and embedded images.
func EmailBarChart(labels []string, series [][]int64, names []string, colors []string) string {
	if len(labels) == 0 || len(series) == 0 {
		return ""
	}
	maxValue := int64(1)
	for _, values := range series {
		for _, value := range values {
			if value > maxValue {
				maxValue = value
			}
		}
	}

	var b strings.Builder
	b.WriteString(`<table role="presentation" data-chart="bar" width="100%" cellpadding="0" cellspacing="0" border="0" style="width:100%;border:1px solid #e5e5e5;background:#ffffff;border-collapse:collapse;">`)
	if len(names) > 0 {
		b.WriteString(`<tr><td colspan="2" style="padding:8px 10px 2px;font-size:11px;color:#737373;">`)
		for i, name := range names {
			if strings.TrimSpace(name) == "" {
				continue
			}
			color := emailChartColor(colors, i)
			fmt.Fprintf(&b, `<span data-series="%s" style="display:inline-block;margin:0 14px 4px 0;color:%s;font-weight:600;">■ %s</span>`, html.EscapeString(name), color, html.EscapeString(name))
		}
		b.WriteString(`</td></tr>`)
	}
	for si, values := range series {
		name := ""
		if si < len(names) {
			name = names[si]
		}
		color := emailChartColor(colors, si)
		fmt.Fprintf(&b, `<tr><td style="width:70px;padding:8px 6px 4px 10px;vertical-align:top;font-size:11px;color:#737373;white-space:nowrap;">%s</td><td style="padding:8px 10px 4px 0;">`, html.EscapeString(name))
		b.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="3" border="0" style="width:100%;table-layout:fixed;border-collapse:separate;"><tr>`)
		for i, label := range labels {
			value := int64(0)
			if i < len(values) && values[i] > 0 {
				value = values[i]
			}
			barWidth := value * 100 / maxValue
			if value > 0 && barWidth < 2 {
				barWidth = 2
			}
			fmt.Fprintf(&b, `<td valign="bottom" style="height:62px;padding:0;text-align:center;vertical-align:bottom;"><div style="height:42px;line-height:42px;font-size:11px;color:#0a0a0a;">%d</div><div style="height:6px;background:%s;width:%d%%;margin:0 auto;border-radius:2px 2px 0 0;font-size:1px;line-height:1px;">&nbsp;</div><div style="padding-top:4px;font-size:10px;color:#737373;white-space:nowrap;">%s</div></td>`, value, color, barWidth, html.EscapeString(label))
		}
		b.WriteString(`</tr></table></td></tr>`)
	}
	b.WriteString(`</table>`)
	return b.String()
}

func emailChartColor(colors []string, index int) string {
	if index < len(colors) && strings.TrimSpace(colors[index]) != "" {
		return colors[index]
	}
	return "#171717"
}
