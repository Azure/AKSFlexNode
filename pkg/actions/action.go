package actions

import "github.com/gogo/protobuf/proto"

// Action represents an action to be applied.
type Action interface {
	proto.Message
	GetMetadata() *Metadata
}
