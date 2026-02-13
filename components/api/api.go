package api

import "google.golang.org/protobuf/proto"

// Action represents an action to be applied.
// It is a protobuf message with a standard metadata field.
type Action interface {
	proto.Message
	GetMetadata() *Metadata
	Redact()
}
