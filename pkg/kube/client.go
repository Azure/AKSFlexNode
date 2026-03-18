package kube

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v5"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Azure/AKSFlexNode/pkg/auth"
	"github.com/Azure/AKSFlexNode/pkg/config"
)

var (
	kubeletMu     sync.Mutex
	kubeletClient *kubernetes.Clientset
)

// KubeletClientset returns a cached client-go clientset constructed from the
// local kubelet kubeconfig (config.KubeletKubeconfigPath).
//
// This is safe to share across status collection and drift remediation within
// the same agent process.
func KubeletClientset() (*kubernetes.Clientset, error) {
	kubeletMu.Lock()
	defer kubeletMu.Unlock()

	if kubeletClient != nil {
		return kubeletClient, nil
	}

	restCfg, err := clientcmd.BuildConfigFromFlags("", config.KubeletKubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("build rest config from kubelet kubeconfig: %w", err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("create clientset from kubelet kubeconfig: %w", err)
	}

	kubeletClient = cs
	return kubeletClient, nil
}

// InvalidateKubeletClientset clears the cached kubelet clientset.
//
// This is useful if the kubelet kubeconfig on disk has rotated (cert renewal,
// bootstrap regeneration, etc.) and callers want subsequent operations to pick
// up the new credentials.
//
// It is safe to call concurrently.
func InvalidateKubeletClientset() {
	kubeletMu.Lock()
	defer kubeletMu.Unlock()
	kubeletClient = nil
}

// AdminClientset returns a client-go clientset constructed from the AKS cluster
// admin kubeconfig fetched via the Azure management plane.
func AdminClientset(ctx context.Context, cfg *config.Config) (*kubernetes.Clientset, error) {
	if cfg == nil {
		return nil, errors.New("cfg is nil")
	}

	adminCfgBytes, err := fetchClusterAdminKubeconfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	restCfg, err := clientcmd.RESTConfigFromKubeConfig(adminCfgBytes)
	if err != nil {
		return nil, fmt.Errorf("build rest config from admin kubeconfig: %w", err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("create clientset from admin kubeconfig: %w", err)
	}
	return cs, nil
}

func fetchClusterAdminKubeconfig(ctx context.Context, cfg *config.Config) ([]byte, error) {
	cred, err := auth.NewAuthProvider().UserCredential(cfg)
	if err != nil {
		return nil, fmt.Errorf("get credential: %w", err)
	}

	subID := cfg.GetTargetClusterSubscriptionID()
	if subID == "" {
		return nil, errors.New("target cluster subscription ID is empty")
	}

	mcClient, err := armcontainerservice.NewManagedClustersClient(subID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("create managed clusters client: %w", err)
	}

	clusterRG := cfg.GetTargetClusterResourceGroup()
	clusterName := cfg.GetTargetClusterName()
	if clusterRG == "" || clusterName == "" {
		return nil, errors.New("target cluster resource group/name is empty")
	}

	resp, err := mcClient.ListClusterAdminCredentials(ctx, clusterRG, clusterName, nil)
	if err != nil {
		return nil, fmt.Errorf("list cluster admin credentials for %s/%s: %w", clusterRG, clusterName, err)
	}
	if len(resp.Kubeconfigs) == 0 || resp.Kubeconfigs[0] == nil || len(resp.Kubeconfigs[0].Value) == 0 {
		return nil, errors.New("cluster admin kubeconfig was empty")
	}
	return resp.Kubeconfigs[0].Value, nil
}
