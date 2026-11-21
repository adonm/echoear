package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
	"golang.org/x/image/draw"
	"golang.org/x/image/math/f64"
	"golang.org/x/text/unicode/norm"
)

const photoModelRevision = 1
const detectEdge = 640

var photoModelFiles = []string{
	"yunet.onnx", "sface.onnx", "vision_model_int8.onnx", "text_model_int8.onnx", "vocab.json", "merges.txt",
}

type photoML struct {
	mu                         sync.Mutex
	yunet, sface, vision, text *ort.DynamicAdvancedSession
	tokens                     clipTokens
	closed                     bool
}

func openPhotoML(dir, lib string) (_ *photoML, err error) {
	for _, f := range photoModelFiles {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return nil, err
		}
	}
	tokens, err := readClipTokens(dir)
	if err != nil {
		return nil, err
	}
	ort.SetSharedLibraryPath(lib)
	if err := ort.InitializeEnvironment(); err != nil {
		return nil, err
	}
	m := &photoML{tokens: tokens}
	defer func() {
		if err != nil {
			m.close()
		}
	}()
	o, err := ort.NewSessionOptions()
	if err != nil {
		return nil, err
	}
	defer o.Destroy()
	if err := o.SetIntraOpNumThreads(2); err != nil {
		return nil, err
	}
	if err := o.SetInterOpNumThreads(1); err != nil {
		return nil, err
	}
	detectOutputs := []string{}
	for _, layer := range []string{"cls", "obj", "bbox", "kps"} {
		for _, stride := range []int{8, 16, 32} {
			detectOutputs = append(detectOutputs, fmt.Sprintf("%s_%d", layer, stride))
		}
	}
	newSession := func(name, in string, out []string) (*ort.DynamicAdvancedSession, error) {
		return ort.NewDynamicAdvancedSession(filepath.Join(dir, name), []string{in}, out, o)
	}
	if m.yunet, err = newSession(photoModelFiles[0], "input", detectOutputs); err != nil {
		return nil, err
	}
	if m.sface, err = newSession(photoModelFiles[1], "data", []string{"fc1"}); err != nil {
		return nil, err
	}
	if m.vision, err = newSession(photoModelFiles[2], "pixel_values", []string{"image_embeds"}); err != nil {
		return nil, err
	}
	if m.text, err = newSession(photoModelFiles[3], "input_ids", []string{"text_embeds"}); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *photoML) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for _, s := range []*ort.DynamicAdvancedSession{m.yunet, m.sface, m.vision, m.text} {
		if s != nil {
			_ = s.Destroy()
		}
	}
	_ = ort.DestroyEnvironment()
}

func runPhotoModel[T ort.TensorData](s *ort.DynamicAdvancedSession, shape ort.Shape, input []T, outShapes ...ort.Shape) ([][]float32, error) {
	in, err := ort.NewTensor(shape, input)
	if err != nil {
		return nil, err
	}
	defer in.Destroy()
	out := make([]ort.Value, len(outShapes))
	data := make([][]float32, len(outShapes))
	for i, shape := range outShapes {
		o, err := ort.NewEmptyTensor[float32](shape)
		if err != nil {
			return nil, err
		}
		defer o.Destroy()
		out[i], data[i] = o, o.GetData()
	}
	if err := s.Run([]ort.Value{in}, out); err != nil {
		return nil, err
	}
	for i := range data {
		data[i] = slices.Clone(data[i])
	}
	return data, nil
}

func clipVector(data []float32) []float32 {
	var ss float64
	for _, v := range data {
		ss += float64(v) * float64(v)
	}
	if ss < 1e-12 || math.IsNaN(ss) || math.IsInf(ss, 0) {
		return nil
	}
	scale := float32(1 / math.Sqrt(ss))
	for i := range data {
		data[i] *= scale
	}
	return data
}

func (m *photoML) queryVector(query string) ([]float32, error) {
	ids := m.tokens.encode(query)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("photo models unavailable")
	}
	out, err := runPhotoModel(m.text, ort.Shape{1, int64(len(ids))}, ids, ort.Shape{1, 512})
	if err != nil {
		return nil, err
	}
	return clipVector(out[0]), nil
}

type photoFace struct {
	rect image.Rectangle
	at   [5]image.Point
	vec  []float32
}

func (m *photoML) analyze(data []byte) ([]float32, []photoFace, error) {
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, nil, err
	}
	b := img.Bounds()
	// CLIP crops the centered square of the source, then applies bicubic 224px scaling.
	side := min(b.Dx(), b.Dy())
	crop := image.Rect((b.Dx()-side)/2, (b.Dy()-side)/2, (b.Dx()+side)/2, (b.Dy()+side)/2).Add(b.Min)
	rgb := image.NewRGBA(image.Rect(0, 0, 224, 224))
	draw.CatmullRom.Scale(rgb, rgb.Bounds(), img, crop, draw.Src, nil)
	pixels := photoChannels(rgb, false, true)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, nil, errors.New("photo models unavailable")
	}
	out, err := runPhotoModel(m.vision, ort.Shape{1, 3, 224, 224}, pixels, ort.Shape{1, 512})
	if err != nil {
		return nil, nil, err
	}
	scene := clipVector(out[0])
	faces, err := m.detectFaces(img)
	if err != nil {
		return nil, nil, err
	}
	for i, f := range faces {
		face := alignPhotoFace(img, f)
		if face == nil {
			continue
		}
		out, err := runPhotoModel(m.sface, ort.Shape{1, 3, 112, 112}, photoChannels(face, false, false), ort.Shape{1, 128})
		if err != nil {
			return nil, nil, err
		}
		faces[i].vec = clipVector(out[0])
	}
	return scene, faces, nil
}

func photoChannels(img *image.RGBA, bgr, normalize bool) []float32 {
	b := img.Bounds()
	size := b.Dx() * b.Dy()
	channels := make([]float32, size*3)
	mean := [3]float32{0.48145466, 0.4578275, 0.40821073}
	std := [3]float32{0.26862954, 0.26130258, 0.27577711}
	for y := range b.Dy() {
		for x := range b.Dx() {
			i := y*b.Dx() + x
			start := y*img.Stride + 4*x
			for channel := range 3 {
				j := channel
				if bgr {
					j = 2 - channel
				}
				v := float32(img.Pix[start+j])
				if normalize {
					v = (v/255 - mean[channel]) / std[channel]
				}
				channels[channel*size+i] = v
			}
		}
	}
	return channels
}

func (m *photoML) detectFaces(img image.Image) ([]photoFace, error) {
	b := img.Bounds()
	scale := float64(detectEdge) / float64(max(b.Dx(), b.Dy()))
	w, h := int(float64(b.Dx())*scale), int(float64(b.Dy())*scale)
	padded := image.NewRGBA(image.Rect(0, 0, detectEdge, detectEdge))
	draw.BiLinear.Scale(padded, image.Rect(0, 0, w, h), img, b, draw.Src, nil)
	strides := []int{8, 16, 32}
	shapes := []ort.Shape{}
	for _, dim := range []int64{1, 1, 4, 10} {
		for _, stride := range strides {
			cells := detectEdge / stride
			shapes = append(shapes, ort.Shape{1, int64(cells * cells), dim})
		}
	}
	out, err := runPhotoModel(m.yunet, ort.Shape{1, 3, detectEdge, detectEdge}, photoChannels(padded, true, false), shapes...)
	if err != nil {
		return nil, err
	}
	type detection struct {
		photoFace
		score float64
	}
	var candidates []detection
	for k, stride := range strides {
		cells := detectEdge / stride
		for i, cls := range out[k] {
			score := math.Sqrt(float64(max(0, min(1, cls)) * max(0, min(1, out[k+3][i]))))
			if !(score > 0.85) {
				continue
			}
			xywh, points := out[k+6][i*4:i*4+4], out[k+9][i*10:i*10+10]
			gridX, gridY := float64(i%cells), float64(i/cells)
			factor := float64(stride) / scale
			cx, cy := (gridX+float64(xywh[0]))*factor, (gridY+float64(xywh[1]))*factor
			fw, fh := math.Exp(float64(xywh[2]))*factor, math.Exp(float64(xywh[3]))*factor
			r := image.Rect(int(cx-fw/2), int(cy-fh/2), int(cx+fw/2), int(cy+fh/2)).Intersect(b)
			if r.Dx() < 16 || r.Dy() < 16 || math.IsNaN(cx+cy+fw+fh) || math.IsInf(cx+cy+fw+fh, 0) {
				continue
			}
			f := photoFace{rect: r}
			for j := range 5 {
				f.at[j] = image.Pt(int((gridX+float64(points[j*2]))*factor), int((gridY+float64(points[j*2+1]))*factor))
			}
			candidates = append(candidates, detection{f, score})
		}
	}
	slices.SortStableFunc(candidates, func(a, b detection) int {
		if a.score > b.score {
			return -1
		}
		if a.score < b.score {
			return 1
		}
		return 0
	})
	var faces []photoFace
	for _, f := range candidates {
		if slices.ContainsFunc(faces, func(g photoFace) bool {
			i := f.rect.Intersect(g.rect)
			overlap := i.Dx() * i.Dy()
			union := f.rect.Dx()*f.rect.Dy() + g.rect.Dx()*g.rect.Dy() - overlap
			return overlap*10 > 3*union
		}) {
			continue
		}
		faces = append(faces, f.photoFace)
		if len(faces) == 8 {
			break
		}
	}
	return faces, nil
}

func alignPhotoFace(img image.Image, f photoFace) *image.RGBA {
	// Fit a similarity transform from detected landmarks to SFace's five-point template.
	ideal := [5][2]float64{{38.2946, 51.6963}, {73.5318, 51.5014}, {56.0252, 71.7366}, {41.5493, 92.3655}, {70.7299, 92.2041}}
	var sx, sy, tx, ty float64
	for i, p := range f.at {
		sx, sy = sx+float64(p.X)/5, sy+float64(p.Y)/5
		tx, ty = tx+ideal[i][0]/5, ty+ideal[i][1]/5
	}
	var dot, cross, variance float64
	for i, p := range f.at {
		x, y, a, b := float64(p.X)-sx, float64(p.Y)-sy, ideal[i][0]-tx, ideal[i][1]-ty
		dot, cross, variance = dot+x*a+y*b, cross+x*b-y*a, variance+x*x+y*y
	}
	if variance < 1 || dot*dot+cross*cross < 1 {
		return nil
	}
	a, b := dot/variance, cross/variance
	dst := image.NewRGBA(image.Rect(0, 0, 112, 112))
	draw.BiLinear.Transform(dst, f64.Aff3{a, -b, tx - a*sx + b*sy, b, a, ty - b*sx - a*sy}, img, img.Bounds(), draw.Src, nil)
	return dst
}

type clipTokens struct {
	vocab map[string]int64
	ranks map[[2]string]int
	bytes [256]string
}

var clipWords = regexp.MustCompile(`(?i:'s|'t|'re|'ve|'m|'ll|'d)|\p{L}+|\p{N}|[^\s\p{L}\p{N}]+`)

func readClipTokens(dir string) (clipTokens, error) {
	t := clipTokens{ranks: map[[2]string]int{}}
	vocab, err := os.ReadFile(filepath.Join(dir, "vocab.json"))
	if err != nil {
		return t, err
	}
	if json.Unmarshal(vocab, &t.vocab) != nil || t.vocab["<|startoftext|>"] == 0 || t.vocab["<|endoftext|>"] == 0 {
		return t, errors.New("invalid CLIP vocabulary")
	}
	merges, err := os.ReadFile(filepath.Join(dir, "merges.txt"))
	if err != nil {
		return t, err
	}
	for i, line := range strings.Split(string(merges), "\n") {
		pair := strings.Fields(line)
		if i != 0 && len(pair) == 2 {
			t.ranks[[2]string{pair[0], pair[1]}] = i
		}
	}
	n := 256
	for i := range 256 {
		if i >= 33 && i <= 126 || i >= 161 && i <= 172 || i >= 174 {
			t.bytes[i] = string(rune(i))
		} else {
			t.bytes[i] = string(rune(n))
			n++
		}
	}
	return t, nil
}

func (t clipTokens) encode(text string) []int64 {
	ids := []int64{t.vocab["<|startoftext|>"]}
	text = strings.ToLower(strings.Join(strings.Fields(norm.NFC.String(text)), " "))
	for _, word := range clipWords.FindAllString(text, -1) {
		part := make([]string, len(word))
		for i := range len(word) {
			part[i] = t.bytes[word[i]]
		}
		part[len(part)-1] += "</w>"
		for len(part) > 1 {
			best, rank := [2]string{}, math.MaxInt
			for i := 0; i+1 < len(part); i++ {
				pair := [2]string{part[i], part[i+1]}
				if r, ok := t.ranks[pair]; ok && r < rank {
					best, rank = pair, r
				}
			}
			if rank == math.MaxInt {
				break
			}
			merged := part[:0]
			for i := 0; i < len(part); i++ {
				if i+1 < len(part) && part[i] == best[0] && part[i+1] == best[1] {
					merged = append(merged, best[0]+best[1])
					i++
				} else {
					merged = append(merged, part[i])
				}
			}
			part = merged
		}
		for _, p := range part {
			ids = append(ids, t.vocab[p])
		}
	}
	return append(ids[:min(len(ids), 76)], t.vocab["<|endoftext|>"])
}

func encodePhotoVector(v []float32) []byte {
	out := make([]byte, 0, 4*len(v))
	for _, x := range v {
		out = binary.LittleEndian.AppendUint32(out, math.Float32bits(x))
	}
	return out
}

func photoDot(v []float32, raw []byte) float32 {
	if len(raw) != 4*len(v) {
		return -1
	}
	var dot float32
	for i, x := range v {
		dot += x * math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:]))
	}
	return dot
}
