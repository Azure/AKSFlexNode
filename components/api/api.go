package api

import "google.golang.org/protobuf/proto"

// Action represents an action to be applied.
// It is a protobuf message with a standard metadata field.
type Action interface {
	proto.Message
	GetMetadata() *Metadata
	Redact()
}

type WithDefaulting interface {
	Defaulting()
}

type WithValidation interface {
	Validate() error
}

func DefaultAndValidate[M any](m M) (M, error) {
	if d, ok := any(m).(WithDefaulting); ok {
		d.Defaulting()
	}

	if v, ok := any(m).(WithValidation); ok {
		if err := v.Validate(); err != nil {
			return m, err
		}
	}

	return m, nil
}
