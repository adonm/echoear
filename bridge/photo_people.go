package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"image"
	"image/jpeg"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var libraryClusterID = regexp.MustCompile(`^[0-9a-f]{24}$`)

func (p *photoLibrary) peoplePage(r *http.Request, device string) (object, error) {
	ctx := r.Context()
	if !p.allowed(ctx, device) {
		return nil, problem(404, "Photo library unavailable")
	}
	q := r.URL.Query()
	offset := 0
	if raw := q.Get("people"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > 250_000 {
			return nil, problem(400, "Invalid people page")
		}
		offset = n
	}
	if cluster := q.Get("cluster"); cluster != "" {
		if !libraryClusterID.MatchString(cluster) {
			return nil, problem(400, "Invalid person group")
		}
		return p.facePage(ctx, device, cluster, offset)
	}
	rows, err := p.db.QueryContext(ctx, `SELECT p.cluster,p.name,count(f.n),sum(f.confirmed)
		FROM library_person p JOIN library_face f USING(account,cluster)
		GROUP BY p.account,p.cluster,p.name,p.updated ORDER BY p.name<>'' DESC,p.updated DESC,p.cluster LIMIT 13 OFFSET ?`, offset)
	if err != nil {
		return nil, problem(502, "Cannot list photo people")
	}
	defer rows.Close()
	people := []object{}
	for rows.Next() {
		var id, name string
		var total, confirmed int
		if rows.Scan(&id, &name, &total, &confirmed) != nil {
			return nil, problem(502, "Cannot list photo people")
		}
		people = append(people, object{"id": id, "name": name, "total": total, "confirmed": confirmed})
	}
	if rows.Err() != nil {
		return nil, problem(502, "Cannot list photo people")
	}
	next := 0
	if len(people) > 12 {
		people, next = people[:12], offset+12
	}
	return object{"people": people, "next": next, "ml": p.ml != nil}, nil
}

func (p *photoLibrary) facePage(ctx context.Context, device, cluster string, offset int) (object, error) {
	var name string
	if p.db.QueryRowContext(ctx, `SELECT name FROM library_person WHERE cluster=?`, cluster).Scan(&name) != nil {
		return nil, problem(404, "Photo person unavailable")
	}
	rows, err := p.db.QueryContext(ctx, `SELECT `+indexedPhotoColumns+`,face.n,face.confirmed
		FROM library_face face JOIN library_file f USING(account,id,revision)
		WHERE face.cluster=? ORDER BY face.confirmed, f.taken DESC,f.id,face.n LIMIT 7 OFFSET ?`, cluster, offset)
	if err != nil {
		return nil, problem(502, "Cannot list photo faces")
	}
	files, items := []indexedPhoto{}, []object{}
	for rows.Next() {
		var f indexedPhoto
		var n int
		var confirmed bool
		if rows.Scan(&f.account, &f.id, &f.revision, &f.title, &f.caption, &f.kind, &f.taken, &f.size, &n, &confirmed) != nil {
			rows.Close()
			return nil, problem(502, "Cannot list photo faces")
		}
		files, items = append(files, f), append(items, object{"face": n, "confirmed": confirmed})
	}
	if errors.Join(rows.Err(), rows.Close()) != nil {
		return nil, problem(502, "Cannot list photo faces")
	}
	next := 0
	if len(files) > 6 {
		files, items, next = files[:6], items[:6], offset+6
	}
	handles := p.newPhotoHandles(ctx, device, "", files, time.Now())
	for i, h := range handles {
		items[i]["handle"] = h
	}
	return object{"cluster": cluster, "name": name, "faces": items[:len(handles)], "next": next}, nil
}

func (p *photoLibrary) changePerson(ctx context.Context, args object, op string) (object, error) {
	id := str(args["cluster"])
	if !libraryClusterID.MatchString(id) {
		return nil, problem(400, "Invalid person group")
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, problem(502, "Cannot edit photo person")
	}
	defer tx.Rollback()
	var account string
	if err := tx.QueryRowContext(ctx, `SELECT account FROM library_person WHERE cluster=?`, id).Scan(&account); err != nil {
		return nil, problem(404, "Photo person unavailable")
	}
	if op == "person-name" {
		raw, ok := args["name"].(string)
		name := photoSearchWords(raw)
		if !ok || len(name) > 80 || strings.TrimSpace(raw) != "" && name == "" {
			return nil, problem(400, "Use a name up to 80 bytes")
		}
		_, err = tx.ExecContext(ctx, `UPDATE library_person SET name=?,updated=? WHERE account=? AND cluster=?`,
			name, time.Now().UnixNano(), account, id)
	} else {
		target := str(args["target"])
		var name string
		if target == id || !libraryClusterID.MatchString(target) ||
			tx.QueryRowContext(ctx, `SELECT name FROM library_person WHERE account=? AND cluster=?`, account, target).Scan(&name) != nil {
			return nil, problem(400, "Choose a different known person group")
		}
		// A merge proposes the moved matches; it never applies the destination's
		// name to previously confirmed faces without fresh confirmation.
		_, err = tx.ExecContext(ctx, `UPDATE library_face SET cluster=?,confirmed=0 WHERE account=? AND cluster=?`, target, account, id)
	}
	if err != nil || pruneEmptyPeople(ctx, tx) != nil || tx.Commit() != nil {
		return nil, problem(502, "Cannot edit photo person")
	}
	return object{"ok": true}, nil
}

func (p *photoLibrary) changeFace(ctx context.Context, key photoKey, args object, op string) (object, error) {
	n, err := mcpUint(args, "face")
	if err != nil || n == nil || *n > 7 {
		return nil, problem(400, "Invalid face")
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, problem(502, "Cannot edit photo face")
	}
	defer tx.Rollback()
	var cluster string
	if err := tx.QueryRowContext(ctx, `SELECT cluster FROM library_face WHERE (account,id,revision,n)=(?,?,?,?)`,
		key.account, key.id, key.revision, int(*n)).Scan(&cluster); err != nil || cluster == "" {
		return nil, problem(404, "Photo face unavailable")
	}
	confirmed := false
	switch op {
	case "face-confirm":
		confirmed = true
	case "face-reject":
		cluster = ""
	case "face-split":
		cluster = randomHex(12)
		if _, err := tx.ExecContext(ctx, `INSERT INTO library_person VALUES (?,?,'',?)`,
			key.account, cluster, time.Now().UnixNano()); err != nil {
			return nil, problem(502, "Cannot split photo face")
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE library_face SET cluster=?,confirmed=? WHERE (account,id,revision,n)=(?,?,?,?)`,
		cluster, confirmed, key.account, key.id, key.revision, int(*n))
	if err != nil || pruneEmptyPeople(ctx, tx) != nil || tx.Commit() != nil {
		return nil, problem(502, "Cannot edit photo face")
	}
	return object{"ok": true}, nil
}

func (p *photoLibrary) facePreview(ctx context.Context, key photoKey, face string, jpegData []byte) ([]byte, error) {
	n, err := strconv.Atoi(face)
	if err != nil || n < 0 || n > 7 {
		return nil, sql.ErrNoRows
	}
	var roi image.Rectangle
	err = p.db.QueryRowContext(ctx, `SELECT x1,y1,x2,y2 FROM library_face WHERE (account,id,revision,n)=(?,?,?,?) AND cluster<>''`,
		key.account, key.id, key.revision, n).Scan(&roi.Min.X, &roi.Min.Y, &roi.Max.X, &roi.Max.Y)
	if err != nil {
		return nil, err
	}
	img, err := jpeg.Decode(bytes.NewReader(jpegData))
	if err != nil {
		return nil, err
	}
	roi = roi.Inset(-max(4, roi.Dx()/5)).Intersect(img.Bounds())
	sub, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	})
	if !ok || roi.Empty() {
		return nil, errPhotoDecode
	}
	var out bytes.Buffer
	err = jpeg.Encode(&out, sub.SubImage(roi), &jpeg.Options{Quality: 85})
	return out.Bytes(), err
}
