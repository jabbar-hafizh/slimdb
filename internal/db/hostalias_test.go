package db

import "testing"

func TestRewriteLocalhost(t *testing.T) {
	const alias = "host.docker.internal"
	tests := map[string]string{
		"postgres://u:p@localhost:5432/app":                 "postgres://u:p@host.docker.internal:5432/app",
		"postgres://u:p@127.0.0.1/app?sslmode=disable":      "postgres://u:p@host.docker.internal/app?sslmode=disable",
		"postgresql://LOCALHOST":                            "postgresql://host.docker.internal",
		"postgres://u:p%40ss@db.internal:5432/localhost":    "postgres://u:p%40ss@db.internal:5432/localhost",
		"mongodb://localhost:27017,[::1]:27018,other:27019": "mongodb://host.docker.internal:27017,host.docker.internal:27018,other:27019",
		"mongodb://user:localhost@remote/db":                "mongodb://user:localhost@remote/db",
		"postgres:///app?host=/tmp":                         "postgres:///app?host=/tmp",
	}
	for in, want := range tests {
		if got := rewriteLocalhost(in, alias); got != want {
			t.Errorf("rewriteLocalhost(%q)\n got %q\nwant %q", in, got, want)
		}
	}
	if got := rewriteLocalhost("postgres://localhost/app", ""); got != "postgres://localhost/app" {
		t.Errorf("empty alias changed the URL: %q", got)
	}
}
