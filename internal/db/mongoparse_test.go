package db

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestParseMongoChain(t *testing.T) {
	stmts, err := parseMongo(`db.users.find({ age: { $gt: 30 }, 'name': "Ann" }, {_id: 0}).sort({name: 1, age: -1}).limit(5)`)
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) != 1 {
		t.Fatalf("got %d statements", len(stmts))
	}
	st := stmts[0]
	if st.coll != "users" || len(st.calls) != 3 {
		t.Fatalf("coll=%q calls=%d", st.coll, len(st.calls))
	}
	wantFilter := bson.D{
		{Key: "age", Value: bson.D{{Key: "$gt", Value: int32(30)}}},
		{Key: "name", Value: "Ann"},
	}
	if !reflect.DeepEqual(st.calls[0].args[0], wantFilter) {
		t.Errorf("filter = %#v", st.calls[0].args[0])
	}
	if got := st.calls[1].args[0].(bson.D); got[0].Key != "name" || got[1].Key != "age" {
		t.Errorf("sort keys out of order: %#v", got)
	}
	if st.calls[2].name != "limit" || st.calls[2].args[0] != int32(5) {
		t.Errorf("limit call = %#v", st.calls[2])
	}
}

func TestParseMongoStatements(t *testing.T) {
	script := `
		show collections
		use shop;
		// comment between statements
		db.getCollection("order items").countDocuments({})
		db.getSiblingDB('logs').events.find()
		db.runCommand({ ping: 1 })
		db.a.b.find() /* dotted collection */
	`
	stmts, err := parseMongo(script)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(stmts))
	for i, s := range stmts {
		got[i] = strings.Join([]string{s.kind, s.arg, s.db, s.coll}, "|")
	}
	want := []string{
		"show|collections||",
		"use|shop||",
		"call|||order items",
		"call||logs|events",
		"call|||",
		"call|||a.b",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestParseMongoValues(t *testing.T) {
	stmts, err := parseMongo(`db.c.insertOne({
		id: ObjectId("65a1b2c3d4e5f60718293a4b"),
		at: ISODate("2026-01-31T10:00:00Z"),
		big: NumberLong("9007199254740993"),
		dec: NumberDecimal("1.10"),
		f: 1.5, neg: -2, exp: 1e3, nul: null, yes: true,
		list: [1, 'two', [3],],
		re: /^ab[/]c/i,
		esc: "line\nnext \"q\" é",
	})`)
	if err != nil {
		t.Fatal(err)
	}
	doc := stmts[0].calls[0].args[0].(bson.D)
	get := func(k string) any { return lookup(doc, k) }

	oid, _ := bson.ObjectIDFromHex("65a1b2c3d4e5f60718293a4b")
	checks := map[string]any{
		"id":  oid,
		"at":  bson.NewDateTimeFromTime(time.Date(2026, 1, 31, 10, 0, 0, 0, time.UTC)),
		"big": int64(9007199254740993),
		"f":   1.5,
		"neg": int32(-2),
		"exp": 1000.0,
		"nul": nil,
		"yes": true,
		"re":  bson.Regex{Pattern: "^ab[/]c", Options: "i"},
		"esc": "line\nnext \"q\" é",
	}
	for k, want := range checks {
		if got := get(k); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v, want %#v", k, got, want)
		}
	}
	if list := get("list").(bson.A); len(list) != 3 || list[1] != "two" {
		t.Errorf("list = %#v", list)
	}
	if _, ok := get("dec").(bson.Decimal128); !ok {
		t.Errorf("dec = %T", get("dec"))
	}
}

func TestParseMongoErrors(t *testing.T) {
	for _, in := range []string{
		"select * from users",
		"db.users",
		"db.users.find({a: })",
		"db.users.find({a: 1}",
		`db.users.find({a: "open})`,
		"db.users.find({a: someVar})",
		"db.users.find().limit",
	} {
		if _, err := parseMongo(in); err == nil {
			t.Errorf("parseMongo(%q) succeeded, want an error", in)
		}
	}
}
