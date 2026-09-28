package protocol

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestDescriptorSetIncludesWKTOutOfOrder(t *testing.T) {
	set := buildSettingDescriptorSet()
	set.File[0].Dependency = []string{"google/protobuf/timestamp.proto"}
	set.File = append(set.File, protodesc.ToFileDescriptorProto(timestamppb.File_google_protobuf_timestamp_proto))
	files, err := LoadDescriptorSet(writeFds(t, set))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := files.FindDescriptorByName("google.protobuf.Timestamp"); err != nil {
		t.Fatal(err)
	}
	set.File = append(set.File, proto.Clone(set.File[0]).(*descriptorpb.FileDescriptorProto))
	if _, err := LoadDescriptorSet(writeFds(t, set)); err == nil {
		t.Fatal("accepted duplicate customer definitions")
	}
}
