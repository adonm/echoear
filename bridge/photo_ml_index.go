package main

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"
)

const photoPersonFilter = `(EXISTS (SELECT 1 FROM library_tag t WHERE (t.account,t.id)=(f.account,f.id) AND t.tag=?)
	OR EXISTS (SELECT 1 FROM library_face face JOIN library_person p USING(account,cluster)
		WHERE (face.account,face.id,face.revision)=(f.account,f.id,f.revision) AND face.confirmed=1 AND p.name=?))`

func (p *photoLibrary) analyzePhoto(ctx context.Context, key photoKey, data []byte) error {
	if p.ml == nil {
		_, err := p.db.ExecContext(ctx, `INSERT OR IGNORE INTO library_processed
			SELECT account,id,revision,0,0 FROM library_file WHERE account=? AND id=? AND revision=?`, key.account, key.id, key.revision)
		return err
	}
	var model int
	err := p.db.QueryRowContext(ctx, `SELECT model FROM library_processed WHERE (account,id,revision)=(?,?,?) AND retry_at=0`,
		key.account, key.id, key.revision).Scan(&model)
	if err == nil && model == photoModelRevision {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	scene, faces, err := p.ml.analyze(data)
	if err != nil {
		return err
	}
	return p.storePhotoFeatures(ctx, key, scene, faces)
}

func (p *photoLibrary) storePhotoFeatures(ctx context.Context, key photoKey, scene []float32, faces []photoFace) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM library_file WHERE (account,id,revision)=(?,?,?)`,
		key.account, key.id, key.revision).Scan(&current); err != nil {
		return err
	}
	if len(scene) != 0 {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO library_vector VALUES (?,?,?,?)`,
			key.account, key.id, key.revision, encodePhotoVector(scene)); err != nil {
			return err
		}
	}
	type prototype struct {
		id   string
		data []byte
	}
	var people []prototype
	if len(faces) != 0 {
		rows, err := tx.QueryContext(ctx, `SELECT p.cluster,(SELECT f.data FROM library_face f
			WHERE (f.account,f.cluster)=(p.account,p.cluster) AND f.id<>?
			ORDER BY confirmed DESC,id,n LIMIT 1) FROM library_person p
			WHERE p.account=? ORDER BY p.name<>'' DESC,p.updated DESC LIMIT 1024`, key.id, key.account)
		if err != nil {
			return err
		}
		for rows.Next() {
			var v prototype
			if err := rows.Scan(&v.id, &v.data); err != nil {
				rows.Close()
				return err
			}
			if len(v.data) != 0 {
				people = append(people, v)
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
	}
	assigned := map[string]bool{}
	for i, f := range faces {
		if len(f.vec) == 0 {
			continue
		}
		best, second := float32(0), float32(0)
		candidate := ""
		for _, person := range people {
			if assigned[person.id] {
				continue
			}
			s := photoDot(f.vec, person.data)
			if s > best {
				best, second, candidate = s, best, person.id
			} else if s > second {
				second = s
			}
		}
		if candidate == "" || best < 0.55 || best-second < 0.10 {
			candidate = randomHex(12)
			if _, err := tx.ExecContext(ctx, `INSERT INTO library_person VALUES (?,?,'',?)`,
				key.account, candidate, time.Now().UnixNano()); err != nil {
				return err
			}
		}
		assigned[candidate] = true
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO library_face VALUES (?,?,?,?,?,?,?,?,?,?,0)`,
			key.account, key.id, key.revision, i, f.rect.Min.X, f.rect.Min.Y, f.rect.Max.X, f.rect.Max.Y,
			encodePhotoVector(f.vec), candidate); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO library_processed VALUES (?,?,?,0,?)`,
		key.account, key.id, key.revision, photoModelRevision); err != nil {
		return err
	}
	return tx.Commit()
}

func pruneEmptyPeople(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM library_person AS p WHERE NOT EXISTS
		(SELECT 1 FROM library_face f WHERE (f.account,f.cluster)=(p.account,p.cluster))`)
	return err
}

func (p *photoLibrary) sceneMatches(ctx context.Context, query string, person string, limit int, exclude []indexedPhoto) ([]indexedPhoto, error) {
	v, err := p.ml.queryVector("a photo of " + query)
	if err != nil || len(v) == 0 {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	where := ""
	args := []any{maxPhotoOriginal}
	if person != "" {
		person = photoSearchWords(person)
		where = ` AND ` + photoPersonFilter
		args = append(args, person, person)
	}
	seen := map[photoKey]bool{}
	for _, f := range exclude {
		seen[f.photoKey] = true
	}
	rows, err := p.db.QueryContext(ctx, `SELECT `+indexedPhotoColumns+`,v.data
		FROM library_vector v JOIN library_file f USING(account,id,revision) WHERE f.size<=? AND `+photoDecodeFilter+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type hit struct {
		indexedPhoto
		score float32
	}
	var best []hit
	limit -= len(exclude)
	for rows.Next() {
		var f indexedPhoto
		var raw []byte
		if err := rows.Scan(&f.account, &f.id, &f.revision, &f.title, &f.caption, &f.kind, &f.taken, &f.size, &raw); err != nil {
			return nil, err
		}
		if seen[f.photoKey] {
			continue
		}
		score := photoDot(v, raw)
		if !(score >= 0.21) || len(best) == limit && score <= best[len(best)-1].score {
			continue
		}
		best = append(best, hit{f, score})
		slices.SortStableFunc(best, func(a, b hit) int {
			if a.score > b.score {
				return -1
			}
			if a.score < b.score {
				return 1
			}
			return 0
		})
		best = best[:min(len(best), limit)]
	}
	found := make([]indexedPhoto, 0, len(best))
	for _, item := range best {
		found = append(found, item.indexedPhoto)
	}
	return found, rows.Err()
}
