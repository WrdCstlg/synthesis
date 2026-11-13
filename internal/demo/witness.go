package demo

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"synthesis.dev/synthesis/pkg/schema"
)

var (
	witnessOpPattern     = regexp.MustCompile(`process (\d+) (read|write) (\S+)(?: -> (\S+))? \[(invoke|ok|fail|info) op_id=(\d+)(?: error=(\S+))?\]`)
	witnessInjectPattern = regexp.MustCompile(`^inject (\S+) -> (\S+)`)
	witnessWithdrPattern = regexp.MustCompile(`^withdraw (\S+)`)
	witnessWhyPattern    = regexp.MustCompile(`At that point the register held (\S+), written by op (\d+), and op (\d+) \(process (\d+), read\) returned (\S+)\. That value was written by op (\d+), which every linearization places before the value the register held\.`)
	witnessCountPattern  = regexp.MustCompile(`no linearization exists for the (\d+) operation`)
	witnessStatesPattern = regexp.MustCompile(`exhausted \((\d+) state\(s\) explored over (\d+) step`)
)

type witnessOpRow struct {
	id        int64
	process   int64
	kind      string
	key       string
	value     string
	hasInvoke bool
	invokeMS  int64
	hasEnd    bool
	endMS     int64
	endType   string
	errCode   string
}

type witnessFaultSpan struct {
	spec  string
	node  string
	from  int64
	hasTo bool
	to    int64
}

// RenderWitnessSVG renders a FAIL verdict's causal timeline and witness into a standalone SVG.
func RenderWitnessSVG(verdict *schema.Verdict) (string, error) {
	if verdict == nil || len(verdict.Violations) == 0 {
		return "", fmt.Errorf("witness svg: verdict carries no violations")
	}
	v := verdict.Violations[0]
	wanted := make(map[int64]bool, len(v.Witness.OpIDs))
	for _, id := range v.Witness.OpIDs {
		wanted[id] = true
	}

	ops := map[int64]*witnessOpRow{}
	var fault *witnessFaultSpan

	for _, e := range v.CausalTimeline {
		switch e.Event {
		case schema.TimelineOp:
			m := witnessOpPattern.FindStringSubmatch(e.Detail)
			if m == nil {
				continue
			}
			proc, _ := strconv.ParseInt(m[1], 10, 64)
			id, _ := strconv.ParseInt(m[6], 10, 64)
			if len(wanted) > 0 && !wanted[id] {
				continue
			}
			op, ok := ops[id]
			if !ok {
				op = &witnessOpRow{id: id, process: proc, kind: m[2], key: m[3]}
				ops[id] = op
			}
			if m[5] == "invoke" {
				op.hasInvoke = true
				op.invokeMS = e.TMS
				if m[2] == "write" {
					op.value = m[4]
				}
			} else {
				op.hasEnd = true
				op.endMS = e.TMS
				op.endType = m[5]
				op.errCode = m[7]
				if m[2] == "read" {
					op.value = m[4]
				} else if m[2] == "write" && op.value == "" {
					op.value = m[4]
				}
			}
		case schema.TimelineFault:
			if inj := witnessInjectPattern.FindStringSubmatch(e.Detail); inj != nil && fault == nil {
				fault = &witnessFaultSpan{spec: inj[1], node: inj[2], from: e.TMS}
			} else if wd := witnessWithdrPattern.FindStringSubmatch(e.Detail); wd != nil && fault != nil && !fault.hasTo {
				fault.hasTo = true
				fault.to = e.TMS
			}
		}
	}

	why := witnessWhyPattern.FindStringSubmatch(v.Explanation)
	countMatch := witnessCountPattern.FindStringSubmatch(v.Explanation)
	statesMatch := witnessStatesPattern.FindStringSubmatch(v.Explanation)

	var order []int64
	if why != nil {
		idInitial, _ := strconv.ParseInt(why[6], 10, 64)
		idReplacer, _ := strconv.ParseInt(why[2], 10, 64)
		idStale, _ := strconv.ParseInt(why[3], 10, 64)
		order = []int64{idInitial, idReplacer, idStale}
	} else {
		order = append(order, v.Witness.OpIDs...)
	}

	var rows []*witnessOpRow
	for _, id := range order {
		if op, ok := ops[id]; ok && op.hasInvoke {
			rows = append(rows, op)
		}
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("witness svg: causal timeline contains none of the witness operations")
	}

	key := v.Witness.Key
	if key == "" {
		key = rows[0].key
	}

	var gapLine string
	if why != nil {
		idReplacer, _ := strconv.ParseInt(why[2], 10, 64)
		idStale, _ := strconv.ParseInt(why[3], 10, 64)
		idInitial, _ := strconv.ParseInt(why[6], 10, 64)
		replacer := ops[idReplacer]
		staleRead := ops[idStale]
		if replacer != nil && staleRead != nil && replacer.hasEnd && staleRead.hasInvoke && replacer.endType == "ok" {
			gap := staleRead.invokeMS - replacer.endMS
			if gap >= 0 {
				gapLine = fmt.Sprintf("op %d was invoked %d ms after op %d had completed, and returned the value op %d wrote.",
					staleRead.id, gap, replacer.id, idInitial)
			}
		}
	}

	const (
		W     = 920.0
		LEFT  = 188.0
		RIGHT = 250.0
		TOP   = 132.0
		ROWH  = 44.0
	)

	var times []int64
	for _, o := range rows {
		times = append(times, o.invokeMS)
		if o.hasEnd {
			times = append(times, o.endMS)
		} else {
			times = append(times, o.invokeMS)
		}
	}
	if fault != nil {
		times = append(times, fault.from)
		if fault.hasTo {
			times = append(times, fault.to)
		}
	}
	minT, maxT := times[0], times[0]
	for _, t := range times[1:] {
		if t < minT {
			minT = t
		}
		if t > maxT {
			maxT = t
		}
	}
	t0 := math.Floor(float64(minT-600)/500.0) * 500.0
	t1 := math.Ceil(float64(maxT+600)/500.0) * 500.0
	if t1 <= t0 {
		t1 = t0 + 1000.0
	}
	xPos := func(t float64) float64 {
		return LEFT + ((t-t0)/(t1-t0))*(W-LEFT-RIGHT)
	}

	bandY := TOP
	firstRowY := TOP + 30.0
	if fault != nil {
		firstRowY = TOP + 62.0
	}
	axisY := firstRowY + float64(len(rows))*ROWH + 4.0

	var quote string
	if why != nil {
		quote = why[0]
	}
	var quoteLines []string
	if quote != "" {
		quoteLines = wrapSVGText("\u201c"+quote+"\u201d", 118)
	}
	quoteY := axisY + 48.0
	if gapLine != "" {
		quoteY += 30.0
	}
	H := int(quoteY) + len(quoteLines)*18 + 36

	var ticks []string
	for t := math.Ceil(t0/1000.0) * 1000.0; t <= t1; t += 1000.0 {
		xp := fmtFloat(xPos(t))
		ticks = append(ticks, fmt.Sprintf(
			`<line class="grid" x1="%s" y1="%s" x2="%s" y2="%s"/><text class="tick" x="%s" y="%s" text-anchor="middle">t+%.0f s</text>`,
			xp, fmtFloat(bandY-6), xp, fmtFloat(axisY), xp, fmtFloat(axisY+16), t/1000.0,
		))
	}

	var band string
	if fault != nil {
		a := math.Max(xPos(float64(fault.from)), LEFT)
		toVal := t1
		if fault.hasTo {
			toVal = float64(fault.to)
		}
		b := math.Min(xPos(toVal), W-RIGHT)
		span := fmt.Sprintf("from t+%d ms", fault.from)
		if fault.hasTo {
			span = fmt.Sprintf("t+%d..%d ms", fault.from, fault.to)
		}
		specBase := fault.spec
		if idx := strings.IndexByte(specBase, '@'); idx >= 0 {
			specBase = specBase[:idx]
		}
		band = fmt.Sprintf(
			`<rect class="band" x="%s" y="%s" width="%s" height="%s" rx="3"/>`+
				`<text class="bandlabel" x="%s" y="%s">network partition: %s cut off</text>`+
				`<text class="bandspec" x="%s" y="%s">%s · %s</text>`,
			fmtFloat(a), fmtFloat(bandY), fmtFloat(math.Max(2, b-a)), fmtFloat(axisY-bandY),
			fmtFloat(a+10), fmtFloat(bandY+20), svgEsc(fault.node),
			fmtFloat(a+10), fmtFloat(bandY+37), svgEsc(specBase), svgEsc(span),
		)
	}

	rowLines := make([]string, 0, len(rows))
	for i, o := range rows {
		y := firstRowY + float64(i)*ROWH
		a := xPos(float64(o.invokeMS))
		endVal := o.invokeMS
		if o.hasEnd {
			endVal = o.endMS
		}
		b := xPos(float64(endVal))
		isRead := o.kind == "read"
		indeterminate := o.endType == "info"
		cls := "write"
		if isRead {
			cls = "read"
		} else if indeterminate {
			cls = "maybe"
		}
		when := fmt.Sprintf("invoked t+%d ms", o.invokeMS)
		if o.hasEnd {
			when = fmt.Sprintf("t+%d..%d ms", o.invokeMS, o.endMS)
		}
		var outcome string
		switch {
		case isRead:
			outcome = fmt.Sprintf("read returned %s · %s", o.value, when)
		case indeterminate:
			outcome = fmt.Sprintf("write %s · timed out, may or may not have applied · %s", o.value, when)
		default:
			outcome = fmt.Sprintf("write %s · %s · %s", o.value, o.endType, when)
		}
		noteW := float64(len(outcome)) * 6.9
		after := math.Max(a, b) + 10.0
		noteX := after
		anchor := "start"
		if after+noteW > W-20.0 {
			noteX = a - 10.0
			anchor = "end"
		}
		noteClass := "note"
		if isRead {
			noteClass = "note noteread"
		}
		rowLines = append(rowLines, fmt.Sprintf(
			`<text class="rowlabel" x="%s" y="%s" text-anchor="end">op %d · process %d</text>`+
				`<rect class="%s" x="%s" y="%s" width="%s" height="18" rx="4"/>`+
				`<text class="%s" x="%s" y="%s" text-anchor="%s">%s</text>`,
			fmtFloat(LEFT-14), fmtFloat(y+5), o.id, o.process,
			cls, fmtFloat(a), fmtFloat(y-9), fmtFloat(math.Max(4, b-a)),
			noteClass, fmtFloat(noteX), fmtFloat(y+5), anchor, svgEsc(outcome),
		))
	}

	title := fmt.Sprintf("A stale read, caught: key %s", key)
	sub := "linearizable.kv found no order of this key\u2019s operations that a correct register could produce."
	if countMatch != nil {
		statesSuffix := ""
		if statesMatch != nil {
			if n, err := strconv.ParseInt(statesMatch[1], 10, 64); err == nil {
				statesSuffix = fmt.Sprintf(" (%s states)", formatThousands(n))
			}
		}
		sub = fmt.Sprintf("linearizable.kv exhausted its search of the %s operations on %s%s and found no order a correct register could produce.",
			countMatch[1], key, statesSuffix)
	}
	sub2 := "Below: the three operations where its deepest attempt broke. They point at the failure; the proof is the whole search."

	var quoteElems []string
	for i, l := range quoteLines {
		quoteElems = append(quoteElems, fmt.Sprintf(`<text class="quote" x="28" y="%d">%s</text>`, int(quoteY)+i*18, svgEsc(l)))
	}
	gapElem := ""
	if gapLine != "" {
		gapElem = fmt.Sprintf(`<text class="gap" x="28" y="%s">%s</text>`, fmtFloat(axisY+48), svgEsc(gapLine))
	}

	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="920" height="%d" viewBox="0 0 920 %d" role="img" aria-labelledby="wt wd">
<title id="wt">%s</title>
<desc id="wd">%s</desc>
<style>
:root{--bg:#ffffff;--fg:#1f2328;--muted:#59636e;--hair:#d1d9e0;--band:rgba(191,135,0,0.10);--bandedge:rgba(191,135,0,0.45);--bandfg:#7d4e00;--write:#0969da;--maybe:#9a6700;--read:#cf222e}
@media (prefers-color-scheme:dark){:root{--bg:#0d1117;--fg:#e6edf3;--muted:#9198a1;--hair:#30363d;--band:rgba(210,153,34,0.12);--bandedge:rgba(210,153,34,0.45);--bandfg:#e3b341;--write:#4493f8;--maybe:#d29922;--read:#ff7b72}}
text{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","Noto Sans",Helvetica,Arial,sans-serif;fill:var(--fg)}
.bg{fill:var(--bg);stroke:var(--hair)}.h1{font-size:19px;font-weight:600}.sub{font-size:13px;fill:var(--muted)}
.grid{stroke:var(--hair);stroke-width:1}.tick{font-size:11px;fill:var(--muted);font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
.band{fill:var(--band);stroke:var(--bandedge);stroke-width:1}.bandlabel{font-size:12px;fill:var(--bandfg);font-weight:600}
.bandspec{font-size:11px;fill:var(--bandfg);font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}.gap{font-size:13px;fill:var(--fg);font-weight:600}
.rowlabel{font-size:12.5px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;fill:var(--fg)}
.write{fill:var(--write)}.read{fill:var(--read)}.maybe{fill:none;stroke:var(--maybe);stroke-width:2;stroke-dasharray:5 4}
.note{font-size:12.5px;fill:var(--muted)}.noteread{fill:var(--read);font-weight:600}
.quote{font-size:12.5px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;fill:var(--fg)}.foot{font-size:11px;fill:var(--muted)}
</style>
<rect class="bg" x="0.5" y="0.5" width="919" height="%d" rx="10"/>
<text class="h1" x="28" y="42">%s</text>
<text class="sub" x="28" y="66">%s</text>
<text class="sub" x="28" y="86">%s</text>
<text class="sub" x="28" y="106">Times are from the start of the client load (t+0), as the run recorded them.</text>
%s
%s
%s
%s
%s
<text class="foot" x="28" y="%d">The checker’s words, quoted from verdict.json of run %s. Drawn by synthesis.</text>
</svg>
`,
		H, H,
		svgEsc(title),
		svgEsc(fmt.Sprintf("%s %s Drawn from verdict.json of run %s.", sub, quote, verdict.RunID)),
		H-1,
		svgEsc(title),
		svgEsc(sub),
		svgEsc(sub2),
		strings.Join(ticks, "\n"),
		band,
		strings.Join(rowLines, "\n"),
		gapElem,
		strings.Join(quoteElems, "\n"),
		H-18,
		svgEsc(verdict.RunID),
	)
	return svg, nil
}

func svgEsc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func fmtFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func wrapSVGText(s string, width int) []string {
	words := strings.Split(s, " ")
	var lines []string
	var cur string
	for _, w := range words {
		cand := strings.TrimSpace(cur + " " + w)
		if len(cand) > width && cur != "" {
			lines = append(lines, strings.TrimSpace(cur))
			cur = w
		} else {
			cur = cand
		}
	}
	if strings.TrimSpace(cur) != "" {
		lines = append(lines, strings.TrimSpace(cur))
	}
	return lines
}

func formatThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	rem := len(s) % 3
	if rem > 0 {
		out = append(out, s[:rem]...)
		if len(s) > rem {
			out = append(out, ',')
		}
	}
	for i := rem; i < len(s); i += 3 {
		out = append(out, s[i:i+3]...)
		if i+3 < len(s) {
			out = append(out, ',')
		}
	}
	return string(out)
}
