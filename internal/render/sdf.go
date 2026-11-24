package render

import "math"

type Pt struct {
	X float64
	Y float64
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func quantize16Gray(v float64) uint8 {
	c := clampF(v, 0, 255)
	q := math.Round(c/17.0) * 17.0
	return uint8(clampF(q, 0, 255))
}

func catmullRomChain(pts []Pt, stepsPerSeg int) []Pt {
	if len(pts) < 2 {
		return pts
	}
	ext := make([]Pt, 0, len(pts)+2)
	ext = append(ext, pts[0])
	ext = append(ext, pts...)
	ext = append(ext, pts[len(pts)-1])

	out := make([]Pt, 0, (len(pts)-1)*stepsPerSeg+1)
	for i := 1; i < len(ext)-2; i++ {
		p0, p1, p2, p3 := ext[i-1], ext[i], ext[i+1], ext[i+2]
		for s := 0; s < stepsPerSeg; s++ {
			t := float64(s) / float64(stepsPerSeg)
			t2 := t * t
			t3 := t2 * t
			x := 0.5 * ((2 * p1.X) + (-p0.X+p2.X)*t + (2*p0.X-5*p1.X+4*p2.X-p3.X)*t2 + (-p0.X+3*p1.X-3*p2.X+p3.X)*t3)
			y := 0.5 * ((2 * p1.Y) + (-p0.Y+p2.Y)*t + (2*p0.Y-5*p1.Y+4*p2.Y-p3.Y)*t2 + (-p0.Y+3*p1.Y-3*p2.Y+p3.Y)*t3)
			out = append(out, Pt{X: x, Y: y})
		}
	}
	out = append(out, pts[len(pts)-1])
	return out
}

func blendSDFCurve(canvas []float64, w, h int, pts []Pt, thickness, color, aaWidth float64) {
	if len(pts) < 2 {
		return
	}
	minX, maxX := pts[0].X, pts[0].X
	minY, maxY := pts[0].Y, pts[0].Y
	for _, p := range pts[1:] {
		if p.X < minX {
			minX = p.X
		}
		if p.X > maxX {
			maxX = p.X
		}
		if p.Y < minY {
			minY = p.Y
		}
		if p.Y > maxY {
			maxY = p.Y
		}
	}
	pad := int(math.Ceil(thickness + 3))
	x0 := max(0, int(math.Floor(minX))-pad)
	x1 := min(w, int(math.Ceil(maxX))+pad)
	y0 := max(0, int(math.Floor(minY))-pad)
	y1 := min(h, int(math.Ceil(maxY))+pad)
	halfW := thickness / 2.0

	for py := y0; py < y1; py++ {
		fy := float64(py)
		for px := x0; px < x1; px++ {
			fx := float64(px)
			minD2 := 1e12
			for i := 0; i < len(pts)-1; i++ {
				ax, ay := pts[i].X, pts[i].Y
				bx, by := pts[i+1].X, pts[i+1].Y
				dx, dy := bx-ax, by-ay
				l2 := dx*dx + dy*dy
				if l2 < 1e-8 {
					continue
				}
				t := clampF(((fx-ax)*dx+(fy-ay)*dy)/l2, 0, 1)
				qx := ax + t*dx
				qy := ay + t*dy
				d2 := (fx-qx)*(fx-qx) + (fy-qy)*(fy-qy)
				if d2 < minD2 {
					minD2 = d2
				}
			}
			dist := math.Sqrt(minD2)
			alpha := clampF((halfW-dist)/aaWidth+0.5, 0, 1)
			if alpha > 0 {
				idx := py*w + px
				canvas[idx] = canvas[idx]*(1-alpha) + color*alpha
			}
		}
	}
}

func splitDashedSegments(pts []Pt, dashLen, gapLen float64) [][]Pt {
	if len(pts) < 2 {
		return nil
	}
	var dashes [][]Pt
	curSeg := []Pt{pts[0]}
	acc := 0.0
	drawing := true
	for i := 1; i < len(pts); i++ {
		x0, y0 := pts[i-1].X, pts[i-1].Y
		x1, y1 := pts[i].X, pts[i].Y
		d := math.Hypot(x1-x0, y1-y0)
		if d < 1e-6 {
			continue
		}
		rem := d
		cx, cy := x0, y0
		ux, uy := (x1-x0)/d, (y1-y0)/d
		for rem > 1e-6 {
			limit := gapLen
			if drawing {
				limit = dashLen
			}
			step := math.Min(rem, limit-acc)
			nx, ny := cx+ux*step, cy+uy*step
			if drawing {
				if len(curSeg) == 0 {
					curSeg = append(curSeg, Pt{X: cx, Y: cy})
				}
				curSeg = append(curSeg, Pt{X: nx, Y: ny})
			}
			acc += step
			rem -= step
			cx, cy = nx, ny
			if acc >= limit-1e-5 {
				if drawing && len(curSeg) >= 2 {
					dashes = append(dashes, curSeg)
				}
				curSeg = nil
				acc = 0
				drawing = !drawing
			}
		}
	}
	if drawing && len(curSeg) >= 2 {
		dashes = append(dashes, curSeg)
	}
	return dashes
}

func blendSDFCircle(canvas []float64, w, h int, cx, cy, rOuter, rInner, fillColor, strokeColor, aaWidth float64) {
	pad := int(math.Ceil(rOuter + 3))
	x0 := max(0, int(math.Floor(cx))-pad)
	x1 := min(w, int(math.Ceil(cx))+pad+1)
	y0 := max(0, int(math.Floor(cy))-pad)
	y1 := min(h, int(math.Ceil(cy))+pad+1)
	for py := y0; py < y1; py++ {
		for px := x0; px < x1; px++ {
			dist := math.Hypot(float64(px)-cx, float64(py)-cy)
			idx := py*w + px
			v := canvas[idx]
			aOut := clampF((rOuter-dist)/aaWidth+0.5, 0, 1)
			v = v*(1-aOut) + strokeColor*aOut
			if rInner > 0 {
				aIn := clampF((rInner-dist)/aaWidth+0.5, 0, 1)
				v = v*(1-aIn) + fillColor*aIn
			}
			canvas[idx] = v
		}
	}
}

func blendSDFRoundedRect(canvas []float64, w, h int, x, y, rw, rh, radius, color float64) {
	x0 := max(0, int(math.Floor(x))-2)
	x1 := min(w, int(math.Ceil(x+rw))+2)
	y0 := max(0, int(math.Floor(y))-2)
	y1 := min(h, int(math.Ceil(y+rh))+2)
	cx := x + rw/2.0
	cy := y + rh/2.0
	hx := rw/2.0 - radius
	hy := rh/2.0 - radius
	for py := y0; py < y1; py++ {
		for px := x0; px < x1; px++ {
			qx := math.Abs(float64(px)-cx) - hx
			qy := math.Abs(float64(py)-cy) - hy
			dist := math.Hypot(math.Max(qx, 0), math.Max(qy, 0)) + math.Min(math.Max(qx, qy), 0) - radius
			alpha := clampF(0.5-dist/0.85, 0, 1)
			if alpha > 0 {
				idx := py*w + px
				canvas[idx] = canvas[idx]*(1-alpha) + color*alpha
			}
		}
	}
}

func blendPillBar(canvas []float64, w, h int, x, y, bw, bh, pct, bgColor, fgColor float64) {
	r := bh / 2.0
	blendSDFRoundedRect(canvas, w, h, x, y, bw, bh, r, bgColor)
	pct = clampF(pct, 0, 1)
	if pct > 0.01 {
		fw := math.Max(bh, bw*pct)
		blendSDFRoundedRect(canvas, w, h, x, y, fw, bh, r, fgColor)
	}
}
