package protocol

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"

	"github.com/faultbox/Faultbox/internal/connowner"
	"github.com/faultbox/Faultbox/internal/kafkawire"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

// Observe completed wire responses, not requests: a rejected produce or
// commit must never look successful in the trace. Listener wrapping also
// keeps Kafka's advertised endpoints on the mock's proxy when present.
type kafkaObserver struct {
	emit          MockEmitter
	advertise     func() string
	mu            sync.Mutex
	topics        map[[16]byte]string
	sequence      uint64
	groups        map[string]*kafkaObservedGroup
	fetchSessions map[kafkaFetchSessionKey]map[kafkaTopicPartition]kafkaSessionPosition
	connections   *connowner.Tracker
	sourceActive  func(connowner.Source) bool
	ambiguous     map[kafkaClientPartition]bool
}
type kafkaObservedListener struct {
	net.Listener
	observer *kafkaObserver
}

func (l *kafkaObservedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	observed := &kafkaObservedConn{Conn: c, observer: l.observer, pending: make(map[int32]kafkaPending)}
	if l.observer.connections != nil {
		observed.owner = l.observer.connections.Bind(c.RemoteAddr(), c.LocalAddr())
	}
	return observed, nil
}

type kafkaPending struct {
	req       kmsg.Request
	client    string
	sequence  uint64
	positions []kafkaFetchPosition
	source    connowner.Source
	owner     *connowner.Binding
}
type kafkaObservedConn struct {
	net.Conn
	observer                   *kafkaObserver
	owner                      *connowner.Binding
	readMu, writeMu, pendingMu sync.Mutex
	input, output              []byte
	pending                    map[int32]kafkaPending
}

func (c *kafkaObservedConn) Close() error {
	if c.owner != nil {
		c.owner.Close()
	}
	return c.Conn.Close()
}

func kafkaFrames(buffer *[]byte, data []byte, visit func([]byte) error) error {
	*buffer = append(*buffer, data...)
	for len(*buffer) >= 4 {
		size := int(binary.BigEndian.Uint32(*buffer))
		if size < 4 || size > 64*1024*1024 {
			return fmt.Errorf("invalid Kafka frame size %d", size)
		}
		if len(*buffer) < size+4 {
			break
		}
		frame := (*buffer)[4 : 4+size]
		if err := visit(frame); err != nil {
			return err
		}
		*buffer = (*buffer)[4+size:]
	}
	if len(*buffer) == 0 {
		*buffer = nil
	}
	return nil
}

func (c *kafkaObservedConn) Read(b []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	n, err := c.Conn.Read(b)
	parseErr := kafkaFrames(&c.input, b[:n], func(frame []byte) error {
		if len(frame) < 2 {
			return nil
		}
		key := int16(binary.BigEndian.Uint16(frame))
		if key != 0 && key != 1 && key != 3 && key != 8 && key != 10 && key != 11 && key != 12 && key != 13 && key != 14 {
			return nil
		}
		req, corr, client, err := kafkawire.DecodeRequest(frame)
		if err != nil {
			return err
		}
		if r, ok := req.(*kmsg.ProduceRequest); ok && r.Acks == 0 {
			emitWith(c.observer.emit, "kafka.produce_unacknowledged", map[string]string{"client_id": client})
			return nil
		}
		var source connowner.Source
		if c.owner != nil {
			source = c.owner.Source()
		}
		pending := c.observer.request(req, client, source)
		pending.owner = c.owner
		c.pendingMu.Lock()
		defer c.pendingMu.Unlock()
		if len(c.pending) >= 1024 {
			return fmt.Errorf("too many pending Kafka requests")
		}
		c.pending[corr] = pending
		return nil
	})
	if parseErr != nil {
		// Bad client input (notably a TLS client hitting this plaintext mock)
		// is a protocol error, not a failure of the observer implementation.
		emitWith(c.observer.emit, "kafka.protocol_error", map[string]string{"error": parseErr.Error()})
		return n, parseErr
	}
	return n, err
}

func (c *kafkaObservedConn) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	err := kafkaFrames(&c.output, b, func(frame []byte) error {
		corr := int32(binary.BigEndian.Uint32(frame))
		c.pendingMu.Lock()
		pending, ok := c.pending[corr]
		delete(c.pending, corr)
		c.pendingMu.Unlock()
		var response kmsg.Response
		if ok {
			response = pending.req.ResponseKind()
			response.SetVersion(pending.req.GetVersion())
			body := frame[4:]
			var err error
			if response.IsFlexible() {
				body, err = kafkawire.SkipTags(body)
				if err != nil {
					c.observer.recordError(err.Error())
					return err
				}
			}
			if err := response.ReadFrom(body); err != nil {
				c.observer.recordError(err.Error())
				return err
			}
			if c.observer.rewrite(response) {
				frame = append([]byte(nil), frame[:4]...)
				if response.IsFlexible() {
					frame = append(frame, 0)
				}
				frame = response.AppendTo(frame)
			}
		}
		wire := make([]byte, 4, 4+len(frame))
		binary.BigEndian.PutUint32(wire, uint32(len(frame)))
		wire = append(wire, frame...)
		for len(wire) > 0 {
			n, err := c.Conn.Write(wire)
			if err != nil {
				return err
			}
			if n == 0 {
				return io.ErrShortWrite
			}
			wire = wire[n:]
		}
		if ok {
			c.observer.observe(pending, response)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func (o *kafkaObserver) rewrite(response kmsg.Response) bool {
	addr := ""
	if o.advertise != nil {
		addr = o.advertise()
	}
	host, portText, err := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portText)
	if r, ok := response.(*kmsg.MetadataResponse); ok {
		o.mu.Lock()
		for _, t := range r.Topics {
			if t.Topic != nil {
				o.topics[t.TopicID] = *t.Topic
			}
		}
		o.mu.Unlock()
		if err != nil {
			return false
		}
		for i := range r.Brokers {
			r.Brokers[i].Host = host
			r.Brokers[i].Port = int32(port)
		}
		return true
	}
	if r, ok := response.(*kmsg.FindCoordinatorResponse); ok && err == nil {
		r.Host = host
		r.Port = int32(port)
		for i := range r.Coordinators {
			r.Coordinators[i].Host = host
			r.Coordinators[i].Port = int32(port)
		}
		return true
	}
	return false
}
func (o *kafkaObserver) topic(name string, id [16]byte) string {
	if name != "" {
		return name
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.topics[id]
}

func (o *kafkaObserver) observe(p kafkaPending, response kmsg.Response) {
	o.observeGroup(p, response)
	switch r := response.(type) {
	case *kmsg.ProduceResponse:
		req := p.req.(*kmsg.ProduceRequest)
		for _, t := range r.Topics {
			for _, part := range t.Partitions {
				if part.ErrorCode == 0 {
					name := o.topic(t.Topic, t.TopicID)
					for _, input := range req.Topics {
						if o.topic(input.Topic, input.TopicID) != name {
							continue
						}
						for _, batch := range input.Partitions {
							if batch.Partition == part.Partition {
								o.records("kafka.produce", name, part.Partition, p.client, batch.Records, &part.BaseOffset, p.source)
							}
						}
					}
				}
			}
		}
	case *kmsg.FetchResponse:
		for _, t := range r.Topics {
			for _, part := range t.Partitions {
				if part.ErrorCode == 0 {
					o.records("kafka.fetch", o.topic(t.Topic, t.TopicID), part.Partition, p.client, part.RecordBatches, nil, p.source)
				}
			}
		}
	case *kmsg.OffsetCommitResponse:
		req := p.req.(*kmsg.OffsetCommitRequest)
		for _, t := range r.Topics {
			for _, part := range t.Partitions {
				if part.ErrorCode == 0 {
					for _, input := range req.Topics {
						if input.Topic != t.Topic {
							continue
						}
						for _, commit := range input.Partitions {
							if commit.Partition == part.Partition {
								emitWith(o.emit, "kafka.commit", sourceFields(map[string]string{"topic": t.Topic, "partition": fmt.Sprint(part.Partition), "offset": fmt.Sprint(commit.Offset), "group": req.Group, "client_id": p.client}, p.source))
							}
						}
					}
				}
			}
		}
	}
}

func (o *kafkaObserver) records(op, topic string, partition int32, client string, data []byte, produceOffset *int64, source connowner.Source) {
	for len(data) > 0 {
		if len(data) < 12 {
			o.recordError("truncated record batch")
			return
		}
		size := int(binary.BigEndian.Uint32(data[8:])) + 12
		if size < 61 || size > len(data) {
			o.recordError("invalid record batch size")
			return
		}
		var batch kmsg.RecordBatch
		if err := batch.ReadFrom(data[:size]); err != nil {
			o.recordError(err.Error())
			return
		}
		data = data[size:]
		if batch.Attributes&32 != 0 {
			continue
		} // transaction control records
		records, err := kgo.DefaultDecompressor().Decompress(batch.Records, kgo.CompressionCodecType(batch.Attributes&7))
		if err != nil {
			o.recordError(err.Error())
			return
		}
		base := batch.FirstOffset
		if produceOffset != nil {
			base = *produceOffset
		}
		for i := int32(0); i < batch.NumRecords; i++ {
			length, n := binary.Varint(records)
			if n <= 0 || length < 0 || length > int64(len(records)-n) {
				o.recordError("invalid record length")
				return
			}
			var record kmsg.Record
			if err := record.ReadFrom(records[:n+int(length)]); err != nil {
				o.recordError(err.Error())
				return
			}
			records = records[n+int(length):]
			emitWith(o.emit, op, sourceFields(map[string]string{"topic": topic, "partition": fmt.Sprint(partition), "offset": fmt.Sprint(base + int64(record.OffsetDelta)), "client_id": client, "key_base64": base64.StdEncoding.EncodeToString(record.Key), "value_base64": base64.StdEncoding.EncodeToString(record.Value)}, source))
		}
		if produceOffset != nil {
			next := base + int64(batch.LastOffsetDelta) + 1
			produceOffset = &next
		}
	}
}
func (o *kafkaObserver) recordError(err string) {
	emitWith(o.emit, "observation_error", map[string]string{"error": err})
}
