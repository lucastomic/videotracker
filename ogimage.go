package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log"
	"os"
	"strings"
	"sync"
	"unicode"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

const (
	ogWidth    = 1200
	ogHeight   = 630
	ogMaxBytes = 300 * 1024
)

var (
	fontsOnce   sync.Once
	fontBold    *opentype.Font
	fontRegular *opentype.Font

	solnowLogoOnce sync.Once
	solnowLogo     image.Image
)

func loadFonts() {
	fontsOnce.Do(func() {
		var err error
		if fontBold, err = opentype.Parse(gobold.TTF); err != nil {
			log.Fatal(err)
		}
		if fontRegular, err = opentype.Parse(goregular.TTF); err != nil {
			log.Fatal(err)
		}
	})
}

func getSolnowLogo() image.Image {
	solnowLogoOnce.Do(func() {
		f, err := os.Open("static/images/logo.png")
		if err != nil {
			log.Printf("WARNING: no se pudo abrir el logo de SolNow: %v", err)
			return
		}
		defer f.Close()
		if solnowLogo, _, err = image.Decode(f); err != nil {
			log.Printf("WARNING: no se pudo decodificar el logo de SolNow: %v", err)
		}
	})
	return solnowLogo
}

func face(f *opentype.Font, size float64) font.Face {
	fc, err := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		log.Fatal(err)
	}
	return fc
}

// OGParams describes one thumbnail. Empty CompanyName => generic SolNow card.
type OGParams struct {
	CompanyName string
	BrandColor  string
	Logo        image.Image
}

// renderOGImage composes the 1200x630 WhatsApp/Open Graph preview.
func renderOGImage(p OGParams) *image.RGBA {
	loadFonts()
	generic := strings.TrimSpace(p.CompanyName) == ""
	brand, ok := parseColor(p.BrandColor)
	if !ok || generic {
		brand, _ = parseColor(defaultBrandColor)
	}
	light := luminance(brand) > 0.4
	textColor := color.RGBA{255, 255, 255, 255}
	// NRGBA: translucent colors must not be passed as (premultiplied) RGBA.
	subColor := color.NRGBA{255, 255, 255, 220}
	if light {
		textColor = color.RGBA{0x0B, 0x25, 0x45, 255}
		subColor = color.NRGBA{0x0B, 0x25, 0x45, 220}
	}

	img := image.NewRGBA(image.Rect(0, 0, ogWidth, ogHeight))

	// Background: soft diagonal gradient from the brand color to a darker shade.
	end := mix(brand, color.RGBA{0, 0, 0, 255}, 0.30)
	start := mix(brand, color.RGBA{255, 255, 255, 255}, 0.08)
	if light {
		end = mix(brand, color.RGBA{0, 0, 0, 255}, 0.12)
		start = brand
	}
	for y := 0; y < ogHeight; y++ {
		for x := 0; x < ogWidth; x++ {
			t := (float64(x)/ogWidth*0.6 + float64(y)/ogHeight*0.4)
			img.SetRGBA(x, y, mix(start, end, t))
		}
	}
	// Decorative circle, very subtle.
	fillPath(img, color.RGBA{255, 255, 255, 14}, circlePath(1080, 90, 260))

	// Logo box (white, rounded) with a soft shadow.
	boxX, boxY, boxS := 80.0, 155.0, 320.0
	fillPath(img, color.RGBA{0, 0, 0, 40}, roundRectPath(boxX+4, boxY+10, boxS, boxS, 40))
	logo := p.Logo
	if generic {
		logo = getSolnowLogo()
	}
	boxFill := color.RGBA{255, 255, 255, 255}
	if logo != nil && !generic && logoIsLight(logo) {
		boxFill = color.RGBA{0x0B, 0x25, 0x45, 255}
		if !light {
			boxFill = mix(brand, color.RGBA{0, 0, 0, 255}, 0.55)
		}
	}
	fillPath(img, boxFill, roundRectPath(boxX, boxY, boxS, boxS, 40))
	if logo != nil {
		pad := 40.0
		drawFitted(img, logo, image.Rect(int(boxX+pad), int(boxY+pad), int(boxX+boxS-pad), int(boxY+boxS-pad)))
	} else {
		// No logo: initials in the brand color.
		initials := companyInitials(p.CompanyName)
		fc := face(fontBold, 130)
		w := font.MeasureString(fc, initials).Round()
		m := fc.Metrics()
		ink := brand
		if light {
			ink = color.RGBA{0x0B, 0x25, 0x45, 255}
		}
		drawText(img, fc, ink, int(boxX+boxS/2)-w/2, int(boxY+boxS/2)+(m.Ascent.Round()-m.Descent.Round())/2, initials)
	}

	// Text block.
	textX := 456
	textW := ogWidth - textX - 70
	title, subtitle := p.CompanyName, "Tu demo personalizada de SolNow"
	if generic {
		title, subtitle = "Tu demo de SolNow", "Reservas, pagos y clientes en un solo sitio"
	}
	title = strings.Join(strings.Fields(title), " ")
	titleSize, titleLines := fitTitle(title, textW)
	titleFace := face(fontBold, titleSize)
	subSize := 36.0
	subFace := face(fontRegular, subSize)
	for subSize > 24 && font.MeasureString(subFace, subtitle).Round() > textW {
		subSize -= 2
		subFace = face(fontRegular, subSize)
	}
	lineH := int(titleSize * 1.12)
	playR := 46
	blockH := lineH*len(titleLines) + 18 + int(subSize) + 48 + playR*2
	y := ogHeight/2 - blockH/2 - 10
	for _, line := range titleLines {
		y += lineH
		drawText(img, titleFace, textColor, textX, y-int(titleSize*0.15), line)
	}
	y += 18 + int(subSize)
	drawText(img, subFace, subColor, textX, y, subtitle)

	// Play button (circle + triangle) so it reads as a video.
	cy := float64(y + 48 + playR)
	cx := float64(textX + playR)
	fillPath(img, color.RGBA{0, 0, 0, 45}, circlePath(cx+2, cy+5, float64(playR)))
	fillPath(img, textColor, circlePath(cx, cy, float64(playR)))
	triInk := brand
	if light {
		triInk = color.RGBA{255, 255, 255, 255}
	}
	if luminance(triInk) > 0.8 && !light {
		triInk = color.RGBA{0x0B, 0x25, 0x45, 255}
	}
	r := float32(playR)
	fillPath(img, triInk, func(z *vector.Rasterizer) {
		z.MoveTo(float32(cx)-r*0.30, float32(cy)-r*0.42)
		z.LineTo(float32(cx)+r*0.48, float32(cy))
		z.LineTo(float32(cx)-r*0.30, float32(cy)+r*0.42)
		z.ClosePath()
	})
	playFace := face(fontBold, 30)
	drawText(img, playFace, textColor, textX+playR*2+22, int(cy)+11, "Ver la demo")

	// SolNow logo, small, bottom-right, on a white pill so it always contrasts.
	if sl := getSolnowLogo(); sl != nil && !generic {
		lh := 30
		lw := lh * sl.Bounds().Dx() / max(1, sl.Bounds().Dy())
		pw, ph := lw+32, lh+22
		px, py := ogWidth-36-pw, ogHeight-30-ph
		fillPath(img, color.RGBA{255, 255, 255, 240}, roundRectPath(float64(px), float64(py), float64(pw), float64(ph), float64(ph)/2))
		drawFitted(img, sl, image.Rect(px+16, py+11, px+16+lw, py+11+lh))
	}
	return img
}

// fitTitle picks the largest size (84→44) where the title fits in 1 line,
// else 2 lines; as last resort truncates with "…".
func fitTitle(title string, width int) (float64, []string) {
	for size := 84.0; size >= 52; size -= 4 {
		if font.MeasureString(face(fontBold, size), title).Round() <= width {
			return size, []string{title}
		}
	}
	for size := 60.0; size >= 44; size -= 4 {
		lines, ok := wrapText(face(fontBold, size), title, width, 2)
		if ok {
			return size, lines
		}
	}
	lines, _ := wrapText(face(fontBold, 44), title, width, 2)
	return 44, lines
}

// wrapText wraps words into at most maxLines; ok=false if it had to truncate.
func wrapText(fc font.Face, text string, width, maxLines int) ([]string, bool) {
	fits := func(s string) bool { return font.MeasureString(fc, s).Round() <= width }
	var lines []string
	cur := ""
	for _, w := range strings.Fields(text) {
		if cur == "" {
			cur = w
		} else if fits(cur + " " + w) {
			cur += " " + w
		} else {
			lines = append(lines, cur)
			cur = w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	ok := len(lines) <= maxLines
	if !ok {
		last := strings.Join(lines[maxLines-1:], " ")
		lines = append(lines[:maxLines-1], last)
	}
	for i, l := range lines {
		if !fits(l) {
			lines[i] = ellipsize(fc, l, width)
			ok = false
		}
	}
	return lines, ok
}

func ellipsize(fc font.Face, s string, width int) string {
	r := []rune(s)
	for len(r) > 0 && font.MeasureString(fc, string(r)+"…").Round() > width {
		r = r[:len(r)-1]
	}
	return strings.TrimRightFunc(string(r), unicode.IsSpace) + "…"
}

func companyInitials(name string) string {
	var out []rune
	for _, w := range strings.Fields(name) {
		r := []rune(w)
		if len(r) > 0 && (unicode.IsLetter(r[0]) || unicode.IsDigit(r[0])) {
			out = append(out, unicode.ToUpper(r[0]))
		}
		if len(out) == 2 {
			break
		}
	}
	if len(out) == 0 {
		return "?"
	}
	return string(out)
}

func drawText(dst *image.RGBA, fc font.Face, c color.Color, x, y int, s string) {
	d := &font.Drawer{Dst: dst, Src: image.NewUniform(c), Face: fc, Dot: fixed.P(x, y)}
	d.DrawString(s)
}

// drawFitted scales src (keeping aspect ratio) to fit and center in r.
func drawFitted(dst *image.RGBA, src image.Image, r image.Rectangle) {
	sb := src.Bounds()
	if sb.Dx() == 0 || sb.Dy() == 0 {
		return
	}
	scale := min(float64(r.Dx())/float64(sb.Dx()), float64(r.Dy())/float64(sb.Dy()))
	w, h := int(float64(sb.Dx())*scale), int(float64(sb.Dy())*scale)
	x0 := r.Min.X + (r.Dx()-w)/2
	y0 := r.Min.Y + (r.Dy()-h)/2
	xdraw.CatmullRom.Scale(dst, image.Rect(x0, y0, x0+w, y0+h), src, sb, xdraw.Over, nil)
}

// logoIsLight reports whether a (transparent) logo is mostly white, in
// which case it needs a dark box behind it.
func logoIsLight(img image.Image) bool {
	b := img.Bounds()
	step := max(1, max(b.Dx(), b.Dy())/96)
	var lum float64
	var opaque, total int
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			total++
			r, g, bl, a := img.At(x, y).RGBA()
			if a < 0x8000 {
				continue
			}
			opaque++
			// un-premultiply
			c := color.RGBA{uint8(r * 0xffff / a >> 8), uint8(g * 0xffff / a >> 8), uint8(bl * 0xffff / a >> 8), 255}
			lum += luminance(c)
		}
	}
	if opaque == 0 || opaque > total*9/10 {
		return false // fully opaque logos carry their own background
	}
	return lum/float64(opaque) > 0.8
}

func mix(a, b color.RGBA, t float64) color.RGBA {
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	l := func(x, y uint8) uint8 { return uint8(float64(x)*(1-t) + float64(y)*t + 0.5) }
	return color.RGBA{l(a.R, b.R), l(a.G, b.G), l(a.B, b.B), 255}
}

// ===== Anti-aliased shapes with x/image/vector =====

func fillPath(dst *image.RGBA, c color.RGBA, path func(z *vector.Rasterizer)) {
	z := vector.NewRasterizer(ogWidth, ogHeight)
	path(z)
	// vector expects a premultiplied source color.
	a := uint32(c.A)
	src := color.RGBA{uint8(uint32(c.R) * a / 255), uint8(uint32(c.G) * a / 255), uint8(uint32(c.B) * a / 255), c.A}
	z.Draw(dst, dst.Bounds(), image.NewUniform(src), image.Point{})
}

const kappa = 0.5523

func circlePath(cx, cy, r float64) func(z *vector.Rasterizer) {
	return func(z *vector.Rasterizer) {
		x, y, rr, k := float32(cx), float32(cy), float32(r), float32(r*kappa)
		z.MoveTo(x+rr, y)
		z.CubeTo(x+rr, y+k, x+k, y+rr, x, y+rr)
		z.CubeTo(x-k, y+rr, x-rr, y+k, x-rr, y)
		z.CubeTo(x-rr, y-k, x-k, y-rr, x, y-rr)
		z.CubeTo(x+k, y-rr, x+rr, y-k, x+rr, y)
		z.ClosePath()
	}
}

func roundRectPath(x, y, w, h, r float64) func(z *vector.Rasterizer) {
	return func(z *vector.Rasterizer) {
		r = min(r, w/2, h/2)
		x0, y0, x1, y1 := float32(x), float32(y), float32(x+w), float32(y+h)
		rr, k := float32(r), float32(r*(1-kappa))
		z.MoveTo(x0+rr, y0)
		z.LineTo(x1-rr, y0)
		z.CubeTo(x1-k, y0, x1, y0+k, x1, y0+rr)
		z.LineTo(x1, y1-rr)
		z.CubeTo(x1, y1-k, x1-k, y1, x1-rr, y1)
		z.LineTo(x0+rr, y1)
		z.CubeTo(x0+k, y1, x0, y1-k, x0, y1-rr)
		z.LineTo(x0, y0+rr)
		z.CubeTo(x0, y0+k, x0+k, y0, x0+rr, y0)
		z.ClosePath()
	}
}

// encodeOGJPEG encodes under ogMaxBytes (WhatsApp ignores big images).
func encodeOGJPEG(img image.Image) ([]byte, error) {
	for q := 88; q >= 40; q -= 8 {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
			return nil, err
		}
		if buf.Len() < ogMaxBytes {
			return buf.Bytes(), nil
		}
	}
	return nil, fmt.Errorf("no se pudo comprimir la miniatura por debajo de %d bytes", ogMaxBytes)
}
