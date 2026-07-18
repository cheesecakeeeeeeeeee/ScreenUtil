package main

import (
	"bytes"
	"image"
	"image/png"
	"log/slog"
	"os"

	"screenutil/internal/overlay"
)

func main() {
	sess := overlay.Open()
	defer sess.Close()

	region, confirmed := sess.SelectRegion()
	if !confirmed {
		slog.Info("cancelled")
		return
	}

	sess.ServeClipboard("image/png", encodePNG(sess.RegionImage(region)))
}

func encodePNG(img *image.RGBA) []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		slog.Error("Encode PNG", "err", err)
		os.Exit(1)
	}

	return buf.Bytes()
}
