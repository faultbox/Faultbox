package protocol

import (
	"reflect"
	"testing"
	"time"

	"github.com/gocql/gocql"
)

func TestCassandraProtocolRegistered(t *testing.T) {
	p, ok := Get("cassandra")
	if !ok {
		t.Fatal("cassandra protocol not registered")
	}
	want := []string{"query", "exec"}
	if !reflect.DeepEqual(p.Methods(), want) {
		t.Errorf("Methods() = %v, want %v", p.Methods(), want)
	}
}

func TestParseConsistency(t *testing.T) {
	cases := []struct {
		in   string
		want gocql.Consistency
	}{
		{"ONE", gocql.One},
		{"one", gocql.One}, // case-insensitive
		{"QUORUM", gocql.Quorum},
		{"LOCAL_QUORUM", gocql.LocalQuorum},
		{"EACH_QUORUM", gocql.EachQuorum},
		{"LOCAL_ONE", gocql.LocalOne},
		{"ALL", gocql.All},
		{"ANY", gocql.Any},
		{"TWO", gocql.Two},
		{"THREE", gocql.Three},
		{"unknown-bogus", gocql.One}, // fallback
		{"", gocql.One},
	}
	for _, c := range cases {
		got := parseConsistency(c.in)
		if got != c.want {
			t.Errorf("parseConsistency(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSplitHostPort(t *testing.T) {
	cases := []struct {
		in   string
		host string
		port int
	}{
		{"localhost:9042", "localhost", 9042},
		{"10.0.0.1:9999", "10.0.0.1", 9999},
		{"cassandra", "cassandra", 9042},           // default port
		{"cassandra:", "cassandra", 9042},          // empty port → default
	}
	for _, c := range cases {
		host, port := splitHostPort(c.in, 9042)
		if host != c.host || port != c.port {
			t.Errorf("splitHostPort(%q) = (%q,%d), want (%q,%d)", c.in, host, port, c.host, c.port)
		}
	}
}

func TestNormalizeCassandraValue_UUID(t *testing.T) {
	uuid := gocql.TimeUUID()
	got := normalizeCassandraValue(uuid)
	if got != uuid.String() {
		t.Errorf("UUID not stringified: got %v", got)
	}
}

func TestNormalizeCassandraValue_Bytes(t *testing.T) {
	got := normalizeCassandraValue([]byte("hello"))
	if got != "hello" {
		t.Errorf("[]byte not stringified: got %v (%T)", got, got)
	}
}

func TestNormalizeCassandraValue_Time(t *testing.T) {
	ts := time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC)
	got := normalizeCassandraValue(ts)
	if got != "2026-04-18T12:00:00Z" {
		t.Errorf("time = %v, want RFC3339 UTC", got)
	}
}
