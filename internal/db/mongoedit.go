package db

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// mongoEditInfo makes find results editable when every document has an _id.
func mongoEditInfo(coll *mongo.Collection, r Result) *EditInfo {
	idx := slices.IndexFunc(r.Columns, func(c Column) bool { return c.Name == "_id" })
	if idx < 0 || len(r.Rows) == 0 {
		return nil
	}
	for _, row := range r.Rows {
		if row[idx] == nil {
			return nil
		}
	}
	info := &EditInfo{Object: coll.Name(), Database: coll.Database().Name(), Key: []string{"_id"}}
	for _, c := range r.Columns {
		info.Columns = append(info.Columns, c.Name)
	}
	return info
}

// extValue parses one value written as relaxed Extended JSON, e.g. 42,
// "text", {"$oid": "..."} or {"$date": "2026-01-01T00:00:00Z"}.
func extValue(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	text, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("value %v must be sent as Extended JSON text", v)
	}
	var d bson.D
	if err := bson.UnmarshalExtJSON([]byte(`{"v":`+text+`}`), false, &d); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", text, err)
	}
	return d[0].Value, nil
}

func extDoc(m map[string]any) (bson.D, error) {
	var d bson.D
	for _, k := range sortedKeys(m) {
		v, err := extValue(m[k])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		d = append(d, bson.E{Key: k, Value: v})
	}
	return d, nil
}

// Apply writes grid edits. MongoDB has no transactions on a standalone
// server, so the changes run in order and stop at the first failure; the
// error says how many went in.
func (m *mongoDB) Apply(ctx context.Context, database string, c Changes) (Result, error) {
	if m.readOnly {
		return Result{}, ErrReadOnly
	}
	if c.Object == "" {
		return Result{}, errors.New("no collection given")
	}
	start := time.Now()
	coll := m.client.Database(m.dbName(database)).Collection(c.Object)
	done := 0
	fail := func(i int, op EditOp, err error) (Result, error) {
		if done > 0 {
			return Result{}, fmt.Errorf("change %d (%s): %w; the %d changes before it were saved", i+1, op.Op, err, done)
		}
		return Result{}, fmt.Errorf("change %d (%s): %w", i+1, op.Op, err)
	}
	for i, op := range c.Ops {
		var filter bson.D
		if op.Op != "insert" {
			id, ok := op.Key["_id"]
			if !ok || len(op.Key) != 1 {
				return fail(i, op, errors.New("a document to change needs its _id"))
			}
			v, err := extValue(id)
			if err != nil {
				return fail(i, op, err)
			}
			filter = bson.D{{Key: "_id", Value: v}}
		}
		values, err := extDoc(op.Values)
		if err != nil {
			return fail(i, op, err)
		}
		switch op.Op {
		case "update":
			if len(values) == 0 {
				return fail(i, op, errors.New("an update needs at least one value"))
			}
			res, err := coll.UpdateOne(ctx, filter, bson.D{{Key: "$set", Value: values}})
			if err == nil && res.MatchedCount != 1 {
				err = errors.New("the document is gone")
			}
			if err != nil {
				return fail(i, op, err)
			}
		case "delete":
			res, err := coll.DeleteOne(ctx, filter)
			if err == nil && res.DeletedCount != 1 {
				err = errors.New("the document is gone")
			}
			if err != nil {
				return fail(i, op, err)
			}
		case "insert":
			if _, err := coll.InsertOne(ctx, values); err != nil {
				return fail(i, op, err)
			}
		default:
			return fail(i, op, fmt.Errorf("unknown edit %q", op.Op))
		}
		done++
	}
	return Result{Message: editSummary(c), Affected: int64(done), Millis: time.Since(start).Milliseconds()}, nil
}
