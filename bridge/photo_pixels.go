package main

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image"
	imagecolor "image/color"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/gen2brain/heic"
	"golang.org/x/image/draw"
)

const photoLongEdge = 512

var errPhotoDecode = errors.New("photo format is unsupported or invalid")

const photoDecodeFilter = `NOT EXISTS (SELECT 1 FROM library_processed w
	WHERE (w.account,w.id,w.revision)=(f.account,f.id,f.revision) AND w.retry_at=-1)`

func encodePhotoDigest(sum [32]byte) string { return hex.EncodeToString(sum[:]) }

func (p *photoLibrary) fetchOriginal(ctx context.Context, account, id, dest string) error {
	cmd := exec.CommandContext(ctx, filepath.Join(p.data, "bin/ente-photo-job"), "fetch", p.data, p.key, account, id, dest)
	// The CLI may include private paths or metadata in its output.
	cmd.Env = []string{}
	for _, key := range []string{"PATH", "HOME", "LANG", "LC_ALL", "TZ", "SSL_CERT_FILE", "SSL_CERT_DIR", "HTTPS_PROXY", "NO_PROXY"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return errors.New("could not fetch photo from Ente")
	}
	return nil
}

func (p *photoLibrary) derivative(ctx context.Context, key photoKey) ([]byte, error) {
	var data []byte
	err := p.db.QueryRowContext(ctx, `SELECT d.data FROM library_derivative d JOIN library_file f USING(account,id,revision)
		WHERE d.account=? AND d.id=? AND d.revision=?`, key.account, key.id, key.revision).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return data, err
}

func (p *photoLibrary) photoStill(ctx context.Context, f indexedPhoto) ([]byte, error) {
	if !p.freshAt(time.Now()) {
		return nil, errors.New("photo index is not current")
	}
	if data, err := p.derivative(ctx, f.photoKey); err != nil || data != nil {
		return data, err
	}
	select {
	case p.download <- struct{}{}:
		defer func() { <-p.download }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if data, err := p.derivative(ctx, f.photoKey); err != nil || data != nil {
		return data, err
	}
	if f.size > maxPhotoOriginal {
		return nil, errPhotoDecode
	}
	if !p.current(ctx, f.photoKey) {
		return nil, errors.New("photo is no longer in the library")
	}
	scratch, err := os.MkdirTemp(filepath.Join(p.data, ".photo-downloads"), "original-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	dest := filepath.Join(scratch, "original")
	if err := p.downloadOriginal(ctx, f.account, f.id, dest); err != nil {
		return nil, err
	}
	data, err := decodePhotoOriginal(dest, f.kind)
	if err != nil {
		if errors.Is(err, errPhotoDecode) {
			_, _ = p.db.ExecContext(ctx, `INSERT OR REPLACE INTO library_processed
				SELECT account,id,revision,-1,0 FROM library_file WHERE account=? AND id=? AND revision=?`,
				f.account, f.id, f.revision)
		}
		return nil, err
	}
	// Remove the original before saving derivatives or starting inference.
	if err := os.Remove(dest); err != nil {
		return nil, err
	}
	if err := p.storeDerivative(ctx, f.photoKey, data); err != nil {
		return nil, err
	}
	return data, p.analyzePhoto(ctx, f.photoKey, data)
}

func (p *photoLibrary) current(ctx context.Context, key photoKey) bool {
	if !p.freshAt(time.Now()) {
		return false
	}
	var found int
	err := p.db.QueryRowContext(ctx, `SELECT 1 FROM library_file WHERE account=? AND id=? AND revision=?`,
		key.account, key.id, key.revision).Scan(&found)
	return err == nil
}

func (p *photoLibrary) storeDerivative(ctx context.Context, key photoKey, data []byte) error {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var before int64
	err = tx.QueryRowContext(ctx, `SELECT bytes FROM library_derivative WHERE (account,id)=(?,?)`, key.account, key.id).Scan(&before)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO library_derivative
		SELECT account,id,revision,?,?,? FROM library_file WHERE account=? AND id=? AND revision=?`,
		data, len(data), time.Now().UnixNano(), key.account, key.id, key.revision)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errors.New("photo is no longer in the library")
	}
	// Reuse freed pages. Keep a local JPEG budget, independent of library size.
	used := p.cacheUsed + int64(len(data)) - before
	for used > p.cacheBudget {
		var row, size int64
		if err := tx.QueryRowContext(ctx, `SELECT rowid,bytes FROM library_derivative ORDER BY used LIMIT 1`).Scan(&row, &size); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM library_derivative WHERE rowid=?`, row); err != nil {
			return err
		}
		used -= size
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	p.cacheUsed = used
	return nil
}

func (p *photoLibrary) backfill(ctx context.Context) {
	model := 0
	if p.ml != nil {
		model = photoModelRevision
	}
	for ctx.Err() == nil {
		wait := 30 * time.Second
		if p.freshAt(time.Now()) {
			now := time.Now()
			f, err := scanIndexedPhoto(p.db.QueryRowContext(ctx, `SELECT `+indexedPhotoColumns+` FROM library_file f
				LEFT JOIN library_processed w USING(account,id,revision)
				WHERE f.size<=? AND (w.id IS NULL OR (w.retry_at>0 AND w.retry_at<=?) OR (w.retry_at=0 AND w.model<?))
				ORDER BY f.favorite DESC,f.taken DESC,f.id DESC LIMIT 1`, maxPhotoOriginal, now.Unix(), model))
			if err == nil {
				call, cancel := context.WithTimeout(ctx, 2*time.Minute)
				data, err := p.photoStill(call, f)
				if err == nil {
					// A cached still from before the models were enabled may
					// not have features yet; don't download it again.
					err = p.analyzePhoto(call, f.photoKey, data)
				}
				cancel()
				if err != nil && ctx.Err() == nil {
					retry := now.Add(12 * time.Hour).Unix()
					if errors.Is(err, errPhotoDecode) {
						retry = -1
					}
					_, _ = p.db.ExecContext(ctx, `INSERT OR REPLACE INTO library_processed
						SELECT account,id,revision,?,? FROM library_file WHERE account=? AND id=? AND revision=?`,
						retry, model, f.account, f.id, f.revision)
				}
				// Pace background transfers to ~1 MiB/s on average. The CLI
				// does not offer a wire-rate cap. Unknown sizes are charged 12 MiB.
				charge := f.size
				if charge <= 0 {
					charge = 12 << 20
				}
				wait = max(time.Second, time.Duration(charge)*time.Second/(1<<20)-time.Since(now))
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func decodePhotoOriginal(path, kind string) ([]byte, error) {
	var data []byte
	var err error
	if kind == "livephoto" {
		data, err = livePhotoStill(path)
	} else {
		f, openErr := os.Open(path)
		if openErr != nil {
			return nil, errPhotoDecode
		}
		data, err = io.ReadAll(io.LimitReader(f, maxPhotoOriginal+1))
		f.Close()
	}
	if err != nil || len(data) > maxPhotoOriginal {
		return nil, errPhotoDecode
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 12_000 || cfg.Height > 12_000 ||
		int64(cfg.Width)*int64(cfg.Height) > 56_000_000 {
		return nil, errPhotoDecode
	}
	if format != "jpeg" && format != "png" && format != "webp" && format != "heic" {
		return nil, errPhotoDecode
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errPhotoDecode
	}
	b := img.Bounds()
	edge := max(b.Dx(), b.Dy())
	w, h := b.Dx(), b.Dy()
	if edge > photoLongEdge {
		w, h = max(1, w*photoLongEdge/edge), max(1, h*photoLongEdge/edge)
	}
	resized := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(resized, resized.Bounds(), image.NewUniform(imagecolor.Black), image.Point{}, draw.Src)
	draw.ApproxBiLinear.Scale(resized, resized.Bounds(), img, b, draw.Over, nil)
	resized = orientPhoto(resized, photoOrientation(data, format))
	var output bytes.Buffer
	err = jpeg.Encode(&output, resized, &jpeg.Options{Quality: 82})
	return output.Bytes(), err
}

func livePhotoStill(path string) ([]byte, error) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer z.Close()
	for _, f := range z.File {
		switch strings.ToLower(filepath.Ext(f.Name)) {
		case ".heic", ".heif", ".jpeg", ".jpg", ".png", ".webp":
			if f.UncompressedSize64 > maxPhotoOriginal {
				return nil, errPhotoDecode
			}
			r, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer r.Close()
			return io.ReadAll(io.LimitReader(r, maxPhotoOriginal+1))
		}
	}
	return nil, errPhotoDecode
}

func orientPhoto(src *image.RGBA, orientation uint16) *image.RGBA {
	if orientation < 2 || orientation > 8 {
		return src
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := w, h
	if orientation >= 5 {
		dw, dh = h, w
	}
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := range h {
		for x := range w {
			var a, b int
			switch orientation {
			case 2:
				a, b = w-1-x, y
			case 3:
				a, b = w-1-x, h-1-y
			case 4:
				a, b = x, h-1-y
			case 5:
				a, b = y, x
			case 6:
				a, b = h-1-y, x
			case 7:
				a, b = h-1-y, w-1-x
			case 8:
				a, b = y, w-1-x
			}
			out.SetRGBA(a, b, src.RGBAAt(x, y))
		}
	}
	return out
}

func photoOrientation(data []byte, format string) uint16 {
	switch format {
	case "heic":
		return heicPhotoOrientation(data)
	case "jpeg":
		for data = data[2:]; len(data) >= 4 && data[0] == 0xff; {
			n := int(binary.BigEndian.Uint16(data[2:4]))
			if n < 2 || n+2 > len(data) || data[1] == 0xda {
				break
			}
			if data[1] == 0xe1 && bytes.HasPrefix(data[4:n+2], []byte("Exif\x00\x00")) {
				return tiffOrientation(data[10 : n+2])
			}
			data = data[n+2:]
		}
	case "png":
		for data = data[8:]; len(data) >= 12; {
			n := int(binary.BigEndian.Uint32(data[:4]))
			if n < 0 || n > len(data)-12 {
				break
			}
			if string(data[4:8]) == "eXIf" {
				return tiffOrientation(data[8 : n+8])
			}
			data = data[n+12:]
		}
	case "webp":
		for data = data[12:]; len(data) >= 8; {
			n := int(binary.LittleEndian.Uint32(data[4:8]))
			if n < 0 || n+(n%2) > len(data)-8 {
				break
			}
			if string(data[:4]) == "EXIF" {
				chunk := bytes.TrimPrefix(data[8:n+8], []byte("Exif\x00\x00"))
				return tiffOrientation(chunk)
			}
			data = data[n+(n%2)+8:]
		}
	}
	return 1
}

func tiffOrientation(data []byte) uint16 {
	if len(data) < 8 {
		return 1
	}
	var order binary.ByteOrder = binary.BigEndian
	switch string(data[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
	default:
		return 1
	}
	start := int(order.Uint32(data[4:8]))
	if order.Uint16(data[2:4]) != 42 || start < 8 || start > len(data)-2 {
		return 1
	}
	count := int(order.Uint16(data[start : start+2]))
	for at, i := start+2, 0; i < count && at <= len(data)-12; i, at = i+1, at+12 {
		if order.Uint16(data[at:]) == 0x112 && order.Uint16(data[at+2:]) == 3 && order.Uint32(data[at+4:]) == 1 {
			return order.Uint16(data[at+8:])
		}
	}
	return 1
}
