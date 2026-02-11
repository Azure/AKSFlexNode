package units

type systemdManager interface {
	ReloadDaemon() error
	StartUnit(name string) error
	RestartUnit(name string) error
	ReloadUnit(name string) error

	GenerateDeltas(oldPath, newPath string) (*systemdUnitDeltas, error)
}

type systemdUnitDeltas struct {
	UnitToStart   []string // do we need this?
	UnitToRestart []string
	UnitToReload  []string
}
