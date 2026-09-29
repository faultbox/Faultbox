package protocol

import (
	"encoding/json"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestTypedDynamicRequestFieldNamesPrecisionAndDefaults(t *testing.T) {
	set := buildSettingDescriptorSet()
	request := set.File[0].MessageType[1]
	request.Field = append(request.Field,
		msgField("city_id", 2, descriptorpb.FieldDescriptorProto_TYPE_INT32),
		msgField("token", 3, descriptorpb.FieldDescriptorProto_TYPE_BYTES),
	)
	files, err := LoadDescriptorSet(writeFds(t, set))
	if err != nil {
		t.Fatal(err)
	}
	in, _, err := ResolveMethod(files, "/test.config.ConfigService/GetSetting")
	if err != nil {
		t.Fatal(err)
	}
	msg := dynamicpb.NewMessage(in)
	msg.Set(in.Fields().ByName("id"), protoreflect.ValueOfInt64(9007199254740993))
	msg.Set(in.Fields().ByName("city_id"), protoreflect.ValueOfInt32(7))
	msg.Set(in.Fields().ByName("token"), protoreflect.ValueOfBytes([]byte{0, 128, 255}))
	wire, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		wire  []byte
		id    string
		city  float64
		token string
	}{
		{"populated", wire, "9007199254740993", 7, "AID/"},
		{"defaults", nil, "0", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := TypedMessageToJSON(files, in, tc.wire)
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			if err := json.Unmarshal(data, &body); err != nil {
				t.Fatal(err)
			}
			if body["id"] != tc.id || body["city_id"] != tc.city || body["token"] != tc.token {
				t.Fatalf("unexpected decoded body: %s", data)
			}
			if _, ok := body["cityId"]; ok {
				t.Fatalf("JSON field alias leaked: %s", data)
			}
		})
	}
	if _, err := TypedMessageToJSON(files, in, []byte{0x08, 0x80}); err == nil {
		t.Fatal("accepted truncated protobuf request")
	}
}
