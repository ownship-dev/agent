package executors

import (
	"bufio"
	"encoding/json"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRespReplyAndJSON(t *testing.T) {
	raw := "*5\r\n$3\r\nabc\r\n$-1\r\n:42\r\n-WRONGTYPE nope\r\n$2\r\n\xff\x00\r\n"
	c := &respConn{r: bufio.NewReader(strings.NewReader(raw))}
	v, err := c.reply()
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(respJSON(v))
	if string(out) != `["abc",null,42,{"$err":"WRONGTYPE nope"},{"$bin":"ff00"}]` {
		t.Fatalf("got %s", out)
	}
}

func TestRedisReadOnly(t *testing.T) {
	for argv, want := range map[string]bool{
		"GET k": true, "hgetall k": true, "SCAN 0 MATCH *": true, "CONFIG GET databases": true,
		"CONFIG SET dir /": false, "SET k v": false, "DEL k": false, "FLUSHALL": false,
		"EVAL x 0": false, "MEMORY USAGE k": true, "MEMORY PURGE": false,
		"CONFIG GET requirepass": false, "CONFIG GET *": false, "CONFIG GET masterauth": false,
		"CONFIG GET maxmemory": true,
	} {
		if got := redisReadOnly(strings.Fields(argv)); got != want {
			t.Errorf("%s: got %v", argv, got)
		}
	}
}

func TestMongoReadOnly(t *testing.T) {
	cases := map[string]bool{
		`{"find": "users", "filter": {}}`:                                true,
		`{"aggregate": "u", "pipeline": [{"$match": {}}], "cursor": {}}`: true,
		`{"aggregate": "u", "pipeline": [{"$out": "x"}], "cursor": {}}`:  false,
		`{"insert": "u", "documents": [{}]}`:                             false,
		`{"dropDatabase": 1}`:                                            false,
		`{"listCollections": 1}`:                                         true,
		`{"find": "system.users"}`:                                       false,
		`{"aggregate": "u", "pipeline": [{"$lookup": {"from": "system.users", "as": "x"}}], "cursor": {}}`: false,
	}
	for src, want := range cases {
		var d bson.D
		if err := bson.UnmarshalExtJSON([]byte(src), false, &d); err != nil {
			t.Fatal(err)
		}
		if got := mongoReadOnly(d); got != want {
			t.Errorf("%s: got %v", src, got)
		}
	}
}
