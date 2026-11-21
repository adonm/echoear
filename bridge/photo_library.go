package main

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"
)

const photoHandleTTL = 12 * time.Minute

var libraryHandleID = regexp.MustCompile(`^lib-[0-9a-f]{32}$`)

type photoGrant struct {
	device string
	ctx    context.Context
}

type photoHandle struct {
	device string
	photo  indexedPhoto
	until  time.Time
}

type photoScopeContext struct{}

func withoutPhotoAccess(ctx context.Context) context.Context {
	return context.WithValue(ctx, photoScopeContext{}, true)
}

func (c *wsClient) canSeeLibrary() bool {
	return c.scope == nil && c.ctx.Value(photoScopeContext{}) == nil
}

func (p *photoLibrary) allowed(ctx context.Context, device string) bool {
	if p == nil || !p.freshAt(time.Now()) || ctx.Value(photoScopeContext{}) != nil {
		return false
	}
	return !p.hidden(ctx, device)
}

func (p *photoLibrary) hidden(ctx context.Context, device string) bool {
	var hidden bool
	err := p.db.QueryRowContext(ctx, `SELECT hidden FROM library_policy WHERE device=?`, strings.ToLower(device)).Scan(&hidden)
	return hidden || err != nil && !errors.Is(err, sql.ErrNoRows)
}

func (p *photoLibrary) newPhotoGrant(ctx context.Context, device string) (string, func()) {
	if p == nil {
		return "", func() {}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.grants) >= 256 || !p.allowed(ctx, device) {
		return "", func() {}
	}
	token := randomHex(16)
	p.grants[token] = photoGrant{strings.ToLower(device), ctx}
	return token, func() {
		p.mu.Lock()
		delete(p.grants, token)
		p.mu.Unlock()
	}
}

func (p *photoLibrary) validateGrant(ctx context.Context, device, token string) bool {
	if !p.allowed(ctx, device) || len(token) != 32 {
		return false
	}
	p.mu.Lock()
	g := p.grants[token]
	p.mu.Unlock()
	return g.ctx != nil && g.ctx.Err() == nil && g.device == strings.ToLower(device)
}

func (p *photoLibrary) prunePhotoCapabilities(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, v := range p.handles {
		if !v.until.After(now) {
			delete(p.handles, k)
		}
	}
	for k, v := range p.grants {
		if v.ctx.Err() != nil {
			delete(p.grants, k)
		}
	}
}

func (p *photoLibrary) newPhotoHandles(ctx context.Context, device, token string, photos []indexedPhoto, now time.Time) []string {
	p.prunePhotoCapabilities(now)
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.allowed(ctx, device) {
		return nil
	}
	if token != "" {
		g := p.grants[token]
		if g.ctx == nil || g.ctx.Err() != nil || g.device != strings.ToLower(device) {
			return nil
		}
	}
	for _, f := range photos {
		if !p.current(ctx, f.photoKey) {
			return nil
		}
	}
	handles := make([]string, 0, len(photos))
	for _, f := range photos {
		if len(p.handles) >= 4096 {
			break
		}
		id := "lib-" + randomHex(16)
		p.handles[id] = photoHandle{strings.ToLower(device), f, now.Add(photoHandleTTL)}
		handles = append(handles, id)
	}
	return handles
}

func (p *photoLibrary) getPhotoHandle(ctx context.Context, device, id string) (photoHandle, bool) {
	if p == nil || !libraryHandleID.MatchString(id) {
		return photoHandle{}, false
	}
	p.mu.Lock()
	h, ok := p.handles[id]
	p.mu.Unlock()
	if !ok || !h.until.After(time.Now()) || device != "" && !strings.EqualFold(device, h.device) ||
		!p.allowed(ctx, h.device) || !p.current(ctx, h.photo.photoKey) {
		return photoHandle{}, false
	}
	return h, true
}

func (p *photoLibrary) setPhotoHidden(ctx context.Context, device string, hidden bool) error {
	device = strings.ToLower(device)
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.db.ExecContext(ctx, `INSERT INTO library_policy VALUES (?,?)
		ON CONFLICT(device) DO UPDATE SET hidden=excluded.hidden`, device, hidden)
	if err != nil || !hidden {
		return err
	}
	for k, v := range p.handles {
		if v.device == device {
			delete(p.handles, k)
		}
	}
	for k, v := range p.grants {
		if v.device == device {
			delete(p.grants, k)
		}
	}
	return nil
}

func (p *photoLibrary) search(ctx context.Context, query, person string, limit int) ([]indexedPhoto, error) {
	ranked, err := p.metadataSearch(ctx, query, person, limit)
	if err != nil || len(ranked) == limit || query == "" || p.ml == nil {
		return ranked, err
	}
	scene, err := p.sceneMatches(ctx, query, person, limit, ranked)
	if err != nil {
		return ranked, nil // Metadata remains usable if local inference fails.
	}
	return append(ranked, scene...), nil
}

func (p *photoLibrary) metadataSearch(ctx context.Context, query, person string, limit int) ([]indexedPhoto, error) {
	out := make([]indexedPhoto, 0, limit)
	words := strings.Fields(photoSearchWords(query))
	if len(words) > 12 {
		words = words[:12]
	}
	// Require every word. Exact terms in a hand-assigned tag or title outrank
	// substring matches in captions and album names.
	where, order := "", ""
	var filter, rank []any
	tags := `((SELECT COALESCE(group_concat(t.tag,' '),'') FROM library_tag t WHERE (t.account,t.id)=(f.account,f.id))
		|| ' ' ||
		(SELECT COALESCE(group_concat(p.name,' '),'') FROM library_face face JOIN library_person p USING(account,cluster)
		 WHERE (face.account,face.id,face.revision)=(f.account,f.id,f.revision) AND face.confirmed=1))`
	if person != "" {
		where += ` AND ` + photoPersonFilter
		person = photoSearchWords(person)
		filter = append(filter, person, person)
	}
	for _, word := range words {
		where += ` AND instr(f.searchable||' '||` + tags + `,?)>0`
		filter = append(filter, word)
		order += `((instr(lower(f.title),?)>0)*2+(instr(` + tags + `,?)>0)*4)+`
		rank = append(rank, word, word)
	}
	if order != "" {
		order = strings.TrimSuffix(order, "+") + ` DESC,`
	}
	args := append(append(filter, rank...), limit)
	rows, err := p.db.QueryContext(ctx, `SELECT `+indexedPhotoColumns+` FROM library_file f
		WHERE f.size<=? AND `+photoDecodeFilter+where+` ORDER BY `+order+`f.favorite DESC,f.taken DESC,f.id DESC LIMIT ?`,
		append([]any{maxPhotoOriginal}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		f, err := scanIndexedPhoto(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func optionalPhotoString(args object, field string, maxBytes int) (string, error) {
	raw, ok := args[field]
	if !ok {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok || len(s) > maxBytes {
		return "", fieldError("photos."+field, "expected text up to %d bytes", maxBytes)
	}
	return strings.TrimSpace(s), nil
}

func (b *bridge) photoQuery(ctx context.Context, device string, raw any) (object, error) {
	args, ok := raw.(object)
	if !ok || device == "" || broadcast(device) {
		return nil, fieldError("photos", "expected an object and exact device_id")
	}
	p := b.library
	if !p.validateGrant(ctx, device, str(args["access"])) {
		return nil, problem(403, "Photo library unavailable for this turn")
	}
	query, err := optionalPhotoString(args, "query", 160)
	if err != nil {
		return nil, err
	}
	person, err := optionalPhotoString(args, "person", 80)
	if err != nil {
		return nil, err
	}
	limit := 8
	n, err := mcpUint(args, "limit")
	if err != nil || n != nil && (*n == 0 || *n > 8) {
		return nil, fieldError("photos.limit", "expected 1–8")
	}
	if n != nil {
		limit = int(*n)
	}
	files, err := p.search(ctx, query, person, limit)
	if err != nil {
		return nil, problem(502, "Cannot search photo library")
	}
	if !p.validateGrant(ctx, device, str(args["access"])) {
		return nil, problem(403, "Photo library unavailable for this turn")
	}
	ids := p.newPhotoHandles(ctx, device, str(args["access"]), files, time.Now())
	photos := make([]object, 0, len(ids))
	for i, id := range ids {
		date := ""
		if files[i].taken > 0 {
			date = time.Unix(files[i].taken, 0).UTC().Format(time.DateOnly)
		}
		// Titles/captions are untrusted strings and can contain paths or GPS.
		// Return only bridge-generated labels and dates to the cloud model.
		photos = append(photos, object{"handle": id, "label": "Photo", "date": date})
	}
	return object{"items": photos, "count": len(photos)}, nil
}
