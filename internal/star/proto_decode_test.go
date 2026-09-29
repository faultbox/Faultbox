package star

import (
	"encoding/base64"
	"fmt"
	"testing"

	"go.starlark.net/starlark"
)

func TestProtoDecodeWireAndBase64(t *testing.T) {
	pb := writeTestDescriptorSet(t)
	rt := New(testLogger())
	src := fmt.Sprintf(`
wire=proto_encode(descriptors=%q,message="test.config.Setting",body={"id":"1000001","name":"a"*160})
decoded=proto_decode(descriptors=%q,message="test.config.Setting",data=wire)
assert_eq(decoded["id"],"1000001")
assert_eq(decoded["name"],"a"*160)
`, pb, pb)
	if err := rt.LoadString("decode.star", src); err != nil {
		t.Fatal(err)
	}
	wire := rt.globals["wire"].(starlark.Bytes)
	rt2 := New(testLogger())
	if err := rt2.LoadString("decode_b64.star", fmt.Sprintf(`
decoded=proto_decode(descriptors=%q,message="test.config.Setting",data_base64=%q)
assert_eq(decoded["id"],"1000001")
assert_eq(decoded["name"],"a"*160)
`, pb, base64.StdEncoding.EncodeToString([]byte(wire)))); err != nil {
		t.Fatal(err)
	}
	if len(rt2.LoadedSpecs()) == 0 {
		t.Fatal("decoder descriptor missing from bundle inputs")
	}
}

func TestProtoDecodeRejectsInvalidInput(t *testing.T) {
	pb := writeTestDescriptorSet(t)
	for _, args := range []string{`data_base64="not base64!"`, `data=b"\x08\x80"`, `data=b"",data_base64=""`, `data=None`, `data=123`} {
		err := New(testLogger()).LoadString("bad.star", fmt.Sprintf(`proto_decode(descriptors=%q,message="test.config.Setting",%s)`, pb, args))
		if err == nil {
			t.Fatalf("accepted %s", args)
		}
	}
}

func TestKafkaResponseOffsetsRemainExact(t *testing.T) {
	response := &Response{Body: `{"records":[{"offset":9223372036854775807}]}`}
	data, err := response.Attr("data")
	if err != nil {
		t.Fatal(err)
	}
	rows, _, _ := data.(*starlark.Dict).Get(starlark.String("records"))
	offset, _, _ := rows.(*starlark.List).Index(0).(*starlark.Dict).Get(starlark.String("offset"))
	if offset.String() != "9223372036854775807" {
		t.Fatalf("offset rounded: %s", offset)
	}
}
