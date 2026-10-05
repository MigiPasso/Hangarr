package web

import (
	"fmt"
	"html"
	"html/template"
	"math"
	"strings"
)

// Server-side SVG charts: no JavaScript, CSP-safe (colors come from CSS
// classes, never style attributes). Every mark carries a <title> tooltip and
// each chart is paired with a data table in the page.

type bar struct {
	Label string  // x tick label, shown when Tick is true
	Tick  bool    // show the label under the bar
	Value float64 // in the chart unit
	Tip   string  // tooltip text
}

// niceMax rounds up to 1, 2 or 5 × 10^n so the 0 / half / max gridlines are round.
func niceMax(v float64) float64 {
	if v <= 0 {
		return 1
	}
	exp := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 5, 10} { // halves stay round: 0.5, 1, 2.5, 5
		if m*exp >= v {
			return m * exp
		}
	}
	return 10 * exp
}

func trimFloat(v float64) string {
	s := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", v), "0"), ".")
	return strings.Replace(s, ".", ",", 1)
}

// topRoundedBar draws a bar anchored on the baseline with 4 px rounded top
// corners (the data end).
func topRoundedBar(x, y, w, h float64) string {
	r := math.Min(4, math.Min(w/2, h))
	if h <= 0 {
		return ""
	}
	return fmt.Sprintf(`M%.1f,%.1f V%.1f Q%.1f,%.1f %.1f,%.1f H%.1f Q%.1f,%.1f %.1f,%.1f V%.1f Z`,
		x, y+h, y+r, x, y, x+r, y, x+w-r, x+w, y, x+w, y+r, y+h)
}

// barChart renders a single-series vertical bar chart (one hue, no legend:
// the section title names the series).
func barChart(bars []bar, unit, label string) template.HTML {
	const (
		slot   = 22.0
		left   = 44.0
		top    = 10.0
		plotH  = 140.0
		bottom = 26.0
	)
	width := left + slot*float64(len(bars)) + 4
	height := top + plotH + bottom
	maxV := 0.0
	for _, b := range bars {
		maxV = math.Max(maxV, b.Value)
	}
	scale := niceMax(maxV)
	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg class="chart" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, width, height, html.EscapeString(label))
	for _, f := range []float64{0, 0.5, 1} {
		y := top + plotH - f*plotH
		fmt.Fprintf(&sb, `<line class="grid" x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f"/>`, left, width-2, y, y)
		fmt.Fprintf(&sb, `<text class="axis" x="%.0f" y="%.1f" text-anchor="end" dominant-baseline="middle">%s</text>`,
			left-6, y, html.EscapeString(trimFloat(f*scale)+unit))
	}
	for i, b := range bars {
		x := left + float64(i)*slot + 1 // 2 px gap between bars
		h := b.Value / scale * plotH
		fmt.Fprintf(&sb, `<g class="mark"><rect class="hit" x="%.1f" y="%.0f" width="%.1f" height="%.0f"/>`, x-1, top, slot, plotH)
		if p := topRoundedBar(x, top+plotH-h, slot-2, h); p != "" {
			fmt.Fprintf(&sb, `<path class="bar" d="%s"/>`, p)
		}
		fmt.Fprintf(&sb, `<title>%s</title></g>`, html.EscapeString(b.Tip))
		if b.Tick {
			fmt.Fprintf(&sb, `<text class="axis" x="%.1f" y="%.0f" text-anchor="middle">%s</text>`,
				x+(slot-2)/2, top+plotH+19, html.EscapeString(b.Label))
		}
	}
	sb.WriteString(`</svg>`)
	return template.HTML(sb.String()) //nolint:gosec // built from escaped values only
}

// heatCell is one weekday × hour cell.
type heatCell struct {
	Count int
	Tip   string
}

var weekdays = []string{"Lun", "Mar", "Mer", "Jeu", "Ven", "Sam", "Dim"}

// heatmap renders a 7 × 24 grid; intensity uses a one-hue sequential ramp
// in 5 steps (CSS classes h1..h5), empty cells stay on the surface.
func heatmap(grid [7][24]heatCell, label string) template.HTML {
	const (
		cell = 18.0
		gap  = 2.0
		left = 40.0
		top  = 16.0
	)
	maxC := 0
	for _, row := range grid {
		for _, c := range row {
			maxC = max(maxC, c.Count)
		}
	}
	width := left + 24*(cell+gap)
	height := top + 7*(cell+gap)
	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg class="chart heatmap" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, width, height, html.EscapeString(label))
	for h := 0; h < 24; h += 3 {
		fmt.Fprintf(&sb, `<text class="axis" x="%.1f" y="10" text-anchor="middle">%dh</text>`, left+float64(h)*(cell+gap)+cell/2, h)
	}
	for d := range 7 {
		y := top + float64(d)*(cell+gap)
		fmt.Fprintf(&sb, `<text class="axis" x="%.0f" y="%.1f" text-anchor="end" dominant-baseline="middle">%s</text>`,
			left-6, y+cell/2, weekdays[d])
		for h := range 24 {
			c := grid[d][h]
			level := 0
			if c.Count > 0 && maxC > 0 {
				level = 1 + (c.Count-1)*5/maxC
				level = min(level, 5)
			}
			fmt.Fprintf(&sb, `<rect class="heat h%d" x="%.1f" y="%.1f" width="%.0f" height="%.0f" rx="3"><title>%s</title></rect>`,
				level, left+float64(h)*(cell+gap), y, cell, cell, html.EscapeString(c.Tip))
		}
	}
	sb.WriteString(`</svg>`)
	return template.HTML(sb.String()) //nolint:gosec // built from escaped values only
}

// pairRow is one category of a paired horizontal bar chart (two measures on
// the same 0-100 % scale).
type pairRow struct {
	Label string
	A, B  float64 // 0..1
}

// pairedBars renders two horizontal bars per row: series-1 (A) and series-2
// (B), each with its value written at the bar end. The legend is in HTML.
func pairedBars(rows []pairRow, nameA, nameB, label string) template.HTML {
	const (
		left  = 120.0
		plotW = 360.0
		rowH  = 30.0
		barH  = 9.0
		right = 48.0
	)
	maxV := 0.0
	for _, r := range rows {
		maxV = math.Max(maxV, math.Max(r.A, r.B))
	}
	scale := math.Max(niceMax(maxV*100)/100, 0.01)
	width := left + plotW + right
	height := float64(len(rows))*rowH + 4
	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg class="chart" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, width, height, html.EscapeString(label))
	for i, r := range rows {
		y := float64(i)*rowH + 4
		fmt.Fprintf(&sb, `<text class="axis-label" x="%.0f" y="%.1f" text-anchor="end" dominant-baseline="middle">%s</text>`,
			left-8, y+barH+1, html.EscapeString(r.Label))
		for j, v := range []float64{r.A, r.B} {
			by := y + float64(j)*(barH+2) // 2 px gap between the pair
			w := v / scale * plotW
			name, class := nameA, "bar"
			if j == 1 {
				name, class = nameB, "bar2"
			}
			pct := trimFloat(v*100) + " %"
			fmt.Fprintf(&sb, `<g class="mark"><rect class="hit" x="%.0f" y="%.1f" width="%.0f" height="%.0f"/>`, left, by, plotW, barH+2)
			if w > 0 {
				fmt.Fprintf(&sb, `<rect class="%s" x="%.0f" y="%.1f" width="%.1f" height="%.0f" rx="2"/>`, class, left, by, w, barH)
			}
			fmt.Fprintf(&sb, `<title>%s · %s : %s</title></g>`, html.EscapeString(r.Label), html.EscapeString(name), pct)
			fmt.Fprintf(&sb, `<text class="value" x="%.1f" y="%.1f" dominant-baseline="middle">%s</text>`,
				left+w+5, by+barH/2+0.5, html.EscapeString(pct))
		}
	}
	sb.WriteString(`</svg>`)
	return template.HTML(sb.String()) //nolint:gosec // built from escaped values only
}
