// Package kafkawire shares Kafka header parsing between proxies and mocks.
package kafkawire

import (
	"encoding/binary"
	"fmt"

	"github.com/twmb/franz-go/pkg/kmsg"
)

func SkipTags(b []byte) ([]byte, error) {
	count, n := binary.Uvarint(b)
	if n <= 0 {
		return nil, fmt.Errorf("invalid Kafka tag count")
	}
	b = b[n:]
	for ; count > 0; count-- {
		_, n = binary.Uvarint(b)
		if n <= 0 {
			return nil, fmt.Errorf("invalid Kafka tag")
		}
		b = b[n:]
		size, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, fmt.Errorf("invalid Kafka tag size")
		}
		b = b[n:]
		if size > uint64(len(b)) {
			return nil, fmt.Errorf("truncated Kafka tag")
		}
		b = b[int(size):]
	}
	return b, nil
}

// DecodeRequest takes a request without its four-byte frame size.
func DecodeRequest(b []byte) (kmsg.Request, int32, string, error) {
	if len(b) < 10 {
		return nil, 0, "", fmt.Errorf("truncated Kafka header")
	}
	key := int16(binary.BigEndian.Uint16(b))
	version := int16(binary.BigEndian.Uint16(b[2:]))
	corr := int32(binary.BigEndian.Uint32(b[4:]))
	size := int(int16(binary.BigEndian.Uint16(b[8:])))
	req := kmsg.RequestForKey(key)
	if req == nil {
		return nil, corr, "", fmt.Errorf("unknown Kafka API %d", key)
	}
	req.SetVersion(version)
	b = b[10:]
	client := ""
	if size >= 0 {
		if size > len(b) {
			return nil, corr, "", fmt.Errorf("truncated client ID")
		}
		client = string(b[:size])
		b = b[size:]
	}
	var err error
	if req.IsFlexible() {
		b, err = SkipTags(b)
		if err != nil {
			return nil, corr, client, err
		}
	}
	if err = req.ReadFrom(b); err != nil {
		return nil, corr, client, err
	}
	return req, corr, client, nil
}
