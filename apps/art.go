package apps

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
)

// Momentum glyphs: one image per token, drawn from the hash of its collection
// and id, so the same token is the same picture on any machine and no asset
// has to ship with the node. The palette is the Zenon tri-colour.

type Glyph struct {
	Collection string
	ID         int
	Rays       int
	Rings      int
	Palette    string
	Rotation   float64
	Weight     int

	primary, secondary color.RGBA
}

func rgb(v uint32) color.RGBA {
	return color.RGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 255}
}

var palettes = []struct {
	name               string
	primary, secondary color.RGBA
}{
	{"Plasma", rgb(0x00D557), rgb(0x0061EB)},
	{"Momentum", rgb(0x0061EB), rgb(0xF91690)},
	{"Sentinel", rgb(0xF91690), rgb(0x00D557)},
	{"Pillar", rgb(0x00D557), rgb(0xE8E8E8)},
}

// NewGlyph derives a token's appearance from its name. Nothing here is random:
// the hash is the whole seed.
func NewGlyph(collection string, id int) Glyph {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s#%d", collection, id))
	at := func(i int) uint32 { return binary.BigEndian.Uint32(sum[i*4 : i*4+4]) }
	p := palettes[int(at(0))%len(palettes)]
	return Glyph{
		Collection: collection,
		ID:         id,
		Rays:       6 + int(at(1))%19,
		Rings:      1 + int(at(2))%5,
		Palette:    p.name,
		Rotation:   float64(at(3)%360) * math.Pi / 180,
		Weight:     1 + int(at(4))%100,
		primary:    p.primary,
		secondary:  p.secondary,
	}
}

func (g Glyph) Attributes() []map[string]any {
	return []map[string]any{
		{"trait_type": "Palette", "value": g.Palette},
		{"trait_type": "Rays", "value": g.Rays},
		{"trait_type": "Rings", "value": g.Rings},
		{"trait_type": "Weight", "value": g.Weight},
	}
}

type canvas struct {
	w, h int
	pix  []float64 // RGB, 0..1
}

func newCanvas(w, h int) *canvas { return &canvas{w: w, h: h, pix: make([]float64, w*h*3)} }

func (c *canvas) blend(x, y int, col color.RGBA, alpha float64) {
	if x < 0 || y < 0 || x >= c.w || y >= c.h || alpha <= 0 {
		return
	}
	if alpha > 1 {
		alpha = 1
	}
	i := (y*c.w + x) * 3
	for k, v := range [3]float64{float64(col.R) / 255, float64(col.G) / 255, float64(col.B) / 255} {
		c.pix[i+k] = c.pix[i+k]*(1-alpha) + v*alpha
	}
}

func (c *canvas) image(scale int) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, c.w/scale, c.h/scale))
	n := float64(scale * scale)
	for y := 0; y < c.h/scale; y++ {
		for x := 0; x < c.w/scale; x++ {
			var r, g, b float64
			for dy := 0; dy < scale; dy++ {
				for dx := 0; dx < scale; dx++ {
					i := ((y*scale+dy)*c.w + x*scale + dx) * 3
					r, g, b = r+c.pix[i], g+c.pix[i+1], b+c.pix[i+2]
				}
			}
			out.Set(x, y, color.RGBA{uint8(r / n * 255), uint8(g / n * 255), uint8(b / n * 255), 255})
		}
	}
	return out
}

// PNG draws the glyph at `size` pixels square, supersampled twice.
func (g Glyph) PNG(size int) ([]byte, error) {
	const scale = 2
	w := size * scale
	c := newCanvas(w, w)
	center := float64(w) / 2
	radius := center * 0.86

	// Ground: a dark radial fall-off, so the glyph sits in something.
	for y := 0; y < w; y++ {
		for x := 0; x < w; x++ {
			d := math.Hypot(float64(x)-center, float64(y)-center) / center
			shade := uint8(26 - 14*math.Min(1, d))
			c.blend(x, y, color.RGBA{shade, shade, shade, 255}, 1)
		}
	}

	// Rays: spikes around the mark, alternating the two colours.
	inner := radius * 0.3
	for i := 0; i < g.Rays; i++ {
		angle := g.Rotation + 2*math.Pi*float64(i)/float64(g.Rays)
		length := radius * (0.5 + 0.5*math.Abs(math.Sin(float64(i)*2.399+g.Rotation)))
		width := radius * 0.035 * (1 + float64(g.Weight%7)/10)
		col := g.primary
		if i%2 == 1 {
			col = g.secondary
		}
		c.wedge(center, center, angle, inner, length, width, col)
	}

	// Rings: the pool of momentums the glyph sits in.
	for i := 1; i <= g.Rings; i++ {
		r := radius * float64(i) / float64(g.Rings+1)
		col := g.secondary
		if i%2 == 1 {
			col = g.primary
		}
		c.ring(center, center, r, float64(scale)*1.6, col, 0.55)
	}

	// The Z at the centre, the mark itself.
	c.zGlyph(center, center, radius*0.42, g.primary)

	var buf bytes.Buffer
	if err := png.Encode(&buf, c.image(scale)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// wedge draws a spike along `angle`, starting clear of the centre so the mark
// there stays readable, and tapering to a point.
func (c *canvas) wedge(cx, cy, angle, inner, length, width float64, col color.RGBA) {
	steps := int(length - inner)
	if steps <= 0 {
		return
	}
	for i := 0; i < steps; i++ {
		t := float64(i) / float64(steps)
		d := inner + float64(i)
		x := cx + math.Cos(angle)*d
		y := cy + math.Sin(angle)*d
		half := width * (1 - t)
		for w := -half; w <= half; w += 0.5 {
			px := x + math.Cos(angle+math.Pi/2)*w
			py := y + math.Sin(angle+math.Pi/2)*w
			c.blend(int(px), int(py), col, 0.85*(1-t*0.6))
		}
	}
}

func (c *canvas) ring(cx, cy, r, thickness float64, col color.RGBA, alpha float64) {
	steps := int(2 * math.Pi * r * 2)
	for i := 0; i < steps; i++ {
		a := 2 * math.Pi * float64(i) / float64(steps)
		for t := -thickness / 2; t <= thickness/2; t += 0.5 {
			c.blend(int(cx+math.Cos(a)*(r+t)), int(cy+math.Sin(a)*(r+t)), col, alpha)
		}
	}
}

// zGlyph draws the Z from the ZNN mark: two bars and the diagonal between
// them, on a dark plate so the rays behind it do not read through.
func (c *canvas) zGlyph(cx, cy, size float64, col color.RGBA) {
	half := size / 2
	bar := size * 0.26
	pad := bar * 0.55
	fill := func(x0, y0, x1, y1 float64) {
		for y := y0; y <= y1; y += 0.5 {
			for x := x0; x <= x1; x += 0.5 {
				c.blend(int(x), int(y), col, 1)
			}
		}
	}
	for y := cy - half - pad; y <= cy+half+pad; y += 0.5 {
		for x := cx - half - pad; x <= cx+half+pad; x += 0.5 {
			c.blend(int(x), int(y), color.RGBA{10, 10, 10, 255}, 0.92)
		}
	}
	fill(cx-half, cy-half, cx+half, cy-half+bar)
	fill(cx-half, cy+half-bar, cx+half, cy+half)

	// The diagonal, swept along its own direction with a perpendicular width,
	// so it meets both bars squarely.
	x0, y0 := cx+half, cy-half+bar
	x1, y1 := cx-half, cy+half-bar
	dx, dy := x1-x0, y1-y0
	length := math.Hypot(dx, dy)
	nx, ny := -dy/length, dx/length
	for t := 0.0; t <= 1; t += 0.5 / length {
		px, py := x0+dx*t, y0+dy*t
		for w := -bar / 2; w <= bar/2; w += 0.5 {
			c.blend(int(px+nx*w), int(py+ny*w), col, 1)
		}
	}
}

// Metadata is the JSON a token URI points at: the fields every marketplace
// reads, including the ones OpenSea defined.
func (g Glyph) Metadata(name, description, imageURL string) map[string]any {
	return map[string]any{
		"name":        name,
		"description": description,
		"image":       imageURL,
		"attributes":  g.Attributes(),
	}
}
