package agentruntime

import (
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/noneops"
)

type bootstrapArtifacts struct {
	none bool
	oci  bool
}

func inspectBootstrapArtifacts() (bootstrapArtifacts, error) {
	noneArtifacts, err := NoneUpgradeArtifactsPresent()
	if err != nil {
		return bootstrapArtifacts{}, err
	}
	nonePresent := noneArtifacts || noneops.Present()

	ociArtifacts, err := ContainerUpgradeArtifactPresent()
	if err != nil {
		return bootstrapArtifacts{}, err
	}

	return bootstrapArtifacts{
		none: nonePresent,
		oci:  ociArtifacts || containerops.Present(),
	}, nil
}
