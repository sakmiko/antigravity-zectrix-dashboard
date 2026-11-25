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

type pillLayout struct {
	label     string
	x         int
	y         int
	w         int
	h         int
	textColor uint8
	textX     int
	textY     int
}

// RenderDashboard renders Option B: Minimalist Big-Card 400x300 16-level grayscale PNG
// tailored for ZECTRIX NOTE4 e-paper displays—large high-contrast quotas, reset countdowns,
// subscription tier badges, and a clean account/keepalive footer without speculative charts.
func RenderDashboard(fc *predictor.Forecast, loc *time.Location) (*FrameResult, error) {
	if loc == nil {
		loc = time.UTC
	}
	w, h := DisplayWidth, DisplayHeight
	canvas := make([]float64, w*h)
	for i := range canvas {
		canvas[i] = 255.0
	}

	// 1. Header: ANTIGRAVITY + Subscription Tier Badge + Status Dot + Clock
	titleEndX := 12 + measureGridFont("ANTIGRAVITY", Mono8x11Bold, 2)
	var tierBadge *pillLayout
	if tier := strings.ToUpper(strings.TrimSpace(fc.PlanTier)); tier != "" {
		tw := measureGridFont(tier, Mono7x10Bold, 1)
		bx := titleEndX + 10
		by := 6
		bw := tw + 12
		bh := 15
		textColor := uint8(0)
		switch tier {
		case "ULTRA":
			blendSDFRoundedRectBorder(canvas, w, h, float64(bx)-2, float64(by)-2, float64(bw)+4, float64(bh)+4, 4.5, 1.2, 255, 0)
			blendSDFRoundedRect(canvas, w, h, float64(bx), float64(by), float64(bw), float64(bh), 3.0, 0)
			textColor = 255
		case "PRO":
			blendSDFRoundedRect(canvas, w, h, float64(bx), float64(by), float64(bw), float64(bh), 3.5, 17)
			textColor = 255
		case "PLUS":
			blendSDFRoundedRectBorder(canvas, w, h, float64(bx), float64(by), float64(bw), float64(bh), 3.5, 1.5, 238, 0)
			textColor = 0
		case "FREE":
			blendSDFRoundedRectBorder(canvas, w, h, float64(bx), float64(by), float64(bw), float64(bh), 3.5, 1.0, 238, 119)
			textColor = 0
		default:
			blendSDFRoundedRectBorder(canvas, w, h, float64(bx), float64(by), float64(bw), float64(bh), 3.5, 1.2, 238, 85)
			textColor = 0
		}
		tierBadge = &pillLayout{
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

	if fc.Stale {
		blendSDFCircle(canvas, w, h, 329.5, 13.5, 3.6, 1.8, 255, 0, 0.75)
	} else {
		blendSDFCircle(canvas, w, h, 329.5, 13.5, 3.5, 0, 0, 0, 0.75)
	}

	// 2. Left Inverted Dark-Ink Bottleneck Hero Card (x=12..148, y=36..252, h=216)
	blendSDFRoundedRect(canvas, w, h, 12, 36, 136, 216, 6.0, 17.0)
	for px := 22; px <= 138; px++ {
		canvas[59*w+px] = 68.0
		canvas[165*w+px] = 68.0
	}
	bn := fc.Bottleneck
	blendPillBar(canvas, w, h, 22, 141, 116, 10, bn.RemainingPct/100.0, 68, 255)

	// 3. Right Spacious Dual-Pool Cards (y=36..252)
	// Top Pool: GEMINI (y=36..139)
	blendSDFRoundedRect(canvas, w, h, 162, 41, 4, 11, 1.5, 68)
	blendPillBar(canvas, w, h, 162, 116, 226, 11, fc.Gemini.RemainingPct/100.0, 221, 68)

	// Middle divider between Gemini & Claude
	for px := 162; px <= 388; px++ {
		canvas[144*w+px] = 170.0
	}

	// Bottom Pool: CLAUDE (y=150..252)
	blendSDFRoundedRect(canvas, w, h, 162, 155, 4, 11, 1.5, 0)
	blendPillBar(canvas, w, h, 162, 230, 226, 11, fc.Claude.RemainingPct/100.0, 221, 0)

	// 4. Crisp Structural Rules (Header & Footer)
	for px := 12; px <= 388; px++ {
		canvas[27*w+px] = 0
		canvas[28*w+px] = 0
		canvas[262*w+px] = 102
	}

	// 5. Footer Keepalive Status Pill
	kaText := "KA OFF"
	if fc.KeepaliveEnabled {
		kaText = "KA " + formatCompactInterval(fc.KeepaliveIntervalSeconds)
	}
	rightText := formatFooterRight(fc)
	rightW := measureGridFont(rightText, Mono7x10Bold, 1)
	kaW := measureGridFont(kaText, Mono7x10Bold, 1) + 12
	kaX := 388 - rightW - 10 - kaW
	kaPill := pillLayout{
		label: kaText,
		x:     kaX,
		y:     272,
		w:     kaW,
		h:     16,
		textX: kaX + 6,
		textY: 275,
	}
	if fc.KeepaliveEnabled {
		blendSDFRoundedRect(canvas, w, h, float64(kaPill.x), float64(kaPill.y), float64(kaPill.w), float64(kaPill.h), 3.5, 17)
		kaPill.textColor = 255
	} else {
		blendSDFRoundedRectBorder(canvas, w, h, float64(kaPill.x), float64(kaPill.y), float64(kaPill.w), float64(kaPill.h), 3.5, 1.0, 238, 136)
		kaPill.textColor = 51
	}

	// 6. Quantize graphic layer to 16-level e-ink grayscale
	frame := make([]uint8, w*h)
	for i, v := range canvas {
		frame[i] = quantize16Gray(v)
	}

	// 7. Composite 100% pure-ink MonoGrid Bold pixel typography
	drawGridFont(frame, w, h, 12, 8, "ANTIGRAVITY", Mono8x11Bold, 0, 2)
	if tierBadge != nil {
		drawGridFont(frame, w, h, tierBadge.textX, tierBadge.textY, tierBadge.label, Mono7x10Bold, tierBadge.textColor, 1)
	}
	syncStr := fc.UpdatedAt.In(loc).Format("15:04")
	drawGridFontRight(frame, w, h, 388, 8, syncStr, Mono8x11Bold, 0, 2)

	// Left Bottleneck Hero Card Typography
	drawGridFont(frame, w, h, 22, 44, "LOWEST", Mono7x10Bold, 255, 2)
	drawGridFontRight(frame, w, h, 138, 44, "FOCUS", Mono7x10Bold, 187, 1)
	drawGridFont(frame, w, h, 22, 74, formatPctInt(bn.RemainingPct, bn.Available), HeroDigits20x30, 255, 2)
	bnName := bn.Name
	if bnName == "" {
		bnName = "POOL"
	}
	drawGridFont(frame, w, h, 22, 118, bnName, Mono8x11Bold, 255, 2)

	drawGridFont(frame, w, h, 22, 176, "RESET IN", Mono7x10Bold, 187, 1)
	drawGridFont(frame, w, h, 22, 193, formatResetCountdown(bn.ResetAt, fc.UpdatedAt, bn.Available), Mono8x11Bold, 255, 2)
	drawGridFont(frame, w, h, 22, 213, "RST "+formatResetClock(bn.ResetAt, loc), Mono7x10Bold, 204, 1)
	drawGridFont(frame, w, h, 22, 230, formatPoolStatus(bn), Mono7x10Bold, 187, 1)

	// Right Row 1: GEMINI
	drawGridFont(frame, w, h, 171, 41, "GEMINI", Mono8x11Bold, 0, 2)
	drawGridFontRight(frame, w, h, 388, 42, "RST "+formatResetClock(fc.Gemini.ResetAt, loc), Mono7x10Bold, 68, 1)
	drawGridFont(frame, w, h, 162, 68, formatPctInt(fc.Gemini.RemainingPct, fc.Gemini.Available), HeroDigits20x30, 0, 2)
	drawGridFontRight(frame, w, h, 388, 69, formatResetCountdown(fc.Gemini.ResetAt, fc.UpdatedAt, fc.Gemini.Available), Mono8x11Bold, 0, 1)
	drawGridFontRight(frame, w, h, 388, 88, formatPoolStatus(fc.Gemini), Mono7x10Bold, 68, 1)

	// Right Row 2: CLAUDE
	drawGridFont(frame, w, h, 171, 155, "CLAUDE", Mono8x11Bold, 0, 2)
	drawGridFontRight(frame, w, h, 388, 156, "RST "+formatResetClock(fc.Claude.ResetAt, loc), Mono7x10Bold, 68, 1)
	drawGridFont(frame, w, h, 162, 182, formatPctInt(fc.Claude.RemainingPct, fc.Claude.Available), HeroDigits20x30, 0, 2)
	drawGridFontRight(frame, w, h, 388, 183, formatResetCountdown(fc.Claude.ResetAt, fc.UpdatedAt, fc.Claude.Available), Mono8x11Bold, 0, 1)
	drawGridFontRight(frame, w, h, 388, 202, formatPoolStatus(fc.Claude), Mono7x10Bold, 68, 1)

	// Footer: Account Email (clipped safely before Keepalive pill) + KA Pill + Weekly/Poll
	acctLabel := formatAccountLabel(fc.AccountEmail, kaPill.x-22)
	drawGridFont(frame, w, h, 12, 275, acctLabel, Mono7x10Bold, 34, 1)
	drawGridFont(frame, w, h, kaPill.textX, kaPill.textY, kaPill.label, Mono7x10Bold, kaPill.textColor, 1)
	drawGridFontRight(frame, w, h, 388, 275, rightText, Mono7x10Bold, 0, 1)

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

func formatPctInt(pct float64, ok bool) string {
	if !ok {
		return "--%"
	}
	return fmt.Sprintf("%d%%", int(math.Round(clampF(pct, 0, 100))))
}

func formatResetClock(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return "--:--"
	}
	return t.In(loc).Format("15:04")
}

func formatResetCountdown(resetAt, now time.Time, ok bool) string {
	if !ok || resetAt.IsZero() {
		return "--"
	}
	d := resetAt.Sub(now)
	if d <= 0 {
		return "SOON"
	}
	return predictor.FormatDurationCompact(d)
}

func formatPoolStatus(p predictor.PoolPrediction) string {
	if !p.Available {
		return "UNAVAILABLE"
	}
	if p.RemainingPct <= 0.5 {
		return "EXHAUSTED"
	}
	if p.BurnRatePctH > 0.05 {
		return fmt.Sprintf("-%.1f%%/h", p.BurnRatePctH)
	}
	if p.RemainingPct >= 99.5 {
		return "FULL"
	}
	return "READY"
}

func formatCompactInterval(sec int) string {
	if sec <= 0 {
		return "ON"
	}
	if sec%3600 == 0 {
		return fmt.Sprintf("%dH", sec/3600)
	}
	if sec >= 60 {
		return fmt.Sprintf("%dM", sec/60)
	}
	return fmt.Sprintf("%dS", sec)
}

func formatFooterRight(fc *predictor.Forecast) string {
	if fc.Weekly.Available {
		return fmt.Sprintf("WK %d%%", int(math.Round(clampF(fc.Weekly.RemainingPct, 0, 100))))
	}
	if fc.PollIntervalSeconds > 0 {
		return "POLL " + formatCompactInterval(fc.PollIntervalSeconds)
	}
	return "LIVE"
}

func formatAccountLabel(email string, maxPx int) string {
	s := strings.ToUpper(strings.TrimSpace(email))
	if s == "" {
		s = "CPA ACCOUNT"
	}
	if measureGridFont(s, Mono7x10Bold, 1) <= maxPx {
		return s
	}
	// Truncate cleanly with "..."
	runes := []rune(s)
	for len(runes) > 3 {
		runes = runes[:len(runes)-1]
		cand := string(runes) + "..."
		if measureGridFont(cand, Mono7x10Bold, 1) <= maxPx {
			return cand
		}
	}
	return s
}
