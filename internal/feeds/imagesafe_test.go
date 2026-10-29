package feeds

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"testing"

	_ "golang.org/x/image/webp" // decode WebP output in assertions
)

const svgWithScript = `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(document.domain)</script></svg>`

const htmlWithScript = `<!doctype html><html><body><script>alert(document.domain)</script></body></html>`

func makeGIF(t *testing.T, w, h, frames int) []byte {
	t.Helper()
	pal := color.Palette{color.Black, color.White}
	g := &gif.GIF{}
	for range frames {
		g.Image = append(g.Image, image.NewPaletted(image.Rect(0, 0, w, h), pal))
		g.Delay = append(g.Delay, 10)
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, g); err != nil {
		t.Fatalf("encode gif: %v", err)
	}
	return b.Bytes()
}

// icoHeader is a minimal ICONDIR: reserved 0, type 1 (icon), count 1.
var icoHeader = []byte{0x00, 0x00, 0x01, 0x00, 0x01, 0x00}

func decodedDims(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("output does not decode: %v", err)
	}
	return cfg.Width, cfg.Height
}

func TestSafeImage_Accepts(t *testing.T) {
	cases := []struct {
		name         string
		data         []byte
		maxDim       int
		wantMIME     string
		wantW, wantH int
	}{
		{"png becomes webp", makePNG(t, 64, 64), 1200, "image/webp", 64, 64},
		{"jpeg stays jpeg", makeJPEG(t, 64, 48), 1200, "image/jpeg", 64, 48},
		{"large png is scaled to fit", makePNG(t, 2000, 1000), 1200, "image/webp", 1200, 600},
		{"static gif", makeGIF(t, 40, 20, 1), 1200, "image/webp", 40, 20},
		{"animated gif keeps first frame", makeGIF(t, 40, 20, 3), 1200, "image/webp", 40, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := safeImage(tc.data, tc.maxDim, maxImageBytes)
			if err != nil {
				t.Fatalf("safeImage: %v", err)
			}
			if got.MimeType != tc.wantMIME {
				t.Errorf("mime = %q, want %q", got.MimeType, tc.wantMIME)
			}
			if got.Width != tc.wantW || got.Height != tc.wantH {
				t.Errorf("reported dims = %dx%d, want %dx%d", got.Width, got.Height, tc.wantW, tc.wantH)
			}
			if w, h := decodedDims(t, got.Data); w != tc.wantW || h != tc.wantH {
				t.Errorf("encoded dims = %dx%d, want %dx%d", w, h, tc.wantW, tc.wantH)
			}
		})
	}
}

func TestSafeImage_RejectsNonRaster(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"html", []byte(htmlWithScript)},
		{"svg", []byte(svgWithScript)},
		{"text", []byte("not an image")},
		{"ico", append(append([]byte{}, icoHeader...), make([]byte, 64)...)},
		{"truncated png", makePNG(t, 64, 64)[:40]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := safeImage(tc.data, 1200, maxImageBytes); err == nil {
				t.Fatal("non-raster input accepted")
			}
		})
	}
}

func TestSafeImage_UnsupportedIsMarked(t *testing.T) {
	for _, data := range [][]byte{[]byte(htmlWithScript), []byte(svgWithScript)} {
		if _, err := safeImage(data, 1200, maxImageBytes); !errors.Is(err, errUnsupportedImage) {
			t.Errorf("err = %v, want errUnsupportedImage", err)
		}
	}
}

func TestSafeImage_RejectsOversize(t *testing.T) {
	if _, err := safeImage(makePNG(t, 64, 64), 1200, 32); err == nil {
		t.Fatal("oversize input accepted")
	}
}

func TestSafeFavicon(t *testing.T) {
	ico := append(append([]byte{}, icoHeader...), make([]byte, 64)...)

	t.Run("ico stored raw", func(t *testing.T) {
		got, err := safeFavicon(ico)
		if err != nil {
			t.Fatalf("safeFavicon: %v", err)
		}
		if got.MimeType != "image/x-icon" || !bytes.Equal(got.Data, ico) {
			t.Errorf("got mime %q, %d bytes; want raw ICO", got.MimeType, len(got.Data))
		}
	})
	t.Run("png scaled to favicon size", func(t *testing.T) {
		got, err := safeFavicon(makePNG(t, 512, 256))
		if err != nil {
			t.Fatalf("safeFavicon: %v", err)
		}
		if got.MimeType != "image/webp" {
			t.Errorf("mime = %q, want image/webp", got.MimeType)
		}
		if w, h := decodedDims(t, got.Data); w != maxFaviconDim || h != maxFaviconDim/2 {
			t.Errorf("dims = %dx%d, want %dx%d", w, h, maxFaviconDim, maxFaviconDim/2)
		}
	})
	for name, data := range map[string][]byte{
		"html":                  []byte(htmlWithScript),
		"svg":                   []byte(svgWithScript),
		"ico with zero entries": {0x00, 0x00, 0x01, 0x00, 0x00, 0x00},
		"short ico header":      {0x00, 0x00, 0x01, 0x00},
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			if _, err := safeFavicon(data); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
