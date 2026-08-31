package service

import (
	"fmt"
	"html"
	"strings"
)

// EmailBarChart renders a table-only chart for email clients that strip SVG.
func EmailBarChart(labels []string, series [][]int64, names []string, colors []string) string {
	if len(labels) == 0 || len(series) == 0 {
		return ""
	}
	maxV := int64(1)
	for _, values := range series {
		for _, value := range values {
			if value > maxV {
				maxV = value
			}
		}
	}

	var b strings.Builder
	b.WriteString(`<table data-report-chart="email-bar" role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="border-collapse:collapse;table-layout:fixed;background:#ffffff;">`)
	b.WriteString(`<tr>`)
	for i, label := range labels {
		b.WriteString(`<td valign="bottom" align="center" style="height:150px;padding:0 3px 6px;border-bottom:1px solid #d4d4d4;">`)
		b.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="2" border="0" style="height:142px;table-layout:fixed;"><tr>`)
		for si, values := range series {
			value := int64(0)
			if i < len(values) && values[i] > 0 {
				value = values[i]
			}
			color := "#171717"
			if si < len(colors) && colors[si] != "" {
				color = colors[si]
			}
			height := int64(2)
			if value > 0 {
				height = value * 110 / maxV
				if height < 4 {
					height = 4
				}
			}
			fmt.Fprintf(&b, `<td valign="bottom" align="center" style="font-size:10px;color:#525252;">%d<div title="%s: %d" style="height:%dpx;line-height:%dpx;background:%s;border-radius:3px 3px 0 0;font-size:0;">&nbsp;</div></td>`,
				value, html.EscapeString(seriesName(names, si)), value, height, height, html.EscapeString(color))
		}
		b.WriteString(`</tr></table>`)
		fmt.Fprintf(&b, `<div style="padding-top:5px;font-size:10px;color:#737373;white-space:nowrap;">%s</div></td>`, html.EscapeString(label))
	}
	b.WriteString(`</tr><tr><td colspan="`)
	b.WriteString(fmt.Sprintf("%d", len(labels)))
	b.WriteString(`" style="padding-top:10px;font-size:11px;color:#737373;">`)
	for i, name := range names {
		color := "#171717"
		if i < len(colors) && colors[i] != "" {
			color = colors[i]
		}
		fmt.Fprintf(&b, `<span style="display:inline-block;margin-right:14px;"><span style="display:inline-block;width:8px;height:8px;margin-right:4px;border-radius:2px;background:%s;"></span>%s</span>`, html.EscapeString(color), html.EscapeString(name))
	}
	b.WriteString(`</td></tr></table>`)
	return b.String()
}

func seriesName(names []string, index int) string {
	if index < len(names) {
		return names[index]
	}
	return fmt.Sprintf("系列 %d", index+1)
}
