package actions

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"go.goms.io/aks/AKSFlexNode/components/api"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilpb"
)

// Server is ActionsServiceServer without UnimplementedActionsServiceServer
type Server interface {
	ApplyAction(context.Context, *ApplyActionRequest) (*ApplyActionResponse, error)
}

// Hub is the central registry for all actions.
// It forwards the ApplyAction request to the corresponding server based on the action type.
type Hub struct {
	Servers map[string]Server

	UnimplementedActionsServiceServer
}

var _ Server = (*Hub)(nil)

func NewHub() *Hub {
	return &Hub{
		Servers: make(map[string]Server),
	}
}

func (h *Hub) MustRegister(newServer func() (Server, error), msg proto.Message) {
	srv, err := newServer()
	if err != nil {
		panic(err)
	}

	h.Servers[utilpb.TypeURL(msg)] = srv
}

func (h *Hub) ApplyAction(
	ctx context.Context,
	req *ApplyActionRequest,
) (*ApplyActionResponse, error) {
	srv, ok := h.Servers[req.GetItem().GetTypeUrl()]
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "")
	}

	resp, err := srv.ApplyAction(ctx, req)
	if err != nil {
		return resp, err
	}

	obj, err := utilpb.AnyTo[api.Action](req.GetItem())
	if err != nil {
		return resp, err
	}

	// post processing on the response action object
	obj.Redact()

	item, err := anypb.New(obj)
	if err != nil {
		return nil, err
	}
	return ApplyActionResponse_builder{Item: item}.Build(), nil
}
