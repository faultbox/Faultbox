package eventsource

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

func init() {
	RegisterSource("wal_stream", func(params map[string]string, decoder Decoder) (EventSource, error) {
		slot := params["slot"]
		if slot == "" {
			slot = "faultbox"
		}
		return &walStreamSource{slot: slot, decoder: decoder}, nil
	})
}

type walStreamSource struct {
	slot    string
	decoder Decoder
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func (s *walStreamSource) Name() string { return "wal_stream" }

func (s *walStreamSource) Start(ctx context.Context, cfg SourceConfig) error {
	ctx, s.cancel = context.WithCancel(ctx)

	connStr := cfg.Params["connstr"]
	if connStr == "" {
		addr := cfg.Params["addr"]
		if addr == "" {
			addr = "localhost:5432"
		}
		connStr = fmt.Sprintf("postgres://%s/postgres?replication=database&sslmode=disable", addr)
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.stream(ctx, connStr, cfg)
	}()

	return nil
}

func (s *walStreamSource) stream(ctx context.Context, connStr string, cfg SourceConfig) {
	// Retry connection with backoff.
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		conn, err := pgconn.Connect(ctx, connStr)
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		// Create replication slot if it doesn't exist (idempotent).
		_, _ = pglogrepl.CreateReplicationSlot(ctx, conn, s.slot, "pgoutput",
			pglogrepl.CreateReplicationSlotOptions{Temporary: true})

		// Start replication.
		err = pglogrepl.StartReplication(ctx, conn, s.slot, 0,
			pglogrepl.StartReplicationOptions{
				PluginArgs: []string{
					"proto_version '1'",
					"publication_names 'faultbox_pub'",
				},
			})
		if err != nil {
			conn.Close(ctx)
			time.Sleep(500 * time.Millisecond)
			continue
		}

		// Read WAL messages.
		s.readMessages(ctx, conn, cfg)
		conn.Close(ctx)
	}
}

func (s *walStreamSource) readMessages(ctx context.Context, conn *pgconn.PgConn, cfg SourceConfig) {
	standbyTimeout := 10 * time.Second
	nextStandby := time.Now().Add(standbyTimeout)
	var clientXLogPos pglogrepl.LSN

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if time.Now().After(nextStandby) {
			_ = pglogrepl.SendStandbyStatusUpdate(ctx, conn,
				pglogrepl.StandbyStatusUpdate{WALWritePosition: clientXLogPos})
			nextStandby = time.Now().Add(standbyTimeout)
		}

		recvCtx, cancel := context.WithDeadline(ctx, nextStandby)
		rawMsg, err := conn.ReceiveMessage(recvCtx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}

		if errMsg, ok := rawMsg.(*pgproto3.ErrorResponse); ok {
			cfg.Emit("wal", map[string]string{
				"error": errMsg.Message,
			})
			continue
		}

		msg, ok := rawMsg.(*pgproto3.CopyData)
		if !ok {
			continue
		}

		switch msg.Data[0] {
		case pglogrepl.XLogDataByteID:
			xld, err := pglogrepl.ParseXLogData(msg.Data[1:])
			if err != nil {
				continue
			}

			if xld.WALStart > clientXLogPos {
				clientXLogPos = xld.WALStart
			}

			// Parse the logical replication message.
			fields := s.parseWALData(xld.WALData)
			if fields != nil {
				cfg.Emit("wal", fields)
			}

		case pglogrepl.PrimaryKeepaliveMessageByteID:
			pka, err := pglogrepl.ParsePrimaryKeepaliveMessage(msg.Data[1:])
			if err == nil && pka.ReplyRequested {
				_ = pglogrepl.SendStandbyStatusUpdate(ctx, conn,
					pglogrepl.StandbyStatusUpdate{WALWritePosition: clientXLogPos})
				nextStandby = time.Now().Add(standbyTimeout)
			}
		}
	}
}

func (s *walStreamSource) parseWALData(data []byte) map[string]string {
	if len(data) == 0 {
		return nil
	}

	// pgoutput message types: 'B' begin, 'C' commit, 'I' insert, 'U' update, 'D' delete, 'R' relation
	msgType := data[0]
	switch msgType {
	case 'I':
		return map[string]string{
			"op":   "INSERT",
			"data": string(data),
		}
	case 'U':
		return map[string]string{
			"op":   "UPDATE",
			"data": string(data),
		}
	case 'D':
		return map[string]string{
			"op":   "DELETE",
			"data": string(data),
		}
	case 'B':
		return map[string]string{"op": "BEGIN"}
	case 'C':
		return map[string]string{"op": "COMMIT"}
	case 'R':
		// Relation message — extract table info.
		// Store as JSON for .data auto-decoding.
		relJSON, _ := json.Marshal(map[string]string{
			"op":   "RELATION",
			"data": string(data),
		})
		fields := map[string]string{
			"op":   "RELATION",
			"data": string(relJSON),
		}
		return fields
	default:
		return nil
	}
}

func (s *walStreamSource) Stop() error {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	return nil
}
