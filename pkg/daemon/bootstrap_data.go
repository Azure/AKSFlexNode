package daemon

import (
	"context"

	"github.com/Azure/AKSFlexNode/pkg/bootstrapdata"
	"github.com/Azure/AKSFlexNode/pkg/config"
)

type bootstrapDataRefresher interface {
	Fetch(context.Context, *config.Config) (*bootstrapdata.Data, error)
}

type noopBootstrapDataRefresher struct{}

func (noopBootstrapDataRefresher) Fetch(context.Context, *config.Config) (*bootstrapdata.Data, error) {
	return nil, nil
}

type aksBootstrapDataRefresher struct{}

func (aksBootstrapDataRefresher) Fetch(ctx context.Context, cfg *config.Config) (*bootstrapdata.Data, error) {
	options, err := bootstrapdata.OptionsFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	return bootstrapdata.Fetch(ctx, options)
}

func bootstrapDataRefresherForConfig(cfg *config.Config) bootstrapDataRefresher {
	if cfg.NeedsBootstrapDataRefresh() {
		return aksBootstrapDataRefresher{}
	}
	return noopBootstrapDataRefresher{}
}
