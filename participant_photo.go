package main

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	stddraw "image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const maxPhotoBytes = 10 << 20
const ballotPhotoBytes = 96 << 10

func uploadedPhoto(r *http.Request) ([]byte, error) {
	file, _, err := r.FormFile("photo")
	if errors.Is(err, http.ErrMissingFile) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("Не удалось прочитать фото.")
	}
	defer file.Close()
	return preparePhoto(file)
}

func preparePhoto(file io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(file, maxPhotoBytes+1))
	if err != nil || len(data) > maxPhotoBytes {
		return nil, errors.New("Не удалось принять файл. Открой предпросмотр, чтобы браузер уменьшил фото перед отправкой.")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("Не удалось прочитать изображение. Проверь предпросмотр или выбери другой файл.")
	}
	if config.Width < 1 || config.Height < 1 || int64(config.Width)*int64(config.Height) > 16_000_000 {
		return nil, errors.New("Разрешение фото должно быть не больше 16 мегапикселей.")
	}
	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("Фото повреждено или не читается.")
	}
	side := min(config.Width, config.Height)
	origin := source.Bounds().Min.Add(image.Pt((config.Width-side)/2, (config.Height-side)/2))
	crop := image.Rectangle{Min: origin, Max: origin.Add(image.Pt(side, side))}
	const size = 720
	thumbnail := image.NewRGBA(image.Rect(0, 0, size, size))
	stddraw.Draw(thumbnail, thumbnail.Bounds(), image.NewUniform(color.White), image.Point{}, stddraw.Src)
	draw.ApproxBiLinear.Scale(thumbnail, thumbnail.Bounds(), source, crop, draw.Over, nil)
	var out bytes.Buffer
	for quality := 90; quality >= 5; quality -= 5 {
		out.Reset()
		if err := jpeg.Encode(&out, thumbnail, &jpeg.Options{Quality: quality}); err != nil {
			return nil, errors.New("Не удалось подготовить фото.")
		}
		if out.Len() <= ballotPhotoBytes-4 {
			raw := out.Bytes()
			padded := make([]byte, 0, ballotPhotoBytes)
			padded = append(padded, raw[:2]...)
			for remaining := ballotPhotoBytes - len(raw); remaining > 0; {
				chunk := min(remaining, 65537)
				if tail := remaining - chunk; tail > 0 && tail < 4 {
					chunk -= 4
				}
				length := chunk - 2
				padded = append(padded, 0xff, 0xfe, byte(length>>8), byte(length))
				padded = append(padded, make([]byte, chunk-4)...)
				remaining -= chunk
			}
			return append(padded, raw[2:]...), nil
		}
	}
	return nil, errors.New("Не удалось сжать фото. Попробуй другой кадр.")
}

func previewPortrait(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPhotoBytes)
	data, err := preparePhoto(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "image/jpeg")
	w.Write(data)
}
