package db

import (
	"reflect"
	"testing"
)

func TestSplitSQL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"single without semicolon", "select 1", []string{"select 1"}},
		{"two statements", "select 1; select 2;", []string{"select 1", "select 2"}},
		{"semicolon in string", "select 'a;b'; select 2", []string{"select 'a;b'", "select 2"}},
		{"doubled quote", "select 'it''s; fine'", []string{"select 'it''s; fine'"}},
		{"E string with backslash quote", `select E'a\';b'; select 2`, []string{`select E'a\';b'`, "select 2"}},
		{"quoted identifier", `select 1 as "a;b"`, []string{`select 1 as "a;b"`}},
		{"line comment", "select 1 -- ; not a split\n; select 2", []string{"select 1 -- ; not a split", "select 2"}},
		{"nested block comment", "/* a /* ; */ ; */ select 1", []string{"/* a /* ; */ ; */ select 1"}},
		{"comment only statement dropped", "select 1; -- trailing\n", []string{"select 1"}},
		{"empty statements dropped", ";;select 1;;", []string{"select 1"}},
		{
			"dollar quoted body",
			"create function f() returns int as $$ begin return 1; end $$ language plpgsql; select f()",
			[]string{"create function f() returns int as $$ begin return 1; end $$ language plpgsql", "select f()"},
		},
		{"tagged dollar quote", "select $x$ ; $$ ; $x$; select 2", []string{"select $x$ ; $$ ; $x$", "select 2"}},
		{"positional parameter is not a dollar quote", "select $1; select 2", []string{"select $1", "select 2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SplitSQL(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SplitSQL(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFirstKeyword(t *testing.T) {
	tests := map[string]string{
		"select 1":                    "select",
		"  -- note\n  WITH x as (..)": "with",
		"/* c */ (select 1)":          "select",
		"insert into t values (1)":    "insert",
		"":                            "",
	}
	for in, want := range tests {
		if got := firstKeyword(in); got != want {
			t.Errorf("firstKeyword(%q) = %q, want %q", in, got, want)
		}
	}
}
