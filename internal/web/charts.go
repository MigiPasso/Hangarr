package web

import (
	"fmt"
	"html"
	"html/template"
	"math"
	"strings"

	"cleanarr/internal/i18n"
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
func barChart(l i18n.Localizer, bars []bar, unit, label string) template.HTML {
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
			left-6, y, html.EscapeString(l.Num(f*scale)+unit))
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

// weekdayKeys are the short weekday names, Monday first (translated).
var weekdayKeys = []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

// heatmap renders a 7 × 24 grid; intensity uses a one-hue sequential ramp
// in 5 steps (CSS classes h1..h5), empty cells stay on the surface.
func heatmap(l i18n.Localizer, grid [7][24]heatCell, label string) template.HTML {
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
			left-6, y+cell/2, html.EscapeString(l.T(weekdayKeys[d])))
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

// pairedBars renders two thin horizontal bars per category (series 1 above
// series 2) on a shared scale, and the two values in right-aligned columns
// headed by the series names, so numbers line up and never collide.
func pairedBars(l i18n.Localizer, rows []pairRow, nameA, nameB, label string) template.HTML {
	const (
		left    = 118.0 // category labels
		plotW   = 280.0
		gap     = 16.0 // plot → value columns
		colW    = 92.0 // each value column, right-aligned
		head    = 22.0 // column headers
		rowH    = 32.0
		barH    = 9.0
		pairGap = 3.0
	)
	maxV := 0.0
	for _, r := range rows {
		maxV = math.Max(maxV, math.Max(r.A, r.B))
	}
	scale := math.Max(niceMax(maxV*100)/100, 0.01)
	width := left + plotW + gap + 2*colW
	height := head + float64(len(rows))*rowH
	colA, colB := left+plotW+gap+colW, left+plotW+gap+2*colW
	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg class="chart" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, width, height, html.EscapeString(label))
	fmt.Fprintf(&sb, `<text class="axis" x="%.0f" y="13" text-anchor="end">%s</text>`, colA, html.EscapeString(nameA))
	fmt.Fprintf(&sb, `<text class="axis" x="%.0f" y="13" text-anchor="end">%s</text>`, colB, html.EscapeString(nameB))
	fmt.Fprintf(&sb, `<line class="grid" x1="%.0f" x2="%.0f" y1="%.0f" y2="%.0f"/>`, left, left, head, height)
	for i, r := range rows {
		top := head + float64(i)*rowH
		y0 := top + (rowH-(2*barH+pairGap))/2
		mid := y0 + barH + pairGap/2
		fmt.Fprintf(&sb, `<g class="mark"><rect class="hit" x="0" y="%.1f" width="%.0f" height="%.0f"/>`, top, width, rowH)
		for j, v := range []float64{r.A, r.B} {
			class := "bar"
			if j == 1 {
				class = "bar2"
			}
			if w := v / scale * plotW; w > 0 {
				fmt.Fprintf(&sb, `<rect class="%s" x="%.0f" y="%.1f" width="%.1f" height="%.0f" rx="2"/>`,
					class, left+1, y0+float64(j)*(barH+pairGap), math.Max(w, 2), barH)
			}
		}
		fmt.Fprintf(&sb, `<title>%s · %s %s · %s %s</title></g>`, html.EscapeString(r.Label),
			html.EscapeString(nameA), l.Pct(r.A), html.EscapeString(nameB), l.Pct(r.B))
		fmt.Fprintf(&sb, `<text class="axis-label" x="%.0f" y="%.1f" text-anchor="end" dominant-baseline="middle">%s</text>`,
			left-8, mid, html.EscapeString(r.Label))
		for _, c := range []struct {
			x float64
			v float64
		}{{colA, r.A}, {colB, r.B}} {
			fmt.Fprintf(&sb, `<text class="value" x="%.0f" y="%.1f" text-anchor="end" dominant-baseline="middle">%s</text>`,
				c.x, mid, l.Pct(c.v))
		}
	}
	sb.WriteString(`</svg>`)
	return template.HTML(sb.String()) //nolint:gosec // built from escaped values only
}
