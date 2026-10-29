package protoset

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	errorsv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/common"
	authv1 "dmatech.ru/internal/pkg/contracts/webapi/v1/identity/auth"
)

func TestBuild_SingleFileWithoutImports(t *testing.T) {
	data, err := Build(authv1.File_webapi_v1_identity_auth_auth_proto)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(data, &set); err != nil {
		t.Fatalf("failed to unmarshal descriptor set: %v", err)
	}
	if len(set.File) != 1 || set.File[0].GetName() != "webapi/v1/identity/auth/auth.proto" {
		t.Fatalf("unexpected file set: %+v", set.File)
	}
}

func TestBuild_TransitiveImportsPrecedeDependent(t *testing.T) {
	// errors.proto импортирует google/protobuf/struct.proto.
	data, err := Build(errorsv1.File_webapi_v1_common_errors_proto)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(data, &set); err != nil {
		t.Fatalf("failed to unmarshal descriptor set: %v", err)
	}

	indexOf := make(map[string]int, len(set.File))
	for i, f := range set.File {
		indexOf[f.GetName()] = i
	}

	errorsIdx, ok := indexOf["webapi/v1/common/errors.proto"]
	if !ok {
		t.Fatalf("expected errors.proto in descriptor set, got %v", indexOf)
	}
	structIdx, ok := indexOf["google/protobuf/struct.proto"]
	if !ok {
		t.Fatalf("expected imported struct.proto in descriptor set, got %v", indexOf)
	}
	if structIdx >= errorsIdx {
		t.Fatalf("expected struct.proto to precede errors.proto (which imports it), got order %v", indexOf)
	}
}

func TestBuild_Dedup(t *testing.T) {
	data, err := Build(errorsv1.File_webapi_v1_common_errors_proto, errorsv1.File_webapi_v1_common_errors_proto)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(data, &set); err != nil {
		t.Fatalf("failed to unmarshal descriptor set: %v", err)
	}
	// errors.proto -> google/protobuf/struct.proto.
	if len(set.File) != 2 {
		t.Fatalf("expected deduplicated 2 files (errors.proto + its import), got %d", len(set.File))
	}
}
