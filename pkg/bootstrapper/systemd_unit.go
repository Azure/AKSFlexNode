package bootstrapper

import (
	"context"
	"errors"

	"github.com/sirupsen/logrus"

	"github.com/Azure/AKSFlexNode/pkg/systemd"
)

// SystemdUnitStopDisableExecutor ensures a systemd unit is stopped (if active) and disabled.
type SystemdUnitStopDisableExecutor struct {
	name     string
	unitName string

	systemd systemd.Manager
	logger  *logrus.Logger
}

var _ Executor = (*SystemdUnitStopDisableExecutor)(nil)

func NewSystemdUnitStopDisableExecutor(
	name string,
	unitName string,
	logger *logrus.Logger,
) *SystemdUnitStopDisableExecutor {
	return &SystemdUnitStopDisableExecutor{
		name:     name,
		unitName: unitName,
		systemd:  systemd.New(),
		logger:   logger,
	}
}

func (e *SystemdUnitStopDisableExecutor) GetName() string {
	return e.name
}

func (e *SystemdUnitStopDisableExecutor) Execute(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	if e.logger != nil {
		e.logger.Infof("Ensuring systemd unit stopped+disabled: %s", e.unitName)
	}

	return systemd.EnsureUnitStoppedAndDisabled(ctx, e.systemd, e.unitName)
}

func (e *SystemdUnitStopDisableExecutor) IsCompleted(ctx context.Context) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	st, err := e.systemd.GetUnitStatus(ctx, e.unitName)
	if errors.Is(err, systemd.ErrUnitNotFound) {
		return true
	}
	if err != nil {
		return false
	}
	return st.ActiveState != systemd.UnitActiveStateActive
}
