package main

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"
)

func libraryLAN(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func (b *bridge) photoParentAPI(r *http.Request) (any, error) {
	if !libraryLAN(r) {
		return nil, problem(404, "Not found")
	}
	p, ctx := b.library, r.Context()
	if p == nil {
		return object{"configured": false}, nil
	}
	device := r.URL.Query().Get("device_id")
	if !deviceIDRE.MatchString(device) || broadcast(device) {
		return nil, problem(400, "An exact device ID is required")
	}
	if _, err := b.lookupDevice(device); err != nil {
		return nil, err
	}
	switch r.Method {
	case http.MethodGet:
		out := object{"configured": true, "ready": p.freshAt(time.Now()), "hidden": p.hidden(ctx, device), "items": []object{}}
		if _, present := r.URL.Query()["people"]; present {
			return p.peoplePage(r, device)
		}
		query := r.URL.Query().Get("q")
		if len(query) > 160 {
			return nil, problem(400, "Photo search is too long")
		}
		if query == "" || !p.allowed(ctx, device) {
			return out, nil
		}
		if query == "*" {
			query = ""
		}
		files, err := p.search(ctx, query, "", 8)
		if err != nil {
			return nil, problem(502, "Cannot search photo library")
		}
		handles := p.newPhotoHandles(ctx, device, "", files, time.Now())
		items := make([]object, 0, len(handles))
		for i, id := range handles {
			f := files[i]
			tags, err := p.tagsFor(ctx, f.photoKey)
			if err != nil {
				return nil, problem(502, "Cannot read photo labels")
			}
			items = append(items, object{"handle": id, "title": shortText(f.title, 100, false), "tags": tags})
		}
		out["items"] = items
		return out, nil
	case http.MethodPost:
		args, err := jsonBody(r)
		if err != nil {
			return nil, err
		}
		op := str(args["op"])
		if op == "visibility" {
			hidden, ok := args["hidden"].(bool)
			if !ok || len(args) != 2 {
				return nil, problem(400, "Expected hidden boolean")
			}
			if err := p.setPhotoHidden(ctx, device, hidden); err != nil {
				return nil, problem(502, "Cannot save photo visibility")
			}
			if b.ws != nil && hidden {
				b.ws.clearInvalidSlides()
			}
			return object{"hidden": hidden}, nil
		}
		if op == "person-name" || op == "person-merge" {
			if !p.allowed(ctx, device) {
				return nil, problem(404, "Photo library unavailable")
			}
			return p.changePerson(ctx, args, op)
		}
		handle, ok := p.getPhotoHandle(ctx, device, str(args["handle"]))
		if !ok {
			return nil, problem(404, "Photo unavailable")
		}
		switch op {
		case "prepare":
			call, cancel := context.WithTimeout(ctx, 75*time.Second)
			defer cancel()
			if data, err := p.photoStill(call, handle.photo); err != nil && len(data) == 0 {
				return nil, problem(502, "Photo cannot be loaded")
			}
			return object{"ok": true}, nil
		case "face-confirm", "face-reject", "face-split":
			return p.changeFace(ctx, handle.photo.photoKey, args, op)
		case "labels":
			labels, err := stringList(args["labels"], "labels")
			if err != nil || len(labels) > 12 {
				return nil, problem(400, "Expected up to 12 labels")
			}
			for i, label := range labels {
				labels[i] = photoSearchWords(label)
				if labels[i] == "" || len(labels[i]) > 80 {
					return nil, problem(400, "Each label must contain text and be at most 80 bytes")
				}
			}
			if err := p.setLabels(ctx, handle.photo.photoKey, labels); err != nil {
				return nil, problem(502, "Cannot save photo labels")
			}
			return object{"ok": true}, nil
		default:
			return nil, problem(400, "Unknown library action")
		}
	default:
		return nil, problem(405, "Method not allowed")
	}
}

func (p *photoLibrary) tagsFor(ctx context.Context, key photoKey) ([]string, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT tag FROM library_tag WHERE account=? AND id=? ORDER BY tag`, key.account, key.id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tags := []string{}
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, err
		}
		tags = append(tags, tag)
	}
	return tags, rows.Err()
}

func (p *photoLibrary) setLabels(ctx context.Context, key photoKey, labels []string) error {
	t, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer t.Rollback()
	if _, err := t.ExecContext(ctx, `DELETE FROM library_tag WHERE account=? AND id=?`, key.account, key.id); err != nil {
		return err
	}
	slices.Sort(labels)
	for _, tag := range slices.Compact(labels) {
		if _, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO library_tag
			SELECT account,id,? FROM library_file WHERE account=? AND id=? AND revision=?`,
			strings.TrimSpace(tag), key.account, key.id, key.revision); err != nil {
			return err
		}
	}
	return t.Commit()
}
