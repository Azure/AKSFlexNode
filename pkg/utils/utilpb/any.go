package utilpb

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

func AnyTo[M proto.Message](o *anypb.Any) (M, error) {
	m, err := o.UnmarshalNew()
	if err != nil {
		var zero M
		return zero, err
	}

	return To[M](m)
}

func To[M proto.Message](m any) (M, error) {
	if m, ok := m.(M); ok {
		return m, nil
	}

	return *new(M), status.Error(codes.InvalidArgument, "type mismatch")
}
