package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestBallotPhotosHaveUniformSize(t *testing.T) {
	for _, bounds := range []image.Rectangle{image.Rect(0, 0, 40, 30), image.Rect(0, 0, 900, 1200)} {
		var source bytes.Buffer
		img := image.NewRGBA(bounds)
		img.Set(15, 15, color.RGBA{R: 255, A: 255})
		if err := png.Encode(&source, img); err != nil {
			t.Fatal(err)
		}
		output, err := preparePhoto(&source)
		if err != nil || len(output) != ballotPhotoBytes {
			t.Fatalf("normalized size: %d, %v", len(output), err)
		}
		decoded, err := jpeg.Decode(bytes.NewReader(output))
		if err != nil || decoded.Bounds().Dx() != 720 || decoded.Bounds().Dy() != 720 {
			t.Fatalf("invalid square JPEG: %v", err)
		}
	}
}
