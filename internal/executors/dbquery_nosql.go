package executors

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// Redis and Mongo replies are returned as one JSON cell per statement.
var replyColumn = []*agentv1.DbColumn{{Name: "reply", Type: "json"}}

// Larger replies are refused, not cut: half a hash is not a hash.
const dbMaxReplyRead = 32 << 20

func jsonReplySet(command string, reply []byte, start time.Time) *agentv1.DbResultSet {
	return &agentv1.DbResultSet{
		Columns:    replyColumn,
		Rows:       []*agentv1.DbRow{{Cells: []*agentv1.DbCell{{Text: string(reply)}}}},
		Command:    command,
		DurationMs: uint32(time.Since(start).Milliseconds()),
	}
}

// Redis

type respError string

type respConn struct {
	conn net.Conn
	r    *bufio.Reader
	read int
}

func (c *respConn) do(ctx context.Context, argv []string) (any, error) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetDeadline(deadline)
	}
	var b strings.Builder
	// Length-prefixed strings: no argument can inject a second command.
	fmt.Fprintf(&b, "*%d\r\n", len(argv))
	for _, a := range argv {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := io.WriteString(c.conn, b.String()); err != nil {
		return nil, err
	}
	c.read = 0
	return c.reply()
}

func (c *respConn) line() (string, error) {
	s, err := c.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	c.read += len(s)
	return strings.TrimSuffix(s, "\r\n"), nil
}

func (c *respConn) reply() (any, error) {
	if c.read > dbMaxReplyRead {
		return nil, fmt.Errorf("reply is larger than %d MB; narrow the command", dbMaxReplyRead>>20)
	}
	l, err := c.line()
	if err != nil {
		return nil, err
	}
	if l == "" {
		return nil, errors.New("empty reply from redis")
	}
	switch l[0] {
	case '+':
		return l[1:], nil
	case '-':
		return respError(l[1:]), nil
	case ':':
		return strconv.ParseInt(l[1:], 10, 64)
	case '$':
		n, err := strconv.Atoi(l[1:])
		if err != nil || n < 0 {
			return nil, err
		}
		if c.read+n > dbMaxReplyRead {
			return nil, fmt.Errorf("reply is larger than %d MB; narrow the command", dbMaxReplyRead>>20)
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(c.r, buf); err != nil {
			return nil, err
		}
		c.read += n + 2
		return buf[:n], nil
	case '*':
		n, err := strconv.Atoi(l[1:])
		if err != nil || n < 0 {
			return nil, err
		}
		// n comes from the server, so cap the preallocation.
		out := make([]any, 0, min(n, 1024))
		for i := 0; i < n; i++ {
			v, err := c.reply()
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	return nil, fmt.Errorf("unexpected redis reply %q", l[:1])
}

// respJSON keeps nil vs "", errors and non-UTF-8 values distinct.
func respJSON(v any) any {
	switch t := v.(type) {
	case []byte:
		if utf8.Valid(t) {
			return string(t)
		}
		return map[string]string{"$bin": hex.EncodeToString(t)}
	case respError:
		return map[string]string{"$err": string(t)}
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = respJSON(e)
		}
		return out
	default:
		return t
	}
}

// Read-only commands. Redis has no read-only session, so anything unlisted is refused.
var redisReadCommands = map[string]bool{
	"GET": true, "MGET": true, "STRLEN": true, "GETRANGE": true, "EXISTS": true, "TYPE": true,
	"TTL": true, "PTTL": true, "EXPIRETIME": true, "SCAN": true, "KEYS": true, "RANDOMKEY": true,
	"HGET": true, "HMGET": true, "HGETALL": true, "HKEYS": true, "HVALS": true, "HLEN": true,
	"HSCAN": true, "HEXISTS": true, "HSTRLEN": true, "HRANDFIELD": true,
	"LRANGE": true, "LLEN": true, "LINDEX": true, "LPOS": true,
	"SMEMBERS": true, "SCARD": true, "SSCAN": true, "SISMEMBER": true, "SMISMEMBER": true, "SRANDMEMBER": true,
	"SINTER": true, "SUNION": true, "SDIFF": true,
	"ZRANGE": true, "ZRANGEBYSCORE": true, "ZRANGEBYLEX": true, "ZREVRANGE": true, "ZREVRANGEBYSCORE": true,
	"ZCARD": true, "ZSCORE": true, "ZMSCORE": true, "ZSCAN": true, "ZRANK": true, "ZREVRANK": true, "ZCOUNT": true,
	"XRANGE": true, "XREVRANGE": true, "XLEN": true, "XINFO": true, "XPENDING": true,
	"PFCOUNT": true, "GETBIT": true, "BITCOUNT": true, "BITPOS": true, "GEOPOS": true, "GEODIST": true,
	"INFO": true, "DBSIZE": true, "PING": true, "ECHO": true, "TIME": true, "LASTSAVE": true,
	"SELECT": true, "DUMP": true,
}

var redisReadSubcommands = map[string]map[string]bool{
	"CONFIG":  {"GET": true},
	"MEMORY":  {"USAGE": true, "STATS": true, "DOCTOR": true},
	"OBJECT":  {"ENCODING": true, "IDLETIME": true, "FREQ": true, "REFCOUNT": true},
	"CLIENT":  {"LIST": true, "INFO": true, "GETNAME": true, "ID": true},
	"COMMAND": {"COUNT": true, "DOCS": true, "INFO": true, "LIST": true},
	"SLOWLOG": {"GET": true, "LEN": true},
}

func redisReadOnly(argv []string) bool {
	name := strings.ToUpper(argv[0])
	if redisReadCommands[name] {
		return true
	}
	subs, ok := redisReadSubcommands[name]
	if !ok || len(argv) < 2 || !subs[strings.ToUpper(argv[1])] {
		return false
	}
	// CONFIG GET could expose requirepass, so only exact, harmless names are allowed.
	if name == "CONFIG" {
		for _, param := range argv[2:] {
			p := strings.ToLower(param)
			if strings.ContainsAny(p, "*?[") || strings.Contains(p, "pass") || strings.Contains(p, "auth") ||
				strings.Contains(p, "acl") || strings.Contains(p, "tls") {
				return false
			}
		}
	}
	return true
}

func runRedis(ctx context.Context, t dbTarget, cmd *agentv1.DbQuery, res *agentv1.DbQueryResult) error {
	var dialer net.Dialer
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, err := dialer.DialContext(dialCtx, "tcp", t.addr)
	cancel()
	if err != nil {
		return err
	}
	defer conn.Close()
	c := &respConn{conn: conn, r: bufio.NewReader(conn)}

	// Setup failures drop the cached target.
	setup := func(argv ...string) error {
		v, err := c.do(ctx, argv)
		if err != nil {
			return err
		}
		if e, ok := v.(respError); ok {
			return fmt.Errorf("%s: %s", argv[0], string(e))
		}
		return nil
	}
	if t.password != "" {
		if err := setup("AUTH", t.password); err != nil {
			return err
		}
	}
	if db := cmd.GetDatabase(); db != "" && db != "0" {
		// A bad db index is user input, so the cache survives.
		if err := setup("SELECT", db); err != nil {
			res.Error = err.Error()
			return nil
		}
	}

	budget := 0
	for i, st := range cmd.GetStatements() {
		start := time.Now()
		argv := make([]string, 0, len(st.GetParams()))
		for _, p := range st.GetParams() {
			argv = append(argv, p.GetText())
		}
		if len(argv) == 0 {
			argv = strings.Fields(st.GetSql())
		}
		if len(argv) == 0 {
			continue
		}
		fail := func(msg string) error {
			res.ErrorStatement = uint32(i)
			res.Error = msg
			return nil
		}
		if cmd.GetReadOnly() && !redisReadOnly(argv) {
			return fail(fmt.Sprintf("%s is not allowed in read-only mode", strings.ToUpper(argv[0])))
		}
		v, err := c.do(ctx, argv)
		if err != nil {
			return fail(err.Error())
		}
		reply, err := json.Marshal(respJSON(v))
		if err != nil {
			return fail(err.Error())
		}
		budget += len(reply)
		if budget > dbMaxResultBytes {
			return fail(fmt.Sprintf("the replies add up to more than %d MB; narrow the commands", dbMaxResultBytes>>20))
		}
		res.Results = append(res.Results, jsonReplySet(strings.ToUpper(argv[0]), reply, start))
	}
	return nil
}

// MongoDB

var mongoReadCommands = map[string]bool{
	"find": true, "aggregate": true, "count": true, "distinct": true, "listcollections": true,
	"listdatabases": true, "listindexes": true, "collstats": true, "dbstats": true, "ping": true,
	"buildinfo": true, "serverstatus": true, "hostinfo": true, "connectionstatus": true, "explain": true,
}

// $out and $merge write, so aggregate is read-only only without them.
func mongoReadOnly(doc bson.D) bool {
	if len(doc) == 0 || !mongoReadCommands[strings.ToLower(doc[0].Key)] {
		return false
	}
	// system.users and system.keys hold credentials; a $lookup could reach them too.
	if raw, err := bson.MarshalExtJSON(doc, false, false); err != nil || strings.Contains(string(raw), "system.") {
		return false
	}
	if strings.ToLower(doc[0].Key) != "aggregate" {
		return true
	}
	for _, e := range doc {
		if e.Key != "pipeline" {
			continue
		}
		stages, _ := e.Value.(bson.A)
		for _, stage := range stages {
			if d, ok := stage.(bson.D); ok && len(d) > 0 && (d[0].Key == "$out" || d[0].Key == "$merge") {
				return false
			}
		}
	}
	return true
}

func runMongo(ctx context.Context, t dbTarget, cmd *agentv1.DbQuery, res *agentv1.DbQueryResult) error {
	uri := (&url.URL{
		Scheme:   "mongodb",
		User:     url.UserPassword(t.user, t.password),
		Host:     t.addr,
		Path:     "/",
		RawQuery: "authSource=admin&directConnection=true&appName=ownship-studio",
	}).String()
	client, err := mongo.Connect(options.Client().ApplyURI(uri).
		SetConnectTimeout(10 * time.Second).
		SetServerSelectionTimeout(10 * time.Second))
	if err != nil {
		return err
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	if err := client.Ping(ctx, nil); err != nil {
		return err
	}

	database := cmd.GetDatabase()
	if database == "" {
		database = t.database
	}
	db := client.Database(database)
	budget := 0
	for i, st := range cmd.GetStatements() {
		start := time.Now()
		fail := func(msg string) error {
			res.ErrorStatement = uint32(i)
			res.Error = msg
			return nil
		}
		var doc bson.D
		if err := bson.UnmarshalExtJSON([]byte(st.GetSql()), false, &doc); err != nil {
			return fail("not a command document: " + err.Error())
		}
		if len(doc) == 0 {
			return fail("empty command document")
		}
		if cmd.GetReadOnly() && !mongoReadOnly(doc) {
			return fail(fmt.Sprintf("%s is not allowed in read-only mode", doc[0].Key))
		}
		raw, err := db.RunCommand(ctx, doc).Raw()
		if err != nil {
			return fail(err.Error())
		}
		reply, err := bson.MarshalExtJSON(raw, false, false)
		if err != nil {
			return fail(err.Error())
		}
		budget += len(reply)
		if budget > dbMaxResultBytes {
			return fail(fmt.Sprintf("the replies add up to more than %d MB; narrow the query", dbMaxResultBytes>>20))
		}
		res.Results = append(res.Results, jsonReplySet(doc[0].Key, reply, start))
	}
	return nil
}
