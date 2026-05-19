package utilaz

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

type CredentialErrorReporter struct {
	CredentialType string
	Err            error
}

func (c *CredentialErrorReporter) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{}, fmt.Errorf("%s credential unavailable: %w", c.CredentialType, c.Err)
}
