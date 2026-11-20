package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

func editableNode(edit nodeEdit) object {
	out := maps.Clone(edit.visual)
	if edit.group != nil {
		out["group"] = *edit.group
		if *edit.group == "" {
			out["group"] = nil
		}
	}
	for k, v := range edit.controls {
		switch v := v.(type) {
		case *tapReaction:
			out[k] = editableReaction(*v)
		case *reactionCycle:
			cycle := make([]object, 0, len(*v))
			for _, step := range *v {
				cycle = append(cycle, editableReaction(step))
			}
			out[k] = cycle
		default:
			out[k] = v
		}
	}
	return out
}

func editableReaction(r tapReaction) object {
	out := object{}
	if len(r.notes) != 0 {
		out["notes"] = r.notes
	}
	for k, v := range map[string]string{"chime": r.chime, "speak": r.speak, "state": r.state, "ask": r.ask} {
		if v != "" {
			out[k] = v
		}
	}
	if r.exit {
		out["exit"] = true
	}
	if r.broadcast {
		out["broadcast"] = true
	}
	if w := r.store; w != nil {
		out["store"] = object{"scope": w.scope, "key": w.key, "value": json.RawMessage(w.value)}
	}
	if r.move != nil {
		out["move"] = object{"target": r.move.target, "dx": r.move.dx, "dy": r.move.dy}
	}
	if r.revert != 0 {
		out["revert_ms"] = r.revert.Milliseconds()
	}
	if r.then != nil {
		out["after_ms"], out["then"] = r.delay.Milliseconds(), editableReaction(*r.then)
	}
	if p := r.patch; p != nil {
		patch := object{}
		if p.bg != "" {
			patch["bg"] = p.bg
		}
		if len(p.set) > 0 {
			set := object{}
			for id, edit := range p.set {
				set[id] = editableNode(edit)
			}
			patch["set"] = set
		}
		if len(p.add) > 0 {
			add := make([]object, 0, len(p.add))
			for _, edit := range p.add {
				add = append(add, editableNode(edit))
			}
			patch["add"] = add
		}
		if len(p.del) > 0 {
			patch["del"] = slices.Sorted(maps.Keys(p.del))
		}
		out["patch"] = patch
	}
	return out
}

func editableScreen(view sceneView) object {
	out, nodes := object{"bg": view.bg}, []object{}
	for _, visual := range view.nodes {
		id := str(visual["id"])
		switch id {
		case exitNode:
			continue
		case "echoear-title":
			out["title"] = visual["s"]
			continue
		}
		var group *string
		if value, ok := view.groups[id]; ok {
			group = &value
		}
		node := editableNode(nodeEdit{visual: visual, controls: view.controls[id], group: group})
		if _, explicit := view.controls[id]["on"]; !explicit {
			delete(node, "on")
		}
		nodes = append(nodes, node)
	}
	out["nodes"] = nodes
	return out
}

func (b *bridge) saveAsset(ctx context.Context, name, kind, author string, view sceneView, states map[string]sceneView) (object, error) {
	if b.db == nil {
		return nil, problem(503, "Assets unavailable")
	}
	if !slugRE.MatchString(name) || kind != "scene" && kind != "nodes" {
		return nil, problem(400, "Invalid asset name or kind")
	}
	data := editableScreen(view)
	if len(states) != 0 {
		screens := object{}
		for name, view := range states {
			screens[name] = editableScreen(view)
		}
		data["states"] = screens
	}
	raw, err := json.Marshal(data)
	if err != nil || len(raw) > 32<<10 {
		return nil, problem(400, "Asset exceeds 32 KB")
	}
	result, err := b.db.ExecContext(ctx, `INSERT INTO assets (name,kind,author,data_json,updated_at)
		SELECT ?,?,?,?,? WHERE EXISTS(SELECT 1 FROM assets WHERE name=?) OR (SELECT count(*) FROM assets) < 128
		ON CONFLICT(name) DO UPDATE SET kind=excluded.kind,author=excluded.author,data_json=excluded.data_json,updated_at=excluded.updated_at`,
		name, kind, shortText(author, 64, true), string(raw), time.Now().UnixMilli(), name)
	if err != nil {
		return nil, problem(502, "Cannot save asset")
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, problem(502, "Cannot save asset")
	}
	if n == 0 {
		return nil, problem(409, "Asset library already has 128 assets")
	}
	return object{"ok": true, "name": name, "kind": kind, "node_count": len(data["nodes"].([]object))}, nil
}

func (b *bridge) asset(ctx context.Context, args object) (object, error) {
	if b.db == nil {
		return nil, problem(503, "Assets unavailable")
	}
	op, name := str(args["op"]), str(args["name"])
	if op != "list" && !slugRE.MatchString(name) {
		return nil, problem(400, "Invalid asset name")
	}
	dbError := problem(502, "Cannot access saved assets")
	switch op {
	case "save":
		kind := "scene"
		if value, ok := args["kind"]; ok {
			kind = str(value)
		}
		if args["nodes"] == nil {
			return nil, problem(400, "Asset needs nodes")
		}
		view, states, err := parseScene(args)
		if err != nil {
			return nil, err
		}
		return b.saveAsset(ctx, name, kind, str(args["author"]), view, states)
	case "get":
		var kind, author, raw string
		err := b.db.QueryRowContext(ctx, "SELECT kind,author,data_json FROM assets WHERE name=?", name).Scan(&kind, &author, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			return object{"found": false, "name": name}, nil
		}
		if err != nil {
			return nil, dbError
		}
		data, err := decodeObject([]byte(raw))
		if err != nil {
			return nil, dbError
		}
		return object{"found": true, "name": name, "kind": kind, "author": author, "data": data}, nil
	case "list":
		rows, err := b.db.QueryContext(ctx, "SELECT name,kind,author,updated_at FROM assets ORDER BY name")
		if err != nil {
			return nil, dbError
		}
		defer rows.Close()
		assets := []object{}
		for rows.Next() {
			var name, kind, author string
			var at int64
			if err := rows.Scan(&name, &kind, &author, &at); err != nil {
				return nil, dbError
			}
			assets = append(assets, object{"name": name, "kind": kind, "author": author, "updated_at": at})
		}
		if rows.Err() != nil {
			return nil, dbError
		}
		return object{"assets": assets}, nil
	case "delete":
		deleted, err := deleteRow(ctx, b.db, "DELETE FROM assets WHERE name=?", name)
		if err != nil {
			return nil, dbError
		}
		return object{"ok": true, "name": name, "deleted": deleted}, nil
	}
	return nil, problem(400, "Unknown asset operation")
}

func (b *bridge) assetAPI(w http.ResponseWriter, r *http.Request, path string) (any, error) {
	if path == "/api/assets" && r.Method == http.MethodGet {
		return b.asset(r.Context(), object{"op": "list"})
	}
	if name, ok := strings.CutPrefix(path, "/api/assets/"); ok && r.Method == http.MethodDelete {
		_, err := b.asset(r.Context(), object{"op": "delete", "name": name})
		if err == nil {
			w.WriteHeader(204)
		}
		return nil, err
	}
	return nil, problem(404, "Not found")
}

func (b *bridge) sceneAsset(ctx context.Context, args object) (object, error) {
	asset, err := b.asset(ctx, object{"op": "get", "name": args["asset"]})
	if err != nil {
		return nil, err
	}
	combined, found := asset["data"].(object)
	if !found {
		return nil, problem(404, "Asset not found")
	}
	base, _ := combined["nodes"].([]any)
	maps.Copy(combined, args)
	if raw, extra := args["nodes"]; extra {
		nodes, ok := raw.([]any)
		if !ok {
			return nil, problem(400, "Expected an array of nodes")
		}
		combined["nodes"] = append(base, nodes...)
	}
	return combined, nil
}
