package bootstrapper

import (
	"google.golang.org/grpc"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

// Component action executors exported for reuse by other packages (e.g. drift remediation).
//
// These wrap unexported component action resolvers defined in components.go.
func ResetKubeletExecutor(name string, conn *grpc.ClientConn, cfg *config.Config) Executor {
	return resetKubelet.Executor(name, conn, cfg)
}

func DownloadKubeBinariesExecutor(name string, conn *grpc.ClientConn, cfg *config.Config) Executor {
	return downloadKubeBinaries.Executor(name, conn, cfg)
}

func StartKubeletExecutor(name string, conn *grpc.ClientConn, cfg *config.Config) Executor {
	return startKubelet.Executor(name, conn, cfg)
}
