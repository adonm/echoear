package main

import "encoding/binary"

// The HEIC decoder already applies the primary item's container transform.
// Use the linked Exif item's TIFF orientation only when irot/imir is absent.
func heicPhotoOrientation(file []byte) uint16 {
	meta := heifBox(file, "meta")
	if len(meta) < 4 || len(meta) > 2<<20 {
		return 1
	}
	meta = meta[4:]
	pitm := heifBox(meta, "pitm")
	var primary uint64
	if len(pitm) >= 6 {
		b := heifCursor{b: pitm[4:]}
		primary = b.read(2 + 2*min(int(pitm[0]), 1))
	}
	if heifContainerOrients(heifBox(meta, "iprp"), primary) {
		return 1
	}
	info := heifBox(meta, "iinf")
	if len(info) < 6 {
		return 1
	}
	skip := 6 + 2*min(int(info[0]), 1)
	if len(info) < skip {
		return 1
	}
	exif := map[uint64]bool{}
	heifBoxes(info[skip:], func(kind string, data []byte) bool {
		if kind == "infe" && len(data) >= 12 && (data[0] == 2 || data[0] == 3) {
			n := 2 + 2*int(data[0]-2)
			if len(data) >= 10+n && string(data[6+n:10+n]) == "Exif" {
				c := heifCursor{b: data[4:]}
				exif[c.read(n)] = true
			}
		}
		return false
	})
	ref := heifBox(meta, "iref")
	exifID := uint64(0)
	if len(ref) >= 4 {
		width := 2 + 2*min(int(ref[0]), 1)
		heifBoxes(ref[4:], func(kind string, data []byte) bool {
			if kind != "cdsc" {
				return false
			}
			c := heifCursor{b: data}
			from, count := c.read(width), c.read(2)
			for range min(count, uint64(len(c.b)/width)) {
				if id := c.read(width); id == primary && exif[from] {
					exifID = from
					return true
				}
			}
			return false
		})
	}
	if exifID == 0 && len(exif) == 1 {
		for id := range exif {
			exifID = id
		}
	}
	if exifID == 0 {
		return 1
	}
	payload := heifExifItem(heifBox(meta, "iloc"), file, heifBox(meta, "idat"), exifID)
	if len(payload) < 4 {
		return 1
	}
	offset := uint64(binary.BigEndian.Uint32(payload[:4])) + 4
	if offset > uint64(len(payload)) {
		return 1
	}
	return tiffOrientation(payload[offset:])
}

func heifBoxes(data []byte, visit func(string, []byte) bool) {
	for len(data) >= 8 {
		size, header := uint64(binary.BigEndian.Uint32(data[:4])), uint64(8)
		switch size {
		case 0:
			size = uint64(len(data))
		case 1:
			if len(data) < 16 {
				return
			}
			size, header = binary.BigEndian.Uint64(data[8:16]), 16
		}
		if size < header || size > uint64(len(data)) || visit(string(data[4:8]), data[header:size]) {
			return
		}
		data = data[size:]
	}
}

func heifBox(data []byte, name string) (out []byte) {
	heifBoxes(data, func(kind string, payload []byte) bool {
		if kind == name {
			out = payload
			return true
		}
		return false
	})
	return
}

func heifContainerOrients(props []byte, primary uint64) bool {
	r := map[uint64]bool{}
	var index uint64
	heifBoxes(heifBox(props, "ipco"), func(kind string, _ []byte) bool {
		index++
		if kind == "irot" || kind == "imir" {
			r[index] = true
		}
		return false
	})
	if len(r) == 0 {
		return false
	}
	match := false
	heifBoxes(props, func(kind string, data []byte) bool {
		if kind != "ipma" || len(data) < 8 {
			return false
		}
		itemWidth, propWidth := 2+2*min(int(data[0]), 1), 1+int(data[3]&1)
		c := heifCursor{b: data[4:]}
		n := c.read(4)
		for range min(n, uint64(len(c.b)/(itemWidth+1))) {
			id, count := c.read(itemWidth), c.read(1)
			for range min(count, uint64(len(c.b)/propWidth)) {
				prop := c.read(propWidth) & ((1 << (8*propWidth - 1)) - 1)
				if id == primary && r[prop] {
					match = true
					return true
				}
			}
		}
		return false
	})
	return match
}

type heifCursor struct {
	b   []byte
	bad bool
}

func (c *heifCursor) read(n int) (v uint64) {
	if n > 8 || n > len(c.b) {
		c.bad, c.b = true, nil
		return 0
	}
	for _, b := range c.b[:n] {
		v = v<<8 | uint64(b)
	}
	c.b = c.b[n:]
	return
}

func heifExifItem(location, file, idat []byte, exif uint64) []byte {
	if len(location) < 8 || location[0] > 2 {
		return nil
	}
	version := location[0]
	c := heifCursor{b: location[6:]}
	offset, length := int(location[4]>>4), int(location[4]&15)
	base, index := int(location[5]>>4), int(location[5]&15)
	if length == 0 {
		return nil
	}
	if version == 0 {
		index = 0
	}
	idWidth := 2
	if version == 2 {
		idWidth = 4
	}
	for range min(c.read(idWidth), uint64(len(c.b)/6)) {
		id := c.read(idWidth)
		method := uint64(0)
		if version != 0 {
			method = c.read(2) & 15
		}
		ref, baseOffset, n := c.read(2), c.read(base), c.read(2)
		src := file
		if method == 1 {
			src = idat
		}
		var payload []byte
		for range n {
			_ = c.read(index)
			at, size := c.read(offset), c.read(length)
			if c.bad {
				return nil
			}
			if id != exif {
				continue
			}
			end := uint64(len(src))
			if ref != 0 || method > 1 || baseOffset > end || at > end-baseOffset || size > end-baseOffset-at ||
				size > uint64((1<<20)-len(payload)) {
				return nil
			}
			start := baseOffset + at
			payload = append(payload, src[start:start+size]...)
		}
		if id == exif {
			return payload
		}
	}
	return nil
}
