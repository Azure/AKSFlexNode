package utilaz

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

type CredentialErrorReporter struct {
	CredentialType string
	Err            error
}

func (c *CredentialErrorReporter) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{}, azidentity.NewCredentialUnavailableError(
		fmt.Sprintf("%s credential unavailable: %v", c.CredentialType, c.Err),
	)
}
