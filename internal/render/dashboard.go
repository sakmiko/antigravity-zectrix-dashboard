package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/png"
	"math"
	"strings"
	"time"

	"github.com/sakmiko/antigravity-zectrix-dashboard/internal/predictor"
)

const (
	DisplayWidth  = 400
	DisplayHeight = 300
)

type FrameResult struct {
	PNG       []byte
	SHA256Hex string
	Width     int
	Height    int
}

type tierBadgeLayout struct {
	label     string
	x         int
	y         int
	w         int
	h         int
	textColor uint8
	textX     int
	textY     int
}

// RenderDashboard renders a refined 400x300 16-level grayscale PNG optimized for
// ZECTRIX NOTE4 e-paper displays, with subscription tier badges, rounded focus
// card, curve-matched pool swatches, and analytical SDF charts.
func RenderDashboard(fc *predictor.Forecast, loc *time.Location) (*FrameResult, error) {
	if loc == nil {
		loc = time.UTC
	}
	w, h := DisplayWidth, DisplayHeight
	canvas := make([]float64, w*h)
	for i := range canvas {
		canvas[i] = 255.0
	}

	// 1. Header Tier Badge & optional Keepalive indicator
	titleEndX := 12 + measureGridFont("ANTIGRAVITY", Mono8x11Bold, 2)
	var tierBadge *tierBadgeLayout
	if tier := strings.ToUpper(strings.TrimSpace(fc.PlanTier)); tier != "" {
		tw := measureGridFont(tier, Mono7x10Bold, 1)
		bx := titleEndX + 10
		by := 6
		bw := tw + 12
		bh := 15
		textColor := uint8(0)
		switch tier {
		case "ULTRA":
			// Prestigious double-framed solid dark pill
			blendSDFRoundedRectBorder(canvas, w, h, float64(bx)-2, float64(by)-2, float64(bw)+4, float64(bh)+4, 4.5, 1.2, 255, 0)
			blendSDFRoundedRect(canvas, w, h, float64(bx), float64(by), float64(bw), float64(bh), 3.0, 0)
			textColor = 255
		case "PRO":
			// Solid dark-ink pill
			blendSDFRoundedRect(canvas, w, h, float64(bx), float64(by), float64(bw), float64(bh), 3.5, 17)
			textColor = 255
		case "PLUS":
			// High-contrast outlined pill
			blendSDFRoundedRectBorder(canvas, w, h, float64(bx), float64(by), float64(bw), float64(bh), 3.5, 1.5, 238, 0)
			textColor = 0
		case "FREE":
			// Muted light pill
			blendSDFRoundedRectBorder(canvas, w, h, float64(bx), float64(by), float64(bw), float64(bh), 3.5, 1.0, 238, 119)
			textColor = 0
		default:
			blendSDFRoundedRectBorder(canvas, w, h, float64(bx), float64(by), float64(bw), float64(bh), 3.5, 1.2, 238, 85)
			textColor = 0
		}
		tierBadge = &tierBadgeLayout{
			label:     tier,
			x:         bx,
			y:         by,
			w:         bw,
			h:         bh,
			textColor: textColor,
			textX:     bx + 6,
			textY:     by + 3,
		}
	}

	// Optional subtle keepalive pill near status dot when enabled
	var kaX int
	if fc.KeepaliveEnabled {
		kaW := measureGridFont("KA", Mono7x10Bold, 1) + 8
		kaX = 320 - kaW - 6
		blendSDFRoundedRectBorder(canvas, w, h, float64(kaX), 7, float64(kaW), 13, 3.0, 1.0, 255, 136)
	}

	// Top status dot (filled black if fresh, hollow ring if stale)
	if fc.Stale {
		blendSDFCircle(canvas, w, h, 329.5, 13.5, 3.6, 1.8, 255, 0, 0.75)
	} else {
		blendSDFCircle(canvas, w, h, 329.5, 13.5, 3.5, 0, 0, 0, 0.75)
	}

	// 2. Inverted dark-ink rounded bottleneck left focus panel (x=12..148, y=34..162)
	blendSDFRoundedRect(canvas, w, h, 12, 34, 136, 128, 5.0, 17.0)
	// Subtle internal divider inside focus card
	for px := 20; px <= 140; px++ {
		canvas[53*w+px] = 68.0
	}
	bn := fc.Bottleneck
	blendPillBar(canvas, w, h, 20, 129, 120, 7, bn.RemainingPct/100.0, 68, 255)

	// 3. Right dual-pool cards (symmetrically balanced around y=98)
	// Matching curve legend swatches (x=162..165)
	blendSDFRoundedRect(canvas, w, h, 162, 37, 4, 11, 1.5, 68)
	blendPillBar(canvas, w, h, 162, 83, 226, 6, fc.Gemini.RemainingPct/100.0, 221, 68)

	blendSDFRoundedRect(canvas, w, h, 162, 104, 4, 11, 1.5, 0)
	blendPillBar(canvas, w, h, 162, 150, 226, 6, fc.Claude.RemainingPct/100.0, 221, 0)

	// 4. Bottom Weekly pill bar
	if fc.Weekly.Available {
		blendPillBar(canvas, w, h, 80, 280, 184, 7, fc.Weekly.RemainingPct/100.0, 221, 34)
	} else {
		blendSDFRoundedRect(canvas, w, h, 80, 280, 184, 7, 3.5, 238)
	}

	// 5. Pixel-snapped crisp structural rules
	for px := 12; px <= 388; px++ {
		canvas[27*w+px] = 0
		canvas[28*w+px] = 0
		canvas[168*w+px] = 102
		canvas[270*w+px] = 170
	}
	for px := 162; px <= 388; px++ {
		canvas[98*w+px] = 187
	}

	// 6. Lower Forecast Chart (x=44..388, y=192..260)
	gx0, gy0, gx1, gy1 := 44, 192, 388, 260
	ymid := (gy0 + gy1) / 2
	nowX := 216

	gemHist, gemPred := buildSeriesPoints(fc.Gemini, gx0, nowX, gx1, gy0, gy1, fc.UpdatedAt)
	claHist, claPred := buildSeriesPoints(fc.Claude, gx0, nowX, gx1, gy0, gy1, fc.UpdatedAt)

	// Light-gray area fill under primary (Claude) actual history curve
	if len(claHist) >= 2 {
		for px := int(math.Ceil(claHist[0].X)); px <= nowX; px++ {
			if px < gx0 {
				continue
			}
			yc := int(math.Round(interpY(claHist, float64(px))))
			if yc < gy0 {
				yc = gy0
			}
			for py := yc; py < gy1; py++ {
				canvas[py*w+px] = 238.0
			}
		}
	}

	// Pixel-snapped 100% & 50% dotted gridlines + 2px solid 0% baseline
	for px := gx0; px <= gx1; px++ {
		if (px-gx0)%4 < 2 {
			canvas[gy0*w+px] = 153.0
			canvas[ymid*w+px] = 153.0
		}
		canvas[gy1*w+px] = 34.0
		canvas[(gy1+1)*w+px] = 34.0
	}
	for py := gy0; py <= gy1; py++ {
		if (py-gy0)%5 < 3 {
			canvas[py*w+nowX] = 68.0
		}
	}

	// Smooth analytical SDF curves (0.75px tight AA -> razor-sharp core + silky edge)
	if len(gemHist) >= 2 {
		blendSDFCurve(canvas, w, h, catmullRomChain(gemHist, 12), 2.0, 68, 0.75)
	}
	if len(gemPred) >= 2 {
		for _, seg := range splitDashedSegments(catmullRomChain(gemPred, 10), 6.0, 4.5) {
			blendSDFCurve(canvas, w, h, seg, 1.8, 85, 0.75)
		}
	}
	if len(claHist) >= 2 {
		blendSDFCurve(canvas, w, h, catmullRomChain(claHist, 12), 2.8, 0, 0.75)
	}
	if len(claPred) >= 2 {
		for _, seg := range splitDashedSegments(catmullRomChain(claPred, 10), 7.0, 4.5) {
			blendSDFCurve(canvas, w, h, seg, 2.4, 0, 0.75)
		}
	}

	// NOW nodes & exhaustion target ring
	if len(gemHist) > 0 {
		last := gemHist[len(gemHist)-1]
		blendSDFCircle(canvas, w, h, last.X, last.Y, 3.2, 1.5, 255, 68, 0.75)
	}
	if len(claHist) > 0 {
		last := claHist[len(claHist)-1]
		blendSDFCircle(canvas, w, h, last.X, last.Y, 4.2, 2.4, 0, 255, 0.75)
	}
	if len(claPred) > 0 {
		end := claPred[len(claPred)-1]
		if end.Y >= float64(gy1)-1.0 {
			blendSDFCircle(canvas, w, h, end.X, float64(gy1), 4.4, 2.0, 255, 0, 0.75)
		}
	}

	// 7. Quantize graphic layer to 16-level e-ink grayscale
	frame := make([]uint8, w*h)
	for i, v := range canvas {
		frame[i] = quantize16Gray(v)
	}

	// 8. Composite 100% pure-ink (0 or 255) MonoGrid Bold pixel typography
	drawGridFont(frame, w, h, 12, 8, "ANTIGRAVITY", Mono8x11Bold, 0, 2)
	if tierBadge != nil {
		drawGridFont(frame, w, h, tierBadge.textX, tierBadge.textY, tierBadge.label, Mono7x10Bold, tierBadge.textColor, 1)
	}
	if fc.KeepaliveEnabled {
		drawGridFont(frame, w, h, kaX+4, 9, "KA", Mono7x10Bold, 68, 1)
	}
	syncStr := fc.UpdatedAt.In(loc).Format("15:04")
	drawGridFontRight(frame, w, h, 388, 8, syncStr, Mono8x11Bold, 0, 2)

	// Left Bottleneck Panel
	drawGridFont(frame, w, h, 20, 40, "RUNWAY", Mono7x10Bold, 255, 2)
	drawGridFontRight(frame, w, h, 140, 40, "FOCUS", Mono7x10Bold, 187, 1)
	drawGridFont(frame, w, h, 20, 58, predictor.FormatRunwayShort(bn.RunwayHours), HeroDigits20x30, 255, 3)
	bnName := bn.Name
	if bnName == "" {
		bnName = "POOL"
	}
	drawGridFont(frame, w, h, 20, 95, bnName, Mono8x11Bold, 255, 2)
	burnText := formatBurn(bn.BurnRatePctH)
	if bn.Available && bn.RunwayHours < 0 {
		burnText = "STABLE"
	}
	drawGridFont(frame, w, h, 20, 112, burnText, Mono7x10Bold, 255, 1)
	drawGridFont(frame, w, h, 20, 144, formatPctInt(bn.RemainingPct, bn.Available), Mono7x10Bold, 255, 1)
	if bn.Available {
		drawGridFontRight(frame, w, h, 140, 144, "RST "+formatResetClock(bn.ResetAt, loc), Mono7x10Bold, 255, 1)
	}

	// Right Row 1: GEMINI (with gray curve swatch)
	drawGridFont(frame, w, h, 170, 37, "GEMINI", Mono8x11Bold, 0, 2)
	drawGridFontRight(frame, w, h, 388, 38, "RST "+formatResetClock(fc.Gemini.ResetAt, loc), Mono7x10Bold, 0, 1)
	drawGridFont(frame, w, h, 162, 55, formatPctInt(fc.Gemini.RemainingPct, fc.Gemini.Available), MedDigits15x22, 0, 3)
	drawGridFontRight(frame, w, h, 388, 54, "~"+predictor.FormatRunwayShort(fc.Gemini.RunwayHours), Mono8x11Bold, 0, 1)
	drawGridFontRight(frame, w, h, 388, 69, formatBurn(fc.Gemini.BurnRatePctH), Mono7x10Bold, 0, 1)

	// Right Row 2: CLAUDE (with black curve swatch)
	drawGridFont(frame, w, h, 170, 104, "CLAUDE", Mono8x11Bold, 0, 2)
	drawGridFontRight(frame, w, h, 388, 105, "RST "+formatResetClock(fc.Claude.ResetAt, loc), Mono7x10Bold, 0, 1)
	drawGridFont(frame, w, h, 162, 122, formatPctInt(fc.Claude.RemainingPct, fc.Claude.Available), MedDigits15x22, 0, 3)
	drawGridFontRight(frame, w, h, 388, 121, "~"+predictor.FormatRunwayShort(fc.Claude.RunwayHours), Mono8x11Bold, 0, 1)
	drawGridFontRight(frame, w, h, 388, 136, formatBurn(fc.Claude.BurnRatePctH), Mono7x10Bold, 0, 1)

	// Forecast header & axis ticks
	drawGridFont(frame, w, h, 12, 175, "FORECAST", Mono7x10Bold, 0, 2)
	drawGridFont(frame, w, h, 203, 175, "NOW", Mono7x10Bold, 0, 2)
	exhaustLabel := "STABLE"
	if bn.RunwayHours >= 0 && !bn.ExhaustAt.IsZero() {
		exhaustLabel = formatExhaustLabel(fc.UpdatedAt, bn.ExhaustAt, loc)
	}
	drawGridFontRight(frame, w, h, 388, 175, exhaustLabel, Mono7x10Bold, 0, 1)

	drawGridFontRight(frame, w, h, 38, 188, "100", Mono7x10Bold, 0, 1)
	drawGridFontRight(frame, w, h, 38, 221, "50", Mono7x10Bold, 0, 1)
	drawGridFontRight(frame, w, h, 38, 255, "0", Mono7x10Bold, 0, 1)

	// Bottom Weekly Strip
	drawGridFont(frame, w, h, 12, 278, "WEEKLY", Mono8x11Bold, 0, 2)
	if fc.Weekly.Available {
		drawGridFont(frame, w, h, 274, 278, formatPctInt(fc.Weekly.RemainingPct, true), Mono8x11Bold, 0, 1)
		wkRem := "--"
		if !fc.Weekly.ResetAt.IsZero() {
			wkRem = predictor.FormatDurationCompact(fc.Weekly.ResetAt.Sub(fc.UpdatedAt))
		}
		drawGridFontRight(frame, w, h, 388, 279, wkRem, Mono7x10Bold, 0, 1)
	} else {
		drawGridFont(frame, w, h, 274, 278, "N/A", Mono8x11Bold, 102, 1)
		drawGridFontRight(frame, w, h, 388, 279, "--", Mono7x10Bold, 102, 1)
	}

	// Encode 8-bit Grayscale PNG + SHA-256 hash of pixel buffer
	sum := sha256.Sum256(frame)
	img := &image.Gray{
		Pix:    frame,
		Stride: w,
		Rect:   image.Rect(0, 0, w, h),
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return &FrameResult{
		PNG:       buf.Bytes(),
		SHA256Hex: hex.EncodeToString(sum[:]),
		Width:     w,
		Height:    h,
	}, nil
}

func formatExhaustLabel(updatedAt, exhaustAt time.Time, loc *time.Location) string {
	u := updatedAt.In(loc)
	e := exhaustAt.In(loc)
	if u.Year() == e.Year() && u.Month() == e.Month() && u.Day() == e.Day() {
		return "0% ~" + e.Format("15:04")
	}
	return "0% ~" + e.Format("01/02 15:04")
}

func buildSeriesPoints(p predictor.PoolPrediction, gx0, nowX, gx1, gy0, gy1 int, now time.Time) ([]Pt, []Pt) {
	if !p.Available {
		return nil, nil
	}
	yOf := func(pct float64) float64 {
		return float64(gy1) - (clampF(pct, 0, 100)/100.0)*float64(gy1-gy0)
	}
	histSpanH := 6.0
	predSpanH := 6.0

	var hist []Pt
	if len(p.History) >= 2 {
		for _, pt := range p.History {
			relH := now.Sub(pt.Time).Hours()
			if relH < 0 || relH > histSpanH {
				continue
			}
			x := float64(nowX) - (relH/histSpanH)*float64(nowX-gx0)
			hist = append(hist, Pt{X: x, Y: yOf(pt.Pct)})
		}
	}
	// Never fabricate an unseen 6-hour history curve if we only have a single point:
	// just anchor the NOW node so only observed history is drawn.
	if len(hist) == 0 {
		hist = []Pt{{X: float64(nowX), Y: yOf(p.RemainingPct)}}
	}

	var pred []Pt
	yNow := yOf(p.RemainingPct)
	pred = append(pred, Pt{X: float64(nowX), Y: yNow})
	if p.BurnRatePctH > 0.05 {
		hoursToZero := p.RemainingPct / p.BurnRatePctH
		if hoursToZero <= predSpanH {
			xEnd := float64(nowX) + (hoursToZero/predSpanH)*float64(gx1-nowX)
			pred = append(pred, Pt{X: xEnd, Y: float64(gy1)})
		} else {
			pctEnd := clampF(p.RemainingPct-p.BurnRatePctH*predSpanH, 0, 100)
			pred = append(pred, Pt{X: float64(gx1), Y: yOf(pctEnd)})
		}
	} else {
		pred = append(pred, Pt{X: float64(gx1), Y: yNow})
	}
	return hist, pred
}

func interpY(pts []Pt, x float64) float64 {
	if x <= pts[0].X {
		return pts[0].Y
	}
	for i := 1; i < len(pts); i++ {
		if x <= pts[i].X {
			dx := pts[i].X - pts[i-1].X
			if dx < 1e-6 {
				return pts[i].Y
			}
			t := (x - pts[i-1].X) / dx
			return pts[i-1].Y + t*(pts[i].Y-pts[i-1].Y)
		}
	}
	return pts[len(pts)-1].Y
}

func formatPctInt(pct float64, ok bool) string {
	if !ok {
		return "--%"
	}
	return fmt.Sprintf("%d%%", int(math.Round(clampF(pct, 0, 100))))
}

func formatBurn(rate float64) string {
	if rate <= 0.05 {
		return "0.0%/h"
	}
	return fmt.Sprintf("-%.1f%%/h", rate)
}

func formatResetClock(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return "--:--"
	}
	return t.In(loc).Format("15:04")
}
