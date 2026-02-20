package actions

import (
	context "context"

	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilpb"
	grpc "google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	anypb "google.golang.org/protobuf/types/known/anypb"
)

var (
	Default = NewHub()

	// MustRegister registers a action server for a giving action message type.
	// It panics if the server instantiation fails.
	MustRegister = Default.MustRegister
)

// ApplyAction invokes the ApplyAction method on a giving gRPC client
func ApplyAction[M proto.Message](
	conn *grpc.ClientConn,
	ctx context.Context,
	m M,
	opts ...grpc.CallOption,
) (M, error) {
	item, err := anypb.New(m)
	if err != nil {
		var zero M
		return zero, err
	}

	req := &ApplyActionRequest{}
	req.SetItem(item)

	client := NewActionsServiceClient(conn)

	resp, err := client.ApplyAction(ctx, req, opts...)
	if err != nil {
		var zero M
		return zero, err
	}

	return utilpb.AnyTo[M](resp.GetItem())
}
