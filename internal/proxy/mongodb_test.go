package proxy

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestSendMongoError_IsValidBSON verifies the error response uses real BSON
// (not JSON) so MongoDB drivers can decode it as a server error.
//
// This is a regression test for a bug where sendMongoError used json.Marshal,
// which left drivers in an undefined state — they could neither parse the
// body nor reliably distinguish "injected fault" from "proxy malfunction".
func TestSendMongoError_IsValidBSON(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		sendMongoError(server, 42, "injected disk full")
		server.Close()
	}()

	client.SetReadDeadline(time.Now().Add(2 * time.Second))

	// Read OP_MSG header: length(4) + reqID(4) + respTo(4) + opCode(4).
	header := make([]byte, 16)
	if _, err := client.Read(header); err != nil {
		t.Fatalf("read header: %v", err)
	}
	totalLen := binary.LittleEndian.Uint32(header[0:4])
	respTo := binary.LittleEndian.Uint32(header[8:12])
	opCode := int32(binary.LittleEndian.Uint32(header[12:16]))

	if respTo != 42 {
		t.Errorf("responseTo = %d, want 42", respTo)
	}
	if opCode != opMsgCode {
		t.Errorf("opCode = %d, want %d", opCode, opMsgCode)
	}

	// Read remaining body.
	body := make([]byte, totalLen-16)
	if _, err := client.Read(body); err != nil {
		t.Fatalf("read body: %v", err)
	}

	// Body = flagBits(4) + section kind 0 + BSON doc.
	if len(body) < 5 {
		t.Fatalf("body too short: %d bytes", len(body))
	}
	flagBits := binary.LittleEndian.Uint32(body[0:4])
	if flagBits != 0 {
		t.Errorf("flagBits = %d, want 0", flagBits)
	}
	if body[4] != 0 {
		t.Errorf("section kind = %d, want 0", body[4])
	}

	// Remainder must decode as valid BSON.
	bsonDoc := body[5:]
	var got bson.M
	if err := bson.Unmarshal(bsonDoc, &got); err != nil {
		t.Fatalf("body is not valid BSON: %v (bytes: %x)", err, bsonDoc)
	}

	if got["ok"] != 0.0 {
		t.Errorf("ok = %v, want 0.0", got["ok"])
	}
	if got["errmsg"] != "injected disk full" {
		t.Errorf("errmsg = %q, want %q", got["errmsg"], "injected disk full")
	}
	if got["code"] == nil {
		t.Errorf("missing code field")
	}

	<-done
}

// TestParseOPMSG_Insert verifies the proxy correctly extracts the command
// and collection from a BSON-encoded OP_MSG insert payload.
//
// Real MongoDB clients always send the command name as the first BSON key.
// We use bson.D (ordered) to guarantee that ordering in the test — bson.M
// would pick an arbitrary order and break the invariant parseOPMSG relies on.
func TestParseOPMSG_Insert(t *testing.T) {
	insertCmd := bson.D{
		{Key: "insert", Value: "orders"},
		{Key: "documents", Value: bson.A{bson.M{"_id": 1, "item": "apple"}}},
		{Key: "$db", Value: "test"},
	}
	doc, err := bson.Marshal(insertCmd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Build OP_MSG body: flagBits(4) + section kind 0 + doc.
	var buf bytes.Buffer
	buf.Write([]byte{0, 0, 0, 0}) // flagBits
	buf.WriteByte(0)              // section kind 0
	buf.Write(doc)

	cmd, collection := parseOPMSG(buf.Bytes())
	if cmd != "insert" {
		t.Errorf("cmd = %q, want insert", cmd)
	}
	if collection != "orders" {
		t.Errorf("collection = %q, want orders", collection)
	}
}

// TestMongoProxy_Protocol verifies the factory returns a mongodb proxy.
func TestMongoProxy_Protocol(t *testing.T) {
	p, err := newProxy("mongodb", nil, "test")
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	if p.Protocol() != "mongodb" {
		t.Errorf("Protocol() = %q, want mongodb", p.Protocol())
	}
}
