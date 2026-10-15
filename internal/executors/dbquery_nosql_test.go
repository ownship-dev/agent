package executors

import (
	"bufio"
	"bytes"
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

func TestTypedExtJSONKeepsNumericTypes(t *testing.T) {
	raw, err := bson.Marshal(bson.D{
		{Key: "_id", Value: bson.ObjectID{1}},
		{Key: "i32", Value: int32(7)},
		{Key: "i64", Value: int64(7)},
		{Key: "big", Value: int64(1 << 60)},
		{Key: "whole", Value: 2.0},
		{Key: "frac", Value: 2.5},
		{Key: "s", Value: "a\"b<"},
		{Key: "when", Value: bson.DateTime(0)},
		{Key: "nested", Value: bson.D{{Key: "arr", Value: bson.A{int64(1), int32(2), 3.0}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := typedExtJSON(nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"_id":{"$oid":"010000000000000000000000"},"i32":7,"i64":{"$numberLong":"7"},` +
		`"big":{"$numberLong":"1152921504606846976"},"whole":{"$numberDouble":"2.0"},"frac":2.5,"s":"a\"b<",` +
		`"when":{"$date":"1970-01-01T00:00:00Z"},"nested":{"arr":[{"$numberLong":"1"},2,{"$numberDouble":"3.0"}]}}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// Relaxed parsing must give the same types back.
	var back bson.D
	if err := bson.UnmarshalExtJSON(got, false, &back); err != nil {
		t.Fatal(err)
	}
	again, _ := bson.Marshal(back)
	if !bytes.Equal(again, raw) {
		t.Fatal("round trip changed the document")
	}
}
