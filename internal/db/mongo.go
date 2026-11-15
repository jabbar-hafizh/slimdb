package db

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/connstring"
)

type mongoDB struct {
	client    *mongo.Client
	defaultDB string
	readOnly  bool
}

// writeMethods are refused on read-only connections. runCommand is included
// because it can do anything.
var writeMethods = map[string]bool{
	"insertOne": true, "insertMany": true, "updateOne": true, "updateMany": true,
	"replaceOne": true, "deleteOne": true, "deleteMany": true, "createIndex": true,
	"dropIndex": true, "drop": true, "createCollection": true, "dropDatabase": true,
	"runCommand": true,
}

func openMongo(ctx context.Context, url string, readOnly bool) (*mongoDB, error) {
	cs, err := connstring.ParseAndValidate(url)
	if err != nil {
		return nil, err
	}
	client, err := mongo.Connect(options.Client().ApplyURI(url).SetAppName("slimdb"))
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, err
	}
	def := cs.Database
	if def == "" {
		def = "test"
	}
	return &mongoDB{client: client, defaultDB: def, readOnly: readOnly}, nil
}

func (m *mongoDB) Close() { _ = m.client.Disconnect(context.Background()) }

func (m *mongoDB) dbName(name string) string {
	if name == "" {
		return m.defaultDB
	}
	return name
}

// Tree lists every database the user can see, falling back to the database
// from the URL when listDatabases is not allowed.
func (m *mongoDB) Tree(ctx context.Context) ([]Node, error) {
	names, err := m.client.ListDatabaseNames(ctx, bson.D{})
	if err != nil {
		names = []string{m.defaultDB}
	}
	sort.Strings(names)
	var out []Node
	for _, name := range names {
		specs, err := m.client.Database(name).ListCollectionSpecifications(ctx, bson.D{})
		if err != nil {
			return nil, err
		}
		sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
		node := Node{Name: name, Kind: "database", Database: name}
		for _, s := range specs {
			kind := "collection"
			if s.Type == "view" {
				kind = "view"
			}
			node.Children = append(node.Children, Node{Name: s.Name, Kind: kind, Object: s.Name, Database: name})
		}
		out = append(out, node)
	}
	return out, nil
}

// Describe infers the top-level fields from a sample of 200 documents and lists the indexes.
func (m *mongoDB) Describe(ctx context.Context, database, object string) ([]Result, error) {
	coll := m.client.Database(m.dbName(database)).Collection(object)

	cur, err := coll.Aggregate(ctx, bson.A{bson.D{{Key: "$sample", Value: bson.D{{Key: "size", Value: 200}}}}})
	if err != nil {
		return nil, err
	}
	var docs []bson.D
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	type field struct {
		name  string
		types map[string]bool
		seen  int
	}
	var order []*field
	byName := map[string]*field{}
	for _, d := range docs {
		for _, e := range d {
			f := byName[e.Key]
			if f == nil {
				f = &field{name: e.Key, types: map[string]bool{}}
				byName[e.Key] = f
				order = append(order, f)
			}
			f.seen++
			f.types[bsonTypeName(e.Value)] = true
		}
	}
	fields := Result{
		Title:   "Fields",
		Columns: []Column{{Name: "field"}, {Name: "types"}, {Name: "present"}},
		Rows:    [][]any{},
		Message: fmt.Sprintf("inferred from a sample of %d documents", len(docs)),
	}
	for _, f := range order {
		types := make([]string, 0, len(f.types))
		for t := range f.types {
			types = append(types, t)
		}
		sort.Strings(types)
		fields.Rows = append(fields.Rows, []any{f.name, strings.Join(types, " | "), fmt.Sprintf("%.0f%%", 100*float64(f.seen)/float64(len(docs)))})
	}

	idxCur, err := coll.Indexes().List(ctx)
	if err != nil {
		return nil, err
	}
	var idx []bson.D
	if err := idxCur.All(ctx, &idx); err != nil {
		return nil, err
	}
	indexes := docsResult(idx, 0)
	indexes.Title = "Indexes"
	return []Result{fields, indexes}, nil
}

func (m *mongoDB) Query(ctx context.Context, database, q string, limit int) ([]Result, error) {
	stmts, err := parseMongo(q)
	if err != nil {
		return []Result{{Statement: q, Error: err.Error()}}, nil
	}
	if len(stmts) == 0 {
		return nil, errors.New("nothing to run")
	}
	current := m.dbName(database)
	var out []Result
	for _, st := range stmts {
		start := time.Now()
		var r Result
		if st.kind == "use" {
			current = st.arg
			r = Result{Message: "switched to db " + current}
		} else {
			r, err = m.run(ctx, current, st, limit)
			if err != nil {
				r = Result{Error: err.Error()}
			}
		}
		r.Statement = st.text
		r.Millis = time.Since(start).Milliseconds()
		out = append(out, r)
		if r.Error != "" {
			break
		}
	}
	return out, nil
}

func (m *mongoDB) run(ctx context.Context, current string, st mongoStmt, limit int) (Result, error) {
	if st.kind == "show" {
		return m.show(ctx, current, st.arg)
	}
	db := m.client.Database(m.dbName(firstNonEmpty(st.db, current)))
	call := st.calls[0]
	if m.readOnly && (writeMethods[call.name] || (call.name == "aggregate" && pipelineWrites(call.args))) {
		return Result{}, fmt.Errorf("%s is not allowed on a read-only connection", call.name)
	}
	if st.coll == "" {
		if len(st.calls) > 1 {
			return Result{}, fmt.Errorf("%s() cannot be chained", call.name)
		}
		return m.dbCall(ctx, db, call)
	}
	return m.collCall(ctx, db.Collection(st.coll), call, st.calls[1:], limit)
}

func (m *mongoDB) show(ctx context.Context, current, what string) (Result, error) {
	switch what {
	case "dbs", "databases":
		res, err := m.client.ListDatabases(ctx, bson.D{})
		if err != nil {
			return Result{}, err
		}
		r := Result{Columns: []Column{{Name: "name"}, {Name: "sizeOnDisk"}, {Name: "empty"}}, Rows: [][]any{}}
		for _, d := range res.Databases {
			r.Rows = append(r.Rows, []any{d.Name, d.SizeOnDisk, d.Empty})
		}
		return r, nil
	case "collections", "tables":
		names, err := m.client.Database(current).ListCollectionNames(ctx, bson.D{})
		if err != nil {
			return Result{}, err
		}
		sort.Strings(names)
		r := Result{Columns: []Column{{Name: "name"}}, Rows: [][]any{}}
		for _, n := range names {
			r.Rows = append(r.Rows, []any{n})
		}
		return r, nil
	}
	return Result{}, fmt.Errorf("show %s is not supported, use show dbs or show collections", what)
}

func (m *mongoDB) dbCall(ctx context.Context, db *mongo.Database, call mongoCall) (Result, error) {
	switch call.name {
	case "runCommand":
		cmd, err := docArg(call.args, 0, true)
		if err != nil {
			return Result{}, err
		}
		return m.command(ctx, db, cmd)
	case "stats":
		return m.command(ctx, db, bson.D{{Key: "dbStats", Value: 1}})
	case "getCollectionNames":
		names, err := db.ListCollectionNames(ctx, bson.D{})
		if err != nil {
			return Result{}, err
		}
		sort.Strings(names)
		r := Result{Columns: []Column{{Name: "name"}}, Rows: [][]any{}}
		for _, n := range names {
			r.Rows = append(r.Rows, []any{n})
		}
		return r, nil
	case "createCollection":
		name, ok := firstString(call.args)
		if !ok {
			return Result{}, errors.New("createCollection needs a name")
		}
		return Result{Message: "created " + name}, db.CreateCollection(ctx, name)
	case "dropDatabase":
		return Result{Message: "dropped " + db.Name()}, db.Drop(ctx)
	}
	return Result{}, fmt.Errorf("db.%s() is not supported; supported: runCommand, stats, getCollectionNames, createCollection, dropDatabase", call.name)
}

func (m *mongoDB) command(ctx context.Context, db *mongo.Database, cmd any) (Result, error) {
	var doc bson.D
	if err := db.RunCommand(ctx, cmd).Decode(&doc); err != nil {
		return Result{}, err
	}
	return docsResult([]bson.D{doc}, 0), nil
}

func (m *mongoDB) collCall(ctx context.Context, coll *mongo.Collection, call mongoCall, mods []mongoCall, limit int) (Result, error) {
	a := call.args
	switch call.name {
	case "find", "findOne":
		return m.find(ctx, coll, call, mods, limit)
	case "aggregate":
		pipeline, err := pipelineArg(a)
		if err != nil {
			return Result{}, err
		}
		cur, err := coll.Aggregate(ctx, pipeline, options.Aggregate().SetBatchSize(batchSize(limit)))
		if err != nil {
			return Result{}, err
		}
		return readCursor(ctx, cur, limit)
	case "countDocuments", "count":
		filter, err := docArg(a, 0, false)
		if err != nil {
			return Result{}, err
		}
		n, err := coll.CountDocuments(ctx, filter)
		return valueResult("count", n), err
	case "estimatedDocumentCount":
		n, err := coll.EstimatedDocumentCount(ctx)
		return valueResult("count", n), err
	case "distinct":
		field, ok := argAt(a, 0).(string)
		if !ok {
			return Result{}, errors.New("distinct needs a field name")
		}
		filter, err := docArg(a, 1, false)
		if err != nil {
			return Result{}, err
		}
		var values bson.A
		if err := coll.Distinct(ctx, field, filter).Decode(&values); err != nil {
			return Result{}, err
		}
		r := Result{Columns: []Column{{Name: field}}, Rows: [][]any{}}
		for _, v := range values {
			r.Rows = append(r.Rows, []any{display(v)})
		}
		return r, nil
	case "insertOne":
		doc, err := docArg(a, 0, true)
		if err != nil {
			return Result{}, err
		}
		res, err := coll.InsertOne(ctx, doc)
		if err != nil {
			return Result{}, err
		}
		r := valueResult("insertedId", display(res.InsertedID))
		r.Affected, r.Message = 1, "inserted 1 document"
		return r, nil
	case "insertMany":
		docs, ok := argAt(a, 0).(bson.A)
		if !ok {
			return Result{}, errors.New("insertMany needs an array of documents")
		}
		res, err := coll.InsertMany(ctx, docs)
		if err != nil {
			return Result{}, err
		}
		n := int64(len(res.InsertedIDs))
		return Result{Affected: n, Message: fmt.Sprintf("inserted %d documents", n)}, nil
	case "updateOne", "updateMany", "replaceOne":
		return update(ctx, coll, call)
	case "deleteOne", "deleteMany":
		filter, err := docArg(a, 0, true)
		if err != nil {
			return Result{}, err
		}
		var res *mongo.DeleteResult
		if call.name == "deleteOne" {
			res, err = coll.DeleteOne(ctx, filter)
		} else {
			res, err = coll.DeleteMany(ctx, filter)
		}
		if err != nil {
			return Result{}, err
		}
		return Result{Affected: res.DeletedCount, Message: fmt.Sprintf("deleted %d documents", res.DeletedCount)}, nil
	case "createIndex":
		return createIndex(ctx, coll, a)
	case "dropIndex":
		name, ok := firstString(a)
		if !ok {
			return Result{}, errors.New("dropIndex needs the index name")
		}
		return Result{Message: "dropped index " + name}, coll.Indexes().DropOne(ctx, name)
	case "getIndexes":
		cur, err := coll.Indexes().List(ctx)
		if err != nil {
			return Result{}, err
		}
		return readCursor(ctx, cur, 0)
	case "drop":
		return Result{Message: "dropped " + coll.Name()}, coll.Drop(ctx)
	}
	return Result{}, fmt.Errorf("%s() is not supported; supported: find, findOne, aggregate, countDocuments, "+
		"estimatedDocumentCount, distinct, insertOne, insertMany, updateOne, updateMany, replaceOne, "+
		"deleteOne, deleteMany, createIndex, dropIndex, getIndexes, drop", call.name)
}

// find applies cursor modifiers such as .sort().limit().skip(), and never
// reads more than limit documents (plus one, to tell whether there were more).
func (m *mongoDB) find(ctx context.Context, coll *mongo.Collection, call mongoCall, mods []mongoCall, limit int) (Result, error) {
	filter, err := docArg(call.args, 0, false)
	if err != nil {
		return Result{}, err
	}
	opts := options.Find()
	if p, err := docArg(call.args, 1, false); err != nil {
		return Result{}, err
	} else if len(p) > 0 {
		opts.SetProjection(p)
	}
	userLimit := int64(0)
	if call.name == "findOne" {
		userLimit = 1
	}
	for _, mod := range mods {
		switch mod.name {
		case "sort":
			d, err := docArg(mod.args, 0, true)
			if err != nil {
				return Result{}, err
			}
			opts.SetSort(d)
		case "projection", "project":
			d, err := docArg(mod.args, 0, true)
			if err != nil {
				return Result{}, err
			}
			opts.SetProjection(d)
		case "limit":
			userLimit = toInt64(argAt(mod.args, 0))
		case "skip":
			opts.SetSkip(toInt64(argAt(mod.args, 0)))
		case "hint":
			opts.SetHint(argAt(mod.args, 0))
		case "count":
			n, err := coll.CountDocuments(ctx, filter)
			return valueResult("count", n), err
		case "toArray", "pretty":
		default:
			return Result{}, fmt.Errorf(".%s() is not supported after find; use sort, limit, skip, projection, hint or count", mod.name)
		}
	}
	effective := int64(limit)
	if userLimit > 0 && (limit <= 0 || userLimit <= int64(limit)) {
		opts.SetLimit(userLimit)
		effective = 0 // the server already stops at the user's limit
	} else if limit > 0 {
		opts.SetLimit(int64(limit) + 1)
	}
	opts.SetBatchSize(batchSize(int(max(effective, userLimit))))
	cur, err := coll.Find(ctx, filter, opts)
	if err != nil {
		return Result{}, err
	}
	r, err := readCursor(ctx, cur, int(effective))
	if err == nil {
		r.Edit = mongoEditInfo(coll, r)
	}
	return r, err
}

func update(ctx context.Context, coll *mongo.Collection, call mongoCall) (Result, error) {
	filter, err := docArg(call.args, 0, true)
	if err != nil {
		return Result{}, err
	}
	upd := argAt(call.args, 1)
	if upd == nil {
		return Result{}, fmt.Errorf("%s needs a filter and an update", call.name)
	}
	opt, err := docArg(call.args, 2, false)
	if err != nil {
		return Result{}, err
	}
	upsert := lookup(opt, "upsert") == true

	var res *mongo.UpdateResult
	switch call.name {
	case "updateOne":
		res, err = coll.UpdateOne(ctx, filter, upd, options.UpdateOne().SetUpsert(upsert))
	case "updateMany":
		res, err = coll.UpdateMany(ctx, filter, upd, options.UpdateMany().SetUpsert(upsert))
	default:
		res, err = coll.ReplaceOne(ctx, filter, upd, options.Replace().SetUpsert(upsert))
	}
	if err != nil {
		return Result{}, err
	}
	msg := fmt.Sprintf("matched %d, modified %d", res.MatchedCount, res.ModifiedCount)
	if res.UpsertedCount > 0 {
		msg += fmt.Sprintf(", upserted %v", display(res.UpsertedID))
	}
	return Result{Affected: res.ModifiedCount + res.UpsertedCount, Message: msg}, nil
}

func createIndex(ctx context.Context, coll *mongo.Collection, a []any) (Result, error) {
	keys, err := docArg(a, 0, true)
	if err != nil {
		return Result{}, err
	}
	opt, err := docArg(a, 1, false)
	if err != nil {
		return Result{}, err
	}
	io := options.Index()
	if v, ok := lookup(opt, "name").(string); ok {
		io.SetName(v)
	}
	if lookup(opt, "unique") == true {
		io.SetUnique(true)
	}
	if lookup(opt, "sparse") == true {
		io.SetSparse(true)
	}
	if v := lookup(opt, "expireAfterSeconds"); v != nil {
		io.SetExpireAfterSeconds(int32(toInt64(v)))
	}
	name, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: keys, Options: io})
	if err != nil {
		return Result{}, err
	}
	return Result{Message: "created index " + name}, nil
}

// Explain explains the first statement, which must be a find, aggregate or count.
func (m *mongoDB) Explain(ctx context.Context, database, q string, analyze bool) ([]Result, error) {
	stmts, err := parseMongo(q)
	if err != nil {
		return nil, err
	}
	if len(stmts) == 0 || stmts[0].kind != "call" || stmts[0].coll == "" {
		return nil, errors.New("explain needs db.<collection>.find(), aggregate() or countDocuments()")
	}
	st := stmts[0]
	call := st.calls[0]
	var inner bson.D
	switch call.name {
	case "find", "findOne":
		filter, err := docArg(call.args, 0, false)
		if err != nil {
			return nil, err
		}
		inner = bson.D{{Key: "find", Value: st.coll}, {Key: "filter", Value: filter}}
		for _, mod := range st.calls[1:] {
			switch mod.name {
			case "sort", "limit", "skip", "hint":
				inner = append(inner, bson.E{Key: mod.name, Value: argAt(mod.args, 0)})
			case "projection", "project":
				inner = append(inner, bson.E{Key: "projection", Value: argAt(mod.args, 0)})
			}
		}
	case "aggregate":
		pipeline, err := pipelineArg(call.args)
		if err != nil {
			return nil, err
		}
		inner = bson.D{{Key: "aggregate", Value: st.coll}, {Key: "pipeline", Value: pipeline}, {Key: "cursor", Value: bson.D{}}}
	case "countDocuments", "count":
		filter, err := docArg(call.args, 0, false)
		if err != nil {
			return nil, err
		}
		inner = bson.D{{Key: "count", Value: st.coll}, {Key: "query", Value: filter}}
	default:
		return nil, fmt.Errorf("cannot explain %s()", call.name)
	}
	verbosity := "queryPlanner"
	if analyze {
		verbosity = "executionStats"
	}
	db := m.client.Database(m.dbName(firstNonEmpty(st.db, database)))
	start := time.Now()
	r, err := m.command(ctx, db, bson.D{{Key: "explain", Value: inner}, {Key: "verbosity", Value: verbosity}})
	if err != nil {
		r = Result{Error: err.Error()}
	}
	r.Title, r.Statement, r.Millis = "Plan", st.text, time.Since(start).Milliseconds()
	return []Result{r}, nil
}

// --- results ---

// readCursor reads up to limit documents (0 means all) and closes the cursor.
func readCursor(ctx context.Context, cur *mongo.Cursor, limit int) (Result, error) {
	defer cur.Close(ctx)
	var docs []bson.D
	for cur.Next(ctx) {
		var d bson.D
		if err := cur.Decode(&d); err != nil {
			return Result{}, err
		}
		docs = append(docs, d)
		if limit > 0 && len(docs) > limit {
			break
		}
	}
	if err := cur.Err(); err != nil {
		return Result{}, err
	}
	return docsResult(docs, limit), nil
}

// docsResult turns documents into a table. Columns are the union of the
// top-level keys in first-seen order; nested values are shown as JSON.
func docsResult(docs []bson.D, limit int) Result {
	r := Result{Rows: [][]any{}, Columns: []Column{}}
	if limit > 0 && len(docs) > limit {
		docs, r.Truncated = docs[:limit], true
	}
	col := map[string]int{}
	for _, d := range docs {
		for _, e := range d {
			if _, ok := col[e.Key]; !ok {
				col[e.Key] = len(r.Columns)
				r.Columns = append(r.Columns, Column{Name: e.Key})
			}
		}
	}
	for _, d := range docs {
		row := make([]any, len(r.Columns))
		for _, e := range d {
			row[col[e.Key]] = display(e.Value)
		}
		r.Rows = append(r.Rows, row)
		b, err := bson.MarshalExtJSON(d, false, false)
		if err != nil {
			b = []byte(fmt.Sprintf("%q", err.Error()))
		}
		r.Docs = append(r.Docs, string(b))
	}
	r.Message = fmt.Sprintf("%d documents", len(docs))
	return r
}

func valueResult(name string, v any) Result {
	return Result{Columns: []Column{{Name: name}}, Rows: [][]any{{v}}}
}

// display converts a BSON value into something JSON can carry and a grid can show.
func display(v any) any {
	switch t := v.(type) {
	case nil, string, bool, int32, int64:
		return t
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return fmt.Sprint(t)
		}
		return t
	case bson.ObjectID:
		return `ObjectId("` + t.Hex() + `")`
	case bson.DateTime:
		return t.Time().UTC().Format(time.RFC3339Nano)
	}
	return valueJSON(v)
}

// valueJSON renders any BSON value as relaxed Extended JSON.
func valueJSON(v any) string {
	b, err := bson.MarshalExtJSON(bson.D{{Key: "v", Value: v}}, false, false)
	if err != nil {
		return fmt.Sprint(v)
	}
	s := string(b)
	return strings.TrimSuffix(strings.TrimPrefix(s, `{"v":`), "}")
}

func bsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "bool"
	case int32:
		return "int"
	case int64:
		return "long"
	case float64:
		return "double"
	case bson.ObjectID:
		return "objectId"
	case bson.DateTime:
		return "date"
	case bson.D:
		return "object"
	case bson.A:
		return "array"
	case bson.Decimal128:
		return "decimal"
	case bson.Binary:
		return "binData"
	}
	return fmt.Sprintf("%T", v)
}

// --- argument helpers ---

func argAt(args []any, i int) any {
	if i < len(args) {
		return args[i]
	}
	return nil
}

// docArg returns argument i as a document. A missing optional argument is an empty document.
func docArg(args []any, i int, required bool) (bson.D, error) {
	switch v := argAt(args, i).(type) {
	case bson.D:
		return v, nil
	case nil:
		if required {
			return nil, fmt.Errorf("argument %d must be a document", i+1)
		}
		return bson.D{}, nil
	default:
		return nil, fmt.Errorf("argument %d must be a document, got %s", i+1, bsonTypeName(v))
	}
}

// pipelineArg accepts aggregate([stage, ...]) and aggregate(stage, stage, ...).
func pipelineArg(args []any) (bson.A, error) {
	if len(args) == 1 {
		if a, ok := args[0].(bson.A); ok {
			return a, nil
		}
	}
	out := bson.A{}
	for i, a := range args {
		if _, ok := a.(bson.D); !ok {
			return nil, fmt.Errorf("aggregate stage %d must be a document", i+1)
		}
		out = append(out, a)
	}
	return out, nil
}

func pipelineWrites(args []any) bool {
	p, err := pipelineArg(args)
	if err != nil {
		return false
	}
	for _, stage := range p {
		if d, ok := stage.(bson.D); ok && len(d) > 0 && (d[0].Key == "$out" || d[0].Key == "$merge") {
			return true
		}
	}
	return false
}

func lookup(d bson.D, key string) any {
	for _, e := range d {
		if e.Key == key {
			return e.Value
		}
	}
	return nil
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int32:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

func batchSize(limit int) int32 {
	if limit <= 0 || limit >= 1000 {
		return 1000
	}
	return int32(limit + 1)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
