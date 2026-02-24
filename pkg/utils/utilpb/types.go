package utilpb

import "google.golang.org/protobuf/proto"

func TypeURL(msg proto.Message) string {
	return "type.googleapis.com/" + string(msg.ProtoReflect().Descriptor().FullName())
}
