package db

import "testing"

func TestSingleTableSelect(t *testing.T) {
	cases := map[string]string{
		"select * from users": "users",
		"SELECT id, name FROM public.users u WHERE id > 3 ORDER BY id LIMIT 10;": "public.users",
		`select * from "My Table" limit 5`:                                       `"My Table"`,
		"select top 10 * from [dbo].[Orders]":                                    "[dbo].[Orders]",
		"select u.* from `shop`.`users` as u":                                    "`shop`.`users`",
		"select count(*) from users":                                             "",
		"select a.id from a join b on a.id = b.id":                               "",
		"select distinct name from users":                                        "",
		"select name from users group by name":                                   "",
		"select id, name as n from users":                                        "",
		"update users set a = 1":                                                 "",
	}
	for stmt, want := range cases {
		got, ok := SingleTableSelect(stmt)
		if (want != "") != ok || got != want {
			t.Errorf("SingleTableSelect(%q) = %q, %v; want %q", stmt, got, ok, want)
		}
	}
}

func TestBuildEdit(t *testing.T) {
	sql, args, err := buildEdit(dialectPostgres, "public.users", EditOp{Op: "update", Key: map[string]any{"id": "7"}, Values: map[string]any{"name": "Ani", "city": nil}})
	if err != nil || sql != `update public.users set "city" = $1, "name" = $2 where "id" = $3` || len(args) != 3 {
		t.Fatalf("update: %q %v %v", sql, args, err)
	}
	sql, _, _ = buildEdit(dialectMSSQL, "[dbo].[t]", EditOp{Op: "delete", Key: map[string]any{"a]b": "1"}})
	if sql != "delete from [dbo].[t] where [a]]b] = @p1" {
		t.Fatalf("delete: %q", sql)
	}
	sql, _, _ = buildEdit(dialectMySQL, "`t`", EditOp{Op: "insert", Values: map[string]any{"b": "2", "a": "1"}})
	if sql != "insert into `t` (`a`, `b`) values (?, ?)" {
		t.Fatalf("insert: %q", sql)
	}
	if _, _, err := buildEdit(dialectSQLite, "t", EditOp{Op: "delete"}); err == nil {
		t.Fatal("delete without key must fail")
	}
}

func TestEditInfoFor(t *testing.T) {
	cols := []Column{{Name: "ID"}, {Name: "name"}, {Name: "upper"}}
	info := editInfoFor("t", "", cols, []string{"id", "name"}, []string{"id"})
	if info == nil || info.Columns[0] != "id" || info.Columns[2] != "" || info.Key[0] != "id" {
		t.Fatalf("%+v", info)
	}
	if editInfoFor("t", "", cols[1:], []string{"id", "name"}, []string{"id"}) != nil {
		t.Fatal("missing key column must not be editable")
	}
}
