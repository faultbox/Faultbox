package protocol

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// ServeMock implements MockHandler for MongoDB. It answers the subset of
// OP_MSG commands that a typical application needs at startup and for
// simple read-through usage:
//
//   - hello / isMaster / ismaster  → server-info document (required for
//     drivers to complete the handshake)
//   - ping                         → ok: 1
//   - buildInfo                    → ok: 1 + minimal version
//   - find                         → cursor with firstBatch from config
//                                    collections["<name>"]
//   - findOne (implemented as find with limit=1)
//   - insert / update / delete     → ok: 1 + n (writes discarded — this
//                                    mock is for read-through tests)
//   - getMore                      → empty cursor (terminates iteration)
//
// Any unrecognized command returns ok: 1 with no payload — lenient by
// design, so unexpected commands don't fail the test with a protocol
// mismatch. Users who need strict semantics should run the real MongoDB.
//
// Configuration keys (from spec.Config):
//
//	collections: map[string]any — collection name → list of BSON documents
//	  served by find/findOne. Each document is a map[string]any.
//	  Pre-population; no updates at runtime.
//
// Wire: real BSON OP_MSG (shares encoding with the RFC-016 MongoDB proxy).
func (p *mongoProtocol) ServeMock(ctx context.Context, addr string, spec MockSpec, emit MockEmitter) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("mock mongodb listen %s: %w", addr, err)
	}

	collections := extractCollections(spec.Config)
	state := &mongoMockState{collections: collections}

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		go handleMongoMockConn(ctx, conn, state, emit)
	}
}

type mongoMockState struct {
	collections map[string][]map[string]any
	requestID   atomic.Int32
}

func extractCollections(config map[string]any) map[string][]map[string]any {
	out := make(map[string][]map[string]any)
	raw, ok := config["collections"]
	if !ok {
		return out
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return out
	}
	for name, docsRaw := range m {
		docs, ok := docsRaw.([]any)
		if !ok {
			continue
		}
		coll := make([]map[string]any, 0, len(docs))
		for _, d := range docs {
			if doc, ok := d.(map[string]any); ok {
				coll = append(coll, doc)
			}
		}
		out[name] = coll
	}
	return out
}

func handleMongoMockConn(ctx context.Context, conn net.Conn, state *mongoMockState, emit MockEmitter) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	header := make([]byte, 16)
	for {
		if ctx.Err() != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}

		msgLen := binary.LittleEndian.Uint32(header[0:4])
		reqID := binary.LittleEndian.Uint32(header[4:8])
		opCode := binary.LittleEndian.Uint32(header[12:16])

		if msgLen < 16 || msgLen > 48*1024*1024 {
			return
		}
		body := make([]byte, msgLen-16)
		if _, err := io.ReadFull(conn, body); err != nil {
			return
		}

		if opCode == opQueryCode {
			// Initial driver handshake. Parse the embedded query document,
			// dispatch by command, and reply as OP_REPLY.
			cmd, doc := parseOPQuery(body)
			emitWith(emit, "op_query", map[string]string{"cmd": cmd})
			respDoc := state.respondToCommand(cmd, "", doc)
			if err := sendMongoOPReply(conn, reqID, respDoc, state); err != nil {
				emitWith(emit, "write_error", map[string]string{"error": err.Error()})
				return
			}
			continue
		}

		if opCode != opMsgCode {
			emitWith(emit, "unknown_opcode", map[string]string{
				"opcode": fmt.Sprintf("%d", opCode),
			})
			continue
		}

		cmd, coll := parseOPMSGCmd(body)
		emitWith(emit, "cmd", map[string]string{
			"cmd":        cmd,
			"collection": coll,
			"body_size":  fmt.Sprintf("%d", len(body)),
		})
		resp := state.respondToCommand(cmd, coll, body)
		if err := sendMongoMockReply(conn, reqID, resp, state); err != nil {
			emitWith(emit, "write_error", map[string]string{"error": err.Error()})
			return
		}
		emitWith(emit, "reply_sent", map[string]string{"cmd": cmd})
	}
}

// MongoDB wire opcodes we handle. Modern drivers (v2+) use OP_QUERY for
// the very first handshake so they work against old servers, then switch
// to OP_MSG once the server's maxWireVersion is confirmed.
const (
	opMsgCode   = 2013 // OP_MSG
	opQueryCode = 2004 // OP_QUERY (legacy handshake path)
	opReplyCode = 1    // OP_REPLY (legacy handshake response)
)

// parseOPMSGCmd extracts the command name and collection from an OP_MSG
// body (same logic as internal/proxy/mongodb.go).
func parseOPMSGCmd(body []byte) (cmd string, collection string) {
	if len(body) < 5 {
		return "", ""
	}
	offset := 4 // skip flagBits
	if body[offset] != 0 {
		return "", ""
	}
	offset++
	if offset+4 > len(body) {
		return "", ""
	}
	bsonLen := int(binary.LittleEndian.Uint32(body[offset:]))
	if offset+bsonLen > len(body) {
		return "", ""
	}
	bsonDoc := body[offset : offset+bsonLen]

	pos := 4
	if pos >= len(bsonDoc) {
		return "", ""
	}
	elemType := bsonDoc[pos]
	pos++
	keyStart := pos
	for pos < len(bsonDoc) && bsonDoc[pos] != 0 {
		pos++
	}
	if pos >= len(bsonDoc) {
		return "", ""
	}
	cmd = string(bsonDoc[keyStart:pos])
	pos++

	if elemType == 0x02 { // string
		if pos+4 > len(bsonDoc) {
			return cmd, ""
		}
		strLen := int(binary.LittleEndian.Uint32(bsonDoc[pos:]))
		pos += 4
		if pos+strLen > len(bsonDoc) {
			return cmd, ""
		}
		collection = string(bsonDoc[pos : pos+strLen-1])
	}
	return cmd, collection
}

// respondToCommand produces the BSON response document for a given command.
// Each branch hand-picks the fields the official drivers check at handshake
// and read time.
func (s *mongoMockState) respondToCommand(cmd, coll string, body []byte) bson.M {
	switch cmd {
	case "hello", "isMaster", "ismaster":
		return bson.M{
			"ok":                           1.0,
			"ismaster":                     true,
			"isWritablePrimary":            true,
			"maxBsonObjectSize":            int32(16777216),
			"maxMessageSizeBytes":          int32(48000000),
			"maxWriteBatchSize":            int32(100000),
			"logicalSessionTimeoutMinutes": int32(30),
			"connectionId":                 int32(1),
			"minWireVersion":               int32(8),
			"maxWireVersion":               int32(17),
			"readOnly":                     false,
			"topologyVersion": bson.M{
				"processId": bson.NewObjectID(),
				"counter":   int64(0),
			},
		}
	case "ping":
		return bson.M{"ok": 1.0}
	case "buildInfo", "buildinfo":
		return bson.M{
			"ok":           1.0,
			"version":      "7.0.0",
			"gitVersion":   "faultbox-mock",
			"versionArray": bson.A{7, 0, 0, 0},
			"maxBsonObjectSize": 16777216,
		}
	case "find", "findOne":
		docs := s.collections[coll]
		batch := bson.A{}
		for _, d := range docs {
			batch = append(batch, bson.M(d))
		}
		return bson.M{
			"ok": 1.0,
			"cursor": bson.M{
				"id":         int64(0), // exhausted cursor — client stops here
				"ns":         "mock." + coll,
				"firstBatch": batch,
			},
		}
	case "getMore":
		return bson.M{
			"ok": 1.0,
			"cursor": bson.M{
				"id":       int64(0),
				"ns":       "mock." + coll,
				"nextBatch": bson.A{},
			},
		}
	case "insert":
		return bson.M{"ok": 1.0, "n": 1}
	case "update":
		return bson.M{"ok": 1.0, "n": 1, "nModified": 1}
	case "delete":
		return bson.M{"ok": 1.0, "n": 1}
	case "endSessions", "abortTransaction", "commitTransaction":
		return bson.M{"ok": 1.0}
	default:
		// Lenient fallback: acknowledge but don't claim support.
		return bson.M{"ok": 1.0}
	}
}

// parseOPQuery extracts the command name from an OP_QUERY body. Body layout:
// flags(4) + fullCollectionName(cstring) + numberToSkip(4) + numberToReturn(4)
// + query(BSON). The first key of the query document is the command name.
func parseOPQuery(body []byte) (cmd string, body2 []byte) {
	if len(body) < 4 {
		return "", nil
	}
	offset := 4 // flags
	// fullCollectionName: null-terminated cstring.
	end := offset
	for end < len(body) && body[end] != 0 {
		end++
	}
	offset = end + 1
	// numberToSkip + numberToReturn
	offset += 8
	if offset+4 > len(body) {
		return "", nil
	}
	bsonLen := int(binary.LittleEndian.Uint32(body[offset:]))
	if offset+bsonLen > len(body) {
		return "", nil
	}
	bsonDoc := body[offset : offset+bsonLen]
	// Parse first element to get command name.
	pos := 4
	if pos >= len(bsonDoc) {
		return "", bsonDoc
	}
	pos++ // elem type
	keyStart := pos
	for pos < len(bsonDoc) && bsonDoc[pos] != 0 {
		pos++
	}
	if pos >= len(bsonDoc) {
		return "", bsonDoc
	}
	cmd = string(bsonDoc[keyStart:pos])
	return cmd, bsonDoc
}

// sendMongoOPReply writes an OP_REPLY (legacy handshake response) containing
// exactly one BSON document. Body layout:
// responseFlags(4) + cursorID(8) + startingFrom(4) + numberReturned(4) + docs
func sendMongoOPReply(conn net.Conn, respondTo uint32, doc bson.M, state *mongoMockState) error {
	bsonData, err := bson.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	body := make([]byte, 0, 20+len(bsonData))
	// responseFlags = 0
	body = append(body, 0, 0, 0, 0)
	// cursorID = 0
	body = append(body, 0, 0, 0, 0, 0, 0, 0, 0)
	// startingFrom = 0
	body = append(body, 0, 0, 0, 0)
	// numberReturned = 1
	nr := make([]byte, 4)
	binary.LittleEndian.PutUint32(nr, 1)
	body = append(body, nr...)
	// Documents
	body = append(body, bsonData...)

	totalLen := uint32(16 + len(body))
	reqID := uint32(state.requestID.Add(1))
	frame := make([]byte, 0, int(totalLen))
	header := make([]byte, 16)
	binary.LittleEndian.PutUint32(header[0:4], totalLen)
	binary.LittleEndian.PutUint32(header[4:8], reqID)
	binary.LittleEndian.PutUint32(header[8:12], respondTo)
	binary.LittleEndian.PutUint32(header[12:16], opReplyCode)
	frame = append(frame, header...)
	frame = append(frame, body...)

	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err = conn.Write(frame)
	return err
}

// sendMongoMockReply writes an OP_MSG response with the given BSON document.
func sendMongoMockReply(conn net.Conn, respondTo uint32, doc bson.M, state *mongoMockState) error {
	bsonData, err := bson.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	// OP_MSG body: flagBits(4) + section kind 0 + BSON document.
	body := make([]byte, 0, 4+1+len(bsonData))
	body = append(body, 0, 0, 0, 0)
	body = append(body, 0)
	body = append(body, bsonData...)

	// Header: messageLength(4) + requestID(4) + responseTo(4) + opCode(4).
	totalLen := uint32(16 + len(body))
	reqID := uint32(state.requestID.Add(1))
	frame := make([]byte, 0, int(totalLen))
	header := make([]byte, 16)
	binary.LittleEndian.PutUint32(header[0:4], totalLen)
	binary.LittleEndian.PutUint32(header[4:8], reqID)
	binary.LittleEndian.PutUint32(header[8:12], respondTo)
	binary.LittleEndian.PutUint32(header[12:16], opMsgCode)
	frame = append(frame, header...)
	frame = append(frame, body...)

	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err = conn.Write(frame)
	return err
}
