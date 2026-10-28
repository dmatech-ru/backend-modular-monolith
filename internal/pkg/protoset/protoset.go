// Package protoset собирает protobuf.FileDescriptorSet из дескрипторов
// proto-файлов и их транзитивных импортов — самодостаточный набор,
// который можно передать по сети или сохранить, не завязываясь на
// отдельную сборку дескрипторов средствами buf.
package protoset

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Build сериализует дескрипторы переданных proto-файлов вместе со всеми
// транзитивными импортами в protobuf.FileDescriptorSet. Импорты всегда идут
// раньше файлов, которые на них ссылаются.
func Build(files ...protoreflect.FileDescriptor) ([]byte, error) {
	seen := make(map[string]struct{})
	set := &descriptorpb.FileDescriptorSet{}

	var collect func(fd protoreflect.FileDescriptor)
	collect = func(fd protoreflect.FileDescriptor) {
		if _, ok := seen[fd.Path()]; ok {
			return
		}
		seen[fd.Path()] = struct{}{}

		imports := fd.Imports()
		for i := 0; i < imports.Len(); i++ {
			collect(imports.Get(i).FileDescriptor)
		}
		// Импорты должны идти раньше файлов, которые на них ссылаются.
		set.File = append(set.File, protodesc.ToFileDescriptorProto(fd))
	}

	for _, fd := range files {
		collect(fd)
	}

	return proto.Marshal(set)
}
