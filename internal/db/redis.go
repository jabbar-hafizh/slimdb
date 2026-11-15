package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const redisTreeKeys = 1000 // keys listed per database in the tree

type redisDB struct {
	opt      *redis.Options
	readOnly bool

	mu      sync.Mutex
	clients map[int]*redis.Client // one per logical database
}

// openRedis takes redis://[user:pass@]host:6379[/db] or rediss:// for TLS.
// readOnly refuses commands that write.
func openRedis(ctx context.Context, raw string, readOnly bool) (*redisDB, error) {
	opt, err := redis.ParseURL(raw)
	if err != nil {
		return nil, err
	}
	opt.PoolSize = 4
	r := &redisDB{opt: opt, readOnly: readOnly, clients: map[int]*redis.Client{}}
	if err := r.client(opt.DB).Ping(ctx).Err(); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

func (r *redisDB) client(n int) *redis.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.clients[n]; ok {
		return c
	}
	o := *r.opt
	o.DB = n
	c := redis.NewClient(&o)
	r.clients[n] = c
	return c
}

func (r *redisDB) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for n, c := range r.clients {
		_ = c.Close()
		delete(r.clients, n)
	}
}

// dbIndex reads "3" or "db3"; empty means the database from the URL.
func (r *redisDB) dbIndex(database string) (int, error) {
	if database == "" {
		return r.opt.DB, nil
	}
	n, err := strconv.Atoi(strings.TrimPrefix(strings.ToLower(database), "db"))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad Redis database %q, use a number such as 0", database)
	}
	return n, nil
}

// Tree lists every non-empty database with up to redisTreeKeys keys each.
// A key's node kind is its Redis type: string, list, set, zset, hash, stream.
func (r *redisDB) Tree(ctx context.Context) ([]Node, error) {
	info, err := r.client(r.opt.DB).Info(ctx, "keyspace").Result()
	if err != nil {
		return nil, err
	}
	var dbs []int
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		if name, _, ok := strings.Cut(line, ":"); ok && strings.HasPrefix(name, "db") {
			if n, err := strconv.Atoi(name[2:]); err == nil {
				dbs = append(dbs, n)
			}
		}
	}
	if !slices.Contains(dbs, r.opt.DB) {
		dbs = append(dbs, r.opt.DB)
	}
	slices.Sort(dbs)
	var out []Node
	for _, n := range dbs {
		c := r.client(n)
		var keys []string
		var cursor uint64
		for {
			batch, next, err := c.Scan(ctx, cursor, "*", 500).Result()
			if err != nil {
				return nil, err
			}
			keys = append(keys, batch...)
			cursor = next
			if cursor == 0 || len(keys) >= redisTreeKeys {
				break
			}
		}
		if len(keys) > redisTreeKeys {
			keys = keys[:redisTreeKeys]
		}
		slices.Sort(keys)
		pipe := c.Pipeline()
		types := make([]*redis.StatusCmd, len(keys))
		for i, k := range keys {
			types[i] = pipe.Type(ctx, k)
		}
		if len(keys) > 0 {
			if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
				return nil, err
			}
		}
		node := Node{Name: "db" + strconv.Itoa(n), Kind: "database"}
		for i, k := range keys {
			node.Children = append(node.Children, Node{Name: k, Kind: types[i].Val(), Object: k, Database: strconv.Itoa(n)})
		}
		out = append(out, node)
	}
	return out, nil
}

func (r *redisDB) Describe(ctx context.Context, database, key string) ([]Result, error) {
	n, err := r.dbIndex(database)
	if err != nil {
		return nil, err
	}
	c := r.client(n)
	typ, err := c.Type(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if typ == "none" {
		return nil, fmt.Errorf("key %q not found in db%d", key, n)
	}
	info := Result{Title: "Key", Columns: []Column{{Name: "property"}, {Name: "value"}}}
	add := func(k string, v any) { info.Rows = append(info.Rows, []any{k, v}) }
	add("key", key)
	add("type", typ)
	ttl, _ := c.TTL(ctx, key).Result()
	switch {
	case ttl == -1:
		add("ttl", "no expiry")
	case ttl > 0:
		add("ttl", ttl.Round(time.Second).String())
	}
	if mem, err := c.MemoryUsage(ctx, key).Result(); err == nil {
		add("memory", fmt.Sprintf("%d bytes", mem))
	}

	const max = 1000
	val := Result{Title: "Value", Rows: [][]any{}}
	switch typ {
	case "string":
		s, err := c.Get(ctx, key).Result()
		if err != nil {
			return nil, err
		}
		val.Columns = []Column{{Name: "value"}}
		val.Rows = append(val.Rows, []any{s})
	case "list":
		l, _ := c.LLen(ctx, key).Result()
		add("length", l)
		items, err := c.LRange(ctx, key, 0, max-1).Result()
		if err != nil {
			return nil, err
		}
		val.Columns = []Column{{Name: "index"}, {Name: "value"}}
		for i, v := range items {
			val.Rows = append(val.Rows, []any{strconv.Itoa(i), v})
		}
		val.Truncated = l > max
	case "set":
		l, _ := c.SCard(ctx, key).Result()
		add("length", l)
		items, _, err := c.SScan(ctx, key, 0, "*", max).Result()
		if err != nil {
			return nil, err
		}
		slices.Sort(items)
		val.Columns = []Column{{Name: "member"}}
		for _, v := range items {
			val.Rows = append(val.Rows, []any{v})
		}
		val.Truncated = l > int64(len(items))
	case "zset":
		l, _ := c.ZCard(ctx, key).Result()
		add("length", l)
		items, err := c.ZRangeWithScores(ctx, key, 0, max-1).Result()
		if err != nil {
			return nil, err
		}
		val.Columns = []Column{{Name: "member"}, {Name: "score"}}
		for _, z := range items {
			val.Rows = append(val.Rows, []any{fmt.Sprint(z.Member), strconv.FormatFloat(z.Score, 'g', -1, 64)})
		}
		val.Truncated = l > max
	case "hash":
		l, _ := c.HLen(ctx, key).Result()
		add("length", l)
		m, err := c.HGetAll(ctx, key).Result()
		if err != nil {
			return nil, err
		}
		val.Columns = []Column{{Name: "field"}, {Name: "value"}}
		for _, f := range sortedStrKeys(m) {
			val.Rows = append(val.Rows, []any{f, m[f]})
		}
	case "stream":
		l, _ := c.XLen(ctx, key).Result()
		add("length", l)
		msgs, err := c.XRangeN(ctx, key, "-", "+", 100).Result()
		if err != nil {
			return nil, err
		}
		val.Columns = []Column{{Name: "id"}, {Name: "fields"}}
		for _, m := range msgs {
			b, _ := json.Marshal(m.Values)
			val.Rows = append(val.Rows, []any{m.ID, string(b)})
		}
		val.Truncated = l > 100
	default:
		return []Result{info}, nil
	}
	return []Result{info, val}, nil
}

// redisWrites are commands refused on a read-only connection.
var redisWrites = map[string]bool{}

func init() {
	for _, c := range strings.Fields(`set setnx setex psetex mset msetnx getset getdel getex append incr incrby incrbyfloat
		decr decrby setrange del unlink expire expireat pexpire pexpireat persist rename renamenx move copy restore
		lpush lpushx rpush rpushx lpop rpop lset lrem ltrim linsert lmove blmove rpoplpush brpoplpush blpop brpop lmpop blmpop
		sadd srem spop smove sinterstore sunionstore sdiffstore
		zadd zincrby zrem zremrangebyrank zremrangebyscore zremrangebylex zpopmin zpopmax bzpopmin bzpopmax zmpop bzmpop
		zunionstore zinterstore zdiffstore zrangestore
		hset hsetnx hmset hdel hincrby hincrbyfloat hexpire hpexpire hpersist hgetdel hgetex hsetex
		xadd xdel xtrim xgroup xack xclaim xautoclaim xsetid
		pfadd pfmerge geoadd georadius georadiusbymember geosearchstore setbit bitop bitfield
		json.set json.del json.forget json.arrappend json.arrinsert json.arrpop json.arrtrim json.clear
		json.merge json.mset json.numincrby json.strappend json.toggle
		flushdb flushall swapdb eval evalsha eval_ro fcall function script config shutdown save bgsave bgrewriteaof
		replicaof slaveof debug migrate publish spublish`) {
		redisWrites[c] = true
	}
	redisWrites["eval_ro"] = false
}

var redisUnsupported = map[string]string{
	"subscribe": "subscribing", "psubscribe": "subscribing", "ssubscribe": "subscribing", "monitor": "MONITOR",
	"multi": "MULTI/EXEC", "exec": "MULTI/EXEC", "watch": "WATCH",
}

// Query runs one command per line on one connection. Lines starting with #
// or // are comments.
func (r *redisDB) Query(ctx context.Context, database, q string, limit int) ([]Result, error) {
	n, err := r.dbIndex(database)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, l := range strings.Split(q, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") && !strings.HasPrefix(l, "//") {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return nil, errors.New("nothing to run")
	}
	conn := r.client(n).Conn()
	defer conn.Close()

	var out []Result
	for _, line := range lines {
		res := r.runLine(ctx, conn, line, limit)
		out = append(out, res)
		if res.Error != "" {
			break
		}
	}
	return out, nil
}

func (r *redisDB) runLine(ctx context.Context, conn *redis.Conn, line string, limit int) Result {
	res := Result{Statement: line}
	start := time.Now()
	defer func() { res.Millis = time.Since(start).Milliseconds() }()
	args, err := splitCommand(line)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	name := strings.ToLower(args[0])
	if what, ok := redisUnsupported[name]; ok {
		res.Error = what + " is not supported here"
		return res
	}
	if r.readOnly && redisWrites[name] {
		res.Error = errReadOnlyStmt.Error()
		return res
	}
	cmdArgs := make([]any, len(args))
	for i, a := range args {
		cmdArgs[i] = a
	}
	v, err := conn.Do(ctx, cmdArgs...).Result()
	switch {
	case errors.Is(err, redis.Nil):
		v = nil
	case err != nil:
		res.Error = err.Error()
		return res
	}
	shapeRedis(name, v, limit, &res)
	return res
}

// shapeRedis turns a reply into a table.
func shapeRedis(cmd string, v any, limit int, res *Result) {
	add := func(row ...any) bool {
		if limit > 0 && len(res.Rows) >= limit {
			res.Truncated = true
			return false
		}
		res.Rows = append(res.Rows, row)
		return true
	}
	res.Rows = [][]any{}
	switch v := v.(type) {
	case map[any]any:
		res.Columns = []Column{{Name: "field"}, {Name: "value"}}
		keys := make([]string, 0, len(v))
		byKey := map[string]any{}
		for k, val := range v {
			ks := redisText(k).(string)
			keys = append(keys, ks)
			byKey[ks] = val
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !add(k, redisText(byKey[k])) {
				break
			}
		}
	case []any:
		if (cmd == "scan" || cmd == "sscan" || cmd == "hscan" || cmd == "zscan") && len(v) == 2 {
			res.Message = "next cursor " + fmt.Sprint(redisText(v[0])) + " (0 means done)"
			items, _ := v[1].([]any)
			if cmd == "hscan" || cmd == "zscan" {
				res.Columns = []Column{{Name: "field"}, {Name: "value"}}
				for i := 0; i+1 < len(items); i += 2 {
					if !add(redisText(items[i]), redisText(items[i+1])) {
						break
					}
				}
				return
			}
			res.Columns = []Column{{Name: "value"}}
			for _, it := range items {
				if !add(redisText(it)) {
					break
				}
			}
			return
		}
		// An array of equal-length scalar arrays, such as ZRANGE WITHSCORES on RESP3.
		if width := tupleWidth(v); width > 0 {
			res.Columns = []Column{{Name: "#"}}
			for i := 1; i <= width; i++ {
				res.Columns = append(res.Columns, Column{Name: "value " + strconv.Itoa(i)})
			}
			for i, it := range v {
				row := []any{strconv.Itoa(i + 1)}
				for _, x := range it.([]any) {
					row = append(row, redisText(x))
				}
				if !add(row...) {
					break
				}
			}
			return
		}
		res.Columns = []Column{{Name: "#"}, {Name: "value"}}
		for i, it := range v {
			if !add(strconv.Itoa(i+1), redisText(it)) {
				break
			}
		}
	case string:
		if cmd == "info" {
			res.Columns = []Column{{Name: "key"}, {Name: "value"}}
			for _, l := range strings.Split(v, "\n") {
				l = strings.TrimSpace(l)
				if k, val, ok := strings.Cut(l, ":"); ok && !strings.HasPrefix(l, "#") {
					add(k, val)
				}
			}
			return
		}
		res.Columns = []Column{{Name: "value"}}
		add(v)
	default:
		res.Columns = []Column{{Name: "value"}}
		add(redisText(v))
	}
}

func tupleWidth(v []any) int {
	width := 0
	for _, it := range v {
		a, ok := it.([]any)
		if !ok || len(a) == 0 || (width != 0 && len(a) != width) {
			return 0
		}
		for _, x := range a {
			switch x.(type) {
			case []any, map[any]any:
				return 0
			}
		}
		width = len(a)
	}
	return width
}

// redisText renders a reply value as text; nested replies become JSON.
func redisText(v any) any {
	switch v := v.(type) {
	case nil:
		return nil
	case string:
		return v
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	case []any, map[any]any:
		b, err := json.Marshal(jsonable(v))
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(b)
	}
	return fmt.Sprint(v)
}

func jsonable(v any) any {
	switch v := v.(type) {
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = jsonable(x)
		}
		return out
	case map[any]any:
		out := map[string]any{}
		for k, x := range v {
			out[fmt.Sprint(k)] = jsonable(x)
		}
		return out
	}
	return v
}

// splitCommand splits a line like the redis-cli does: on spaces, with
// "double" (backslash escapes) and 'single' quoted arguments.
func splitCommand(line string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inArg := false
	r := []rune(line)
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case c == ' ' || c == '\t':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		case c == '"' || c == '\'':
			inArg = true
			q := c
			closed := false
			for i++; i < len(r); i++ {
				if r[i] == '\\' && i+1 < len(r) && (q == '"' || r[i+1] == '\'') {
					i++
					switch r[i] {
					case 'n':
						cur.WriteRune('\n')
					case 't':
						cur.WriteRune('\t')
					case 'r':
						cur.WriteRune('\r')
					default:
						cur.WriteRune(r[i])
					}
					continue
				}
				if r[i] == q {
					closed = true
					break
				}
				cur.WriteRune(r[i])
			}
			if !closed {
				return nil, fmt.Errorf("unbalanced quotes in %q", line)
			}
		default:
			inArg = true
			cur.WriteRune(c)
		}
	}
	if inArg {
		args = append(args, cur.String())
	}
	if len(args) == 0 {
		return nil, errors.New("empty command")
	}
	return args, nil
}

func (r *redisDB) Explain(context.Context, string, string, bool) ([]Result, error) {
	return []Result{{Title: "Plan", Error: "explain is not available for Redis"}}, nil
}

func sortedStrKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
