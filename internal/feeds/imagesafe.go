package feeds

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/infodancer/web/media"
)

// errUnsupportedImage marks a response that is not a raster image herald will
// store: HTML, SVG, other text, or an unknown binary. A server that serves one
// tends to keep serving it, so callers may treat it as a permanent failure.
var errUnsupportedImage = errors.New("not a supported raster image")

// safeImg is an image made safe to serve from herald's origin.
type safeImg struct {
	Data     []byte
	MimeType string
	Width    int
	Height   int
}

// safeImage turns untrusted image bytes from a feed into a canonical raster.
// The format comes from the bytes, never the server's Content-Type. Only PNG,
// JPEG, GIF, and WebP are accepted; everything is decoded and re-encoded (WebP,
// or JPEG for JPEG sources), which strips metadata and drops any non-image bytes
// riding along. Images larger than maxDim on either side are scaled down, and
// an animated GIF keeps only its first frame.
func safeImage(data []byte, maxDim int, maxBytes int64) (safeImg, error) {
	p, err := media.Process(bytes.NewReader(data), media.Config{
		MaxUploadBytes:   maxBytes,
		MaxWidth:         maxDim,
		MaxHeight:        maxDim,
		FlattenAnimation: true,
	})
	if err != nil {
		if errors.Is(err, media.ErrUnsupportedFormat) {
			return safeImg{}, fmt.Errorf("%w: %w", errUnsupportedImage, err)
		}
		return safeImg{}, err
	}
	return safeImg{Data: p.Bytes, MimeType: p.ContentType, Width: p.Width, Height: p.Height}, nil
}

// safeFavicon is safeImage at favicon size, plus ICO. ICO is the most common
// favicon format and has no decoder in the standard library, so it is stored
// as-is once its header checks out; it is only ever served as image/x-icon with
// nosniff, which browsers will not render as a document.
func safeFavicon(data []byte) (safeImg, error) {
	if isICO(data) {
		return safeImg{Data: data, MimeType: "image/x-icon"}, nil
	}
	return safeImage(data, maxFaviconDim, maxFaviconBytes)
}

// isICO reports whether data starts with an ICONDIR header for at least one
// icon: reserved 0, type 1, count > 0.
func isICO(data []byte) bool {
	if len(data) < 6 {
		return false
	}
	return binary.LittleEndian.Uint16(data[0:2]) == 0 &&
		binary.LittleEndian.Uint16(data[2:4]) == 1 &&
		binary.LittleEndian.Uint16(data[4:6]) > 0
}
