package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/png"
	"math"
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

// RenderDashboard renders the approved Variant 1 (Inverted Focus Panel) 400x300
// 16-level grayscale PNG using MonoGrid Bold bitmap fonts and analytical SDF curves.
func RenderDashboard(fc *predictor.Forecast, loc *time.Location) (*FrameResult, error) {
	if loc == nil {
		loc = time.UTC
	}
	w, h := DisplayWidth, DisplayHeight
	canvas := make([]float64, w*h)
	for i := range canvas {
		canvas[i] = 255.0
	}

	// 1. Top status dot (filled black if fresh, hollow ring if stale)
	if fc.Stale {
		blendSDFCircle(canvas, w, h, 329.5, 13.5, 3.6, 1.8, 255, 0, 0.75)
	} else {
		blendSDFCircle(canvas, w, h, 329.5, 13.5, 3.5, 0, 0, 0, 0.75)
	}

	// 2. Inverted dark-ink bottleneck left focus panel (x=12..148, y=34..162)
	for py := 34; py < 162; py++ {
		for px := 12; px < 148; px++ {
			canvas[py*w+px] = 17.0
		}
	}
	bn := fc.Bottleneck
	blendPillBar(canvas, w, h, 20, 130, 120, 7, bn.RemainingPct/100.0, 85, 255)

	// Right dual-pool pill bars
	blendPillBar(canvas, w, h, 164, 85, 224, 6, fc.Gemini.RemainingPct/100.0, 204, 34)
	blendPillBar(canvas, w, h, 164, 152, 224, 6, fc.Claude.RemainingPct/100.0, 204, 0)

	// Bottom weekly pill bar
	blendPillBar(canvas, w, h, 80, 281, 184, 7, fc.Weekly.RemainingPct/100.0, 204, 34)

	// 3. Pixel-snapped crisp structural rules
	for px := 12; px <= 388; px++ {
		canvas[27*w+px] = 0
		canvas[28*w+px] = 0
		canvas[168*w+px] = 102
		canvas[270*w+px] = 170
	}
	for px := 164; px <= 388; px++ {
		canvas[99*w+px] = 187
	}

	// 4. Lower Forecast Chart (x=44..388, y=192..260)
	gx0, gy0, gx1, gy1 := 44, 192, 388, 260
	ymid := (gy0 + gy1) / 2
	nowX := 216

	gemHist, gemPred := buildSeriesPoints(fc.Gemini, gx0, nowX, gx1, gy0, gy1, fc.UpdatedAt)
	claHist, claPred := buildSeriesPoints(fc.Claude, gx0, nowX, gx1, gy0, gy1, fc.UpdatedAt)

	// Light-gray area fill under primary (Claude) actual history curve
	if len(claHist) >= 2 {
		for px := gx0; px <= nowX; px++ {
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

	// 5. Quantize graphic layer to 16-level e-ink grayscale
	frame := make([]uint8, w*h)
	for i, v := range canvas {
		frame[i] = quantize16Gray(v)
	}

	// 6. Composite 100% pure-ink (0 or 255) MonoGrid Bold pixel typography
	drawGridFont(frame, w, h, 12, 8, "ANTIGRAVITY", Mono8x11Bold, 0, 2)
	syncStr := fc.UpdatedAt.In(loc).Format("15:04")
	drawGridFontRight(frame, w, h, 388, 8, syncStr, Mono8x11Bold, 0, 2)

	// Left Bottleneck Panel
	drawGridFont(frame, w, h, 20, 40, "RUNWAY", Mono7x10Bold, 255, 2)
	drawGridFont(frame, w, h, 20, 56, predictor.FormatRunwayShort(bn.RunwayHours), HeroDigits20x30, 255, 3)
	bnName := bn.Name
	if bnName == "" {
		bnName = "POOL"
	}
	drawGridFont(frame, w, h, 20, 95, bnName, Mono8x11Bold, 255, 2)
	drawGridFont(frame, w, h, 20, 112, formatBurn(bn.BurnRatePctH), Mono7x10Bold, 255, 1)
	drawGridFont(frame, w, h, 20, 145, formatPctInt(bn.RemainingPct, bn.Available), Mono7x10Bold, 255, 1)
	drawGridFontRight(frame, w, h, 140, 145, formatResetClock(bn.ResetAt, loc), Mono7x10Bold, 255, 1)

	// Right Row 1: GEMINI
	drawGridFont(frame, w, h, 164, 37, "GEMINI", Mono8x11Bold, 0, 2)
	drawGridFontRight(frame, w, h, 388, 38, "RST "+formatResetClock(fc.Gemini.ResetAt, loc), Mono7x10Bold, 0, 1)
	drawGridFont(frame, w, h, 164, 55, formatPctInt(fc.Gemini.RemainingPct, fc.Gemini.Available), MedDigits15x22, 0, 3)
	drawGridFontRight(frame, w, h, 388, 54, "~"+predictor.FormatRunwayShort(fc.Gemini.RunwayHours), Mono8x11Bold, 0, 1)
	drawGridFontRight(frame, w, h, 388, 69, formatBurn(fc.Gemini.BurnRatePctH), Mono7x10Bold, 0, 1)

	// Right Row 2: CLAUDE
	drawGridFont(frame, w, h, 164, 105, "CLAUDE", Mono8x11Bold, 0, 2)
	drawGridFontRight(frame, w, h, 388, 106, "RST "+formatResetClock(fc.Claude.ResetAt, loc), Mono7x10Bold, 0, 1)
	drawGridFont(frame, w, h, 164, 123, formatPctInt(fc.Claude.RemainingPct, fc.Claude.Available), MedDigits15x22, 0, 3)
	drawGridFontRight(frame, w, h, 388, 122, "~"+predictor.FormatRunwayShort(fc.Claude.RunwayHours), Mono8x11Bold, 0, 1)
	drawGridFontRight(frame, w, h, 388, 137, formatBurn(fc.Claude.BurnRatePctH), Mono7x10Bold, 0, 1)

	// Forecast header & axis ticks
	drawGridFont(frame, w, h, 12, 175, "FORECAST", Mono7x10Bold, 0, 2)
	drawGridFont(frame, w, h, 203, 175, "NOW", Mono7x10Bold, 0, 2)
	exhaustLabel := "STABLE"
	if bn.RunwayHours >= 0 && !bn.ExhaustAt.IsZero() {
		exhaustLabel = "0% ~" + bn.ExhaustAt.In(loc).Format("15:04")
	}
	drawGridFontRight(frame, w, h, 388, 175, exhaustLabel, Mono7x10Bold, 0, 1)

	drawGridFontRight(frame, w, h, 38, 188, "100", Mono7x10Bold, 0, 1)
	drawGridFontRight(frame, w, h, 38, 221, "50", Mono7x10Bold, 0, 1)
	drawGridFontRight(frame, w, h, 38, 255, "0", Mono7x10Bold, 0, 1)

	// Bottom Weekly Strip (fixed full-height '%' in Mono8x11Bold!)
	drawGridFont(frame, w, h, 12, 278, "WEEKLY", Mono8x11Bold, 0, 2)
	drawGridFont(frame, w, h, 274, 278, formatPctInt(fc.Weekly.RemainingPct, fc.Weekly.Available), Mono8x11Bold, 0, 1)
	wkRem := "--"
	if !fc.Weekly.ResetAt.IsZero() {
		wkRem = predictor.FormatDurationCompact(fc.Weekly.ResetAt.Sub(fc.UpdatedAt))
	}
	drawGridFontRight(frame, w, h, 388, 279, wkRem, Mono7x10Bold, 0, 1)

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
	if len(hist) < 2 {
		yNow := yOf(p.RemainingPct)
		yStart := yOf(clampF(p.RemainingPct+p.BurnRatePctH*histSpanH, 0, 100))
		hist = []Pt{{X: float64(gx0), Y: yStart}, {X: float64(nowX), Y: yNow}}
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
