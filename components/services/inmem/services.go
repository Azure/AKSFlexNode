package inmem

import (
	"context"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	_ "go.goms.io/aks/AKSFlexNode/components" // register all known components
	"go.goms.io/aks/AKSFlexNode/components/services/actions"
)

var localConn = bufconn.Listen(1024 * 1024)

func init() {
	s := grpc.NewServer()

	actions.RegisterActionsServiceServer(s, actions.Default)

	go s.Serve(localConn) //nolint:errcheck // serve in background
}

// NewConnection creates a new gRPC client connection to the in-memory gRPC server.
func NewConnection() (*grpc.ClientConn, error) {
	return grpc.NewClient("passthrough:",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return localConn.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
}
