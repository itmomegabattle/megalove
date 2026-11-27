package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	photoWidth   = 720
	photoHeight  = 540
	photoBytes   = 96 << 10
	fixtureCount = 235
)

type sourcePhoto struct {
	ID, Author, URL string
}

type fixturePhoto struct {
	Source sourcePhoto
	Data   []byte
}

func prepareFixtures(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	var sources []sourcePhoto
	for page := 1; page <= 4; page++ {
		body, err := download(client, fmt.Sprintf("https://picsum.photos/v2/list?page=%d&limit=100", page), 1<<20)
		if err != nil {
			return err
		}
		var batch []sourcePhoto
		if err := json.Unmarshal(body, &batch); err != nil {
			return err
		}
		sources = append(sources, batch...)
	}
	rand.New(rand.NewPCG(2026, 27)).Shuffle(len(sources), func(i, j int) { sources[i], sources[j] = sources[j], sources[i] })
	jobs := make(chan sourcePhoto)
	photos := make(chan fixturePhoto, 12)
	stop := make(chan struct{})
	defer close(stop)
	var workers sync.WaitGroup
	for range 12 {
		workers.Go(func() {
			for source := range jobs {
				data, err := loadFixturePhoto(client, dir, source)
				if err == nil {
					select {
					case photos <- fixturePhoto{source, data}:
					case <-stop:
						return
					}
				}
			}
		})
	}
	go func() {
		defer close(jobs)
		for _, source := range sources {
			select {
			case jobs <- source:
			case <-stop:
				return
			}
		}
	}()
	go func() { workers.Wait(); close(photos) }()
	selected := make([]fixturePhoto, 0, fixtureCount)
	seen := map[string]bool{}
	for photo := range photos {
		if seen[photo.Source.ID] {
			continue
		}
		seen[photo.Source.ID] = true
		selected = append(selected, photo)
		if len(selected)%25 == 0 {
			fmt.Printf("Photos ready: %d/%d\n", len(selected), fixtureCount)
		}
		if len(selected) == fixtureCount {
			break
		}
	}
	if len(selected) != fixtureCount {
		return fmt.Errorf("got %d photos, need %d", len(selected), fixtureCount)
	}
	if err := writeSeed(filepath.Join(dir, "load-fixtures.sql.gz"), selected); err != nil {
		return err
	}
	manifest := make([]sourcePhoto, 0, len(selected))
	for _, photo := range selected {
		manifest = append(manifest, photo.Source)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "photo-sources.json"), data, 0600); err != nil {
		return err
	}
	fmt.Printf("Prepared %d photos, %dx%d, %d bytes each.\n", len(selected), photoWidth, photoHeight, photoBytes)
	return nil
}

func download(client *http.Client, url string, limit int64) ([]byte, error) {
	var last error
	for range 2 {
		response, err := client.Get(url)
		if err == nil {
			data, readErr := io.ReadAll(io.LimitReader(response.Body, limit+1))
			response.Body.Close()
			if response.StatusCode == http.StatusOK && readErr == nil && int64(len(data)) <= limit {
				return data, nil
			}
			err = fmt.Errorf("%s: HTTP %d, read error %v", url, response.StatusCode, readErr)
		}
		last = err
	}
	return nil, last
}

func loadFixturePhoto(client *http.Client, dir string, source sourcePhoto) ([]byte, error) {
	path := filepath.Join(dir, "picsum-"+source.ID+".jpg")
	if data, err := os.ReadFile(path); err == nil && len(data) == photoBytes {
		config, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err == nil && config.Width == photoWidth && config.Height == photoHeight {
			return data, nil
		}
	}
	data, err := download(client, fmt.Sprintf("https://picsum.photos/id/%s/%d/%d.jpg", source.ID, photoWidth, photoHeight), 2<<20)
	if err != nil {
		return nil, err
	}
	image, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if image.Bounds().Dx() != photoWidth || image.Bounds().Dy() != photoHeight {
		return nil, fmt.Errorf("wrong image size")
	}
	var encoded bytes.Buffer
	for quality := 85; quality >= 35; quality -= 5 {
		encoded.Reset()
		if err := jpeg.Encode(&encoded, image, &jpeg.Options{Quality: quality}); err != nil {
			return nil, err
		}
		if encoded.Len() <= photoBytes-4 {
			break
		}
	}
	if encoded.Len() > photoBytes-4 {
		return nil, fmt.Errorf("could not normalize %s", source.ID)
	}
	raw := encoded.Bytes()
	out := make([]byte, 0, photoBytes)
	out = append(out, raw[:2]...)
	for remaining := photoBytes - len(raw); remaining > 0; {
		size := min(remaining, 65537)
		if tail := remaining - size; tail > 0 && tail < 4 {
			size -= 4
		}
		length := size - 2
		out = append(out, 0xff, 0xfe, byte(length>>8), byte(length))
		out = append(out, make([]byte, size-4)...)
		remaining -= size
	}
	out = append(out, raw[2:]...)
	if err := os.WriteFile(path, out, 0600); err != nil {
		return nil, err
	}
	return out, nil
}

func writeSeed(path string, photos []fixturePhoto) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	z := gzip.NewWriter(file)
	writer := bufio.NewWriter(z)
	fmt.Fprintln(writer, `\set ON_ERROR_STOP on
BEGIN;
UPDATE events SET slug='loadtest', name='Нагрузочная проверка' WHERE slug='megabattle-2026' AND state='preparing';
CREATE TEMP TABLE load_fixtures (ordinal int, faculty text, role text, photo bytea);
COPY load_fixtures FROM STDIN;`)
	faculties := []string{"ktu", "tint", "nozh", "ftmf", "ftmi"}
	for i, photo := range photos {
		role := "actor"
		switch n := i % 47; {
		case n < 4:
			role = "vocalist"
		case n < 29:
			role = "dancer"
		}
		fmt.Fprintf(writer, "%d\t%s\t%s\t\\\\x%x\n", i+1, faculties[i/47], role, photo.Data)
	}
	fmt.Fprintln(writer, `\.
INSERT INTO participants (event_id, faculty_code, display_name, import_key, photo_jpeg)
SELECT e.id, f.faculty, 'Тестовый участник ' || f.ordinal, 'load-' || f.ordinal, f.photo
FROM load_fixtures f CROSS JOIN events e WHERE e.slug='loadtest';
INSERT INTO candidacies (event_id, participant_id, nomination)
SELECT p.event_id, p.id, f.role FROM load_fixtures f
JOIN participants p ON p.import_key='load-' || f.ordinal
JOIN events e ON e.id=p.event_id AND e.slug='loadtest';
COMMIT;`)
	if err := writer.Flush(); err != nil {
		return err
	}
	return z.Close()
}
