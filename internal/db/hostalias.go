package db

import "strings"

// rewriteLocalhost replaces localhost hosts in a connection URL with alias.
//
// Inside a container, localhost is the container itself. Setting
// SLIMDB_LOCALHOST_ALIAS=host.docker.internal lets the same saved connection
// reach a database on the host machine. Other hosts, user info, the path and
// the query string are left as they are; Mongo's comma-separated host lists
// are handled host by host.
func rewriteLocalhost(raw, alias string) string {
	if alias == "" {
		return raw
	}
	i := strings.Index(raw, "://")
	if i < 0 {
		return raw
	}
	rest := raw[i+3:]
	end := strings.IndexAny(rest, "/?")
	if end < 0 {
		end = len(rest)
	}
	authority := rest[:end]
	userinfo := ""
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		userinfo, authority = authority[:at+1], authority[at+1:]
	}

	hosts := strings.Split(authority, ",")
	for k, h := range hosts {
		name, port := h, ""
		if strings.HasPrefix(h, "[") {
			if j := strings.Index(h, "]"); j >= 0 {
				name, port = h[:j+1], h[j+1:]
			}
		} else if j := strings.LastIndex(h, ":"); j >= 0 {
			name, port = h[:j], h[j:]
		}
		switch strings.ToLower(name) {
		case "localhost", "127.0.0.1", "[::1]":
			hosts[k] = alias + port
		}
	}
	return raw[:i+3] + userinfo + strings.Join(hosts, ",") + rest[end:]
}
