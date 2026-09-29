package protocol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestHTTPFullBodyHeadersAndReadFailures(t *testing.T) {
	want := "  " + strings.Repeat("metric 123\n", 12000) + "\n"
	h := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "a=1")
		w.Header().Add("Set-Cookie", "b=2")
		w.Header().Set("X-Test", "present")
		io.WriteString(w, want)
	}
	http1 := httptest.NewServer(http.HandlerFunc(h))
	defer http1.Close()
	ln, http2 := startTestH2CServer(t, h)
	defer http2.Close()
	for _, tc := range []struct {
		p    Protocol
		addr string
	}{{&httpProtocol{}, strings.TrimPrefix(http1.URL, "http://")}, {&http2Protocol{}, ln.Addr().String()}} {
		res, err := tc.p.ExecuteStep(context.Background(), tc.addr, "get", nil)
		if err != nil || !res.Success || res.Body != strings.TrimSpace(want) || len(res.Headers["Set-Cookie"]) != 2 {
			t.Fatalf("%s: full response missing: %v %+v", tc.p.Name(), err, res)
		}
		res, err = tc.p.ExecuteStep(context.Background(), tc.addr, "get", map[string]any{"max_response_bytes": int64(65536)})
		if err != nil || res.Success || !strings.Contains(res.Error, "exceeds") || res.Body != "" {
			t.Fatalf("silent truncation: %v %+v", err, res)
		}
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		io.WriteString(w, "short")
	}))
	defer bad.Close()
	res, err := (&httpProtocol{}).ExecuteStep(context.Background(), strings.TrimPrefix(bad.URL, "http://"), "get", nil)
	if err != nil || res.Success || res.Error == "" {
		t.Fatalf("short body accepted: %+v %v", res, err)
	}
}

func TestRedisBinaryNullAndFragmentedBulk(t *testing.T) {
	payload := []byte{0x80, 0xff, 0, 'A', 'B'}
	for _, wire := range []string{"$5\r\n" + string(payload) + "\r\n", "$0\r\n\r\n", "$-1\r\n", "*3\r\n$5\r\n" + string(payload) + "\r\n$-1\r\n:42\r\n"} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			readRESP(bufio.NewReader(c))
			for _, b := range []byte(wire) {
				c.Write([]byte{b})
				time.Sleep(time.Millisecond)
			}
		}()
		res, err := (&redisProtocol{}).ExecuteStep(context.Background(), ln.Addr().String(), "get", map[string]any{"key": "binary"})
		ln.Close()
		<-done
		if err != nil || !res.Success {
			t.Fatalf("%v %+v", err, res)
		}
		var result map[string]any
		if err := json.Unmarshal([]byte(res.Body), &result); err != nil {
			t.Fatal(err)
		}
		switch wire[0:2] {
		case "$5":
			if result["value_base64"] != base64.StdEncoding.EncodeToString(payload) {
				t.Fatalf("corrupt bytes: %s", res.Body)
			}
		case "$0":
			if result["value_base64"] != "" || result["value_is_null"] != false {
				t.Fatal(res.Body)
			}
		case "$-":
			if result["value_base64"] != nil || result["value_is_null"] != true {
				t.Fatal(res.Body)
			}
		case "*3":
			a := result["value_base64"].([]any)
			if a[0] != "gP8AQUI=" || a[1] != nil || a[2] != float64(42) {
				t.Fatal(res.Body)
			}
		}
	}
	for _, kwargs := range []map[string]any{{"value": payload}, {"value_base64": "gP8AQUI="}} {
		got, err := redisInputBytes(kwargs)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("binary input: %x %v", got, err)
		}
	}
}

func TestProtoEncodingIsByteStable(t *testing.T) {
	files, err := LoadDescriptorSet(writeFds(t, buildSettingDescriptorSet()))
	if err != nil {
		t.Fatal(err)
	}
	desc, err := files.FindDescriptorByName("test.config.Setting")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"id":42,"name":"primary","scope":"default","currency":"USD"}`)
	var want []byte
	for i := 0; i < 100; i++ {
		wire, err := JSONToTypedMessage(files, desc.(protoreflect.MessageDescriptor), body)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			want = wire
		}
		if !bytes.Equal(wire, want) {
			t.Fatalf("encoding %d differs: %x / %x", i, wire, want)
		}
	}
}

func TestKafkaPublishAcknowledgedLocation(t *testing.T) {
	p := &kafkaProtocol{}
	addr := freeLoopbackAddr(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- p.ServeMock(ctx, addr, MockSpec{Config: map[string]any{"topics": map[string]any{"locations": nil}}}, nil)
	}()
	defer func() { cancel(); <-done }()
	if err := waitKafkaMockReady(addr, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		res, err := p.ExecuteStep(ctx, addr, "publish", map[string]any{"topic": "locations", "key": []byte{0xff}, "data": fmt.Sprint(i)})
		if err != nil || !res.Success {
			t.Fatalf("publish %v %+v", err, res)
		}
		var v map[string]any
		json.Unmarshal([]byte(res.Body), &v)
		if v["partition"] != float64(0) || v["offset"] != float64(i) || v["key_base64"] != "/w==" {
			t.Fatalf("wrong receipt: %s", res.Body)
		}
	}
}
