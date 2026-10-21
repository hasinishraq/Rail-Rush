package api

import (
	"bytes"
	_ "embed"
	"sync"

	"github.com/go-text/typesetting/di"
	"github.com/go-text/typesetting/font"
	ot "github.com/go-text/typesetting/font/opentype"
	"github.com/go-text/typesetting/language"
	"github.com/go-text/typesetting/shaping"
	"golang.org/x/image/math/fixed"
)

// Bangla on the ticket. The PDF core fonts are Latin only, and Bangla needs
// shaping (conjuncts, reordered vowel signs), which fpdf does not do. Text with
// Bangla in it is shaped with HarfBuzz (go-text) in Hind Siliguri, which also
// covers Latin, and its glyph outlines are drawn as vector paths.
// Fonts: fonts/OFL.txt (SIL Open Font License 1.1).

var (
	//go:embed fonts/HindSiliguri-Regular.ttf
	hindRegular []byte
	//go:embed fonts/HindSiliguri-Bold.ttf
	hindBold []byte
)

// Parsed once: a *font.Font is safe for concurrent use, a *font.Face is not.
var banglaFonts = sync.OnceValues(func() ([2]*font.Font, error) {
	var out [2]*font.Font
	for i, b := range [][]byte{hindRegular, hindBold} {
		ld, err := ot.NewLoader(bytes.NewReader(b))
		if err != nil {
			return out, err
		}
		if out[i], err = font.NewFont(ld); err != nil {
			return out, err
		}
	}
	return out, nil
})

func hasBangla(s string) bool {
	for _, r := range s {
		if r >= 0x0980 && r <= 0x09FF {
			return true
		}
	}
	return false
}

// banglaText shapes and draws text for one ticket; it is not shared between requests.
type banglaText struct {
	faces  [2]*font.Face // regular, bold
	shaper shaping.HarfbuzzShaper
}

func newBanglaText() *banglaText {
	fonts, err := banglaFonts()
	if err != nil {
		return nil // embedded fonts are broken: Bangla falls back to "?"
	}
	return &banglaText{faces: [2]*font.Face{font.NewFace(fonts[0]), font.NewFace(fonts[1])}}
}

func (bt *banglaText) face(style string) *font.Face {
	if style == "B" {
		return bt.faces[1]
	}
	return bt.faces[0]
}

// shape lays s out in font units.
func (bt *banglaText) shape(s, style string) (shaping.Output, *font.Face) {
	f := bt.face(style)
	runes := []rune(s)
	out := bt.shaper.Shape(shaping.Input{
		Text: runes, RunStart: 0, RunEnd: len(runes),
		Direction: di.DirectionLTR, Face: f,
		Size:     fixed.I(int(f.Upem())), // advances come back in font units
		Script:   language.Bengali,
		Language: language.NewLanguage("bn"),
	})
	return out, f
}

// mmPerUnit converts font units to mm at a point size.
func mmPerUnit(f *font.Face, size float64) float64 {
	return size * 25.4 / 72 / float64(f.Upem())
}

func (bt *banglaText) width(s, style string, size float64) float64 {
	out, f := bt.shape(s, style)
	return fixedF(out.Advance) * mmPerUnit(f, size)
}

// draw fills the glyph outlines of s with the current fill colour, baseline at y.
func (bt *banglaText) draw(t *ticketPDF, x, y float64, s, style string, size float64) {
	out, f := bt.shape(s, style)
	k := mmPerUnit(f, size)
	pen := x
	for _, g := range out.Glyphs {
		ox, oy := pen+fixedF(g.XOffset)*k, y-fixedF(g.YOffset)*k
		if outline, ok := f.GlyphDataOutline(g.GlyphID); ok && len(outline.Segments) > 0 {
			// Font Y grows up, PDF page Y grows down.
			px := func(p ot.SegmentPoint) (float64, float64) { return ox + float64(p.X)*k, oy - float64(p.Y)*k }
			for _, seg := range outline.Segments {
				a := seg.Args
				switch seg.Op {
				case ot.SegmentOpMoveTo:
					t.MoveTo(px(a[0]))
				case ot.SegmentOpLineTo:
					t.LineTo(px(a[0]))
				case ot.SegmentOpQuadTo:
					cx, cy := px(a[0])
					ex, ey := px(a[1])
					t.CurveTo(cx, cy, ex, ey)
				case ot.SegmentOpCubeTo:
					c0x, c0y := px(a[0])
					c1x, c1y := px(a[1])
					ex, ey := px(a[2])
					t.CurveBezierCubicTo(c0x, c0y, c1x, c1y, ex, ey)
				}
			}
			t.ClosePath()
			t.DrawPath("F")
		}
		pen += fixedF(g.Advance) * k
	}
}

func fixedF(v fixed.Int26_6) float64 { return float64(v) / 64 }
