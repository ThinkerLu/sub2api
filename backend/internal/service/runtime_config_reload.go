package service

import (
	"errors"
	"fmt"
)

// RuntimeConfigReloader is implemented by process-wide configuration snapshots
// that can be replaced without restarting the service.
type RuntimeConfigReloader interface {
	Name() string
	Reload() error
}

// ReloadRuntimeConfigs reloads every registered snapshot in order. Each reload
// keeps its previous valid snapshot on failure, and all errors are returned
// together so one broken file does not prevent the others from being reloaded.
func ReloadRuntimeConfigs(reloaders ...RuntimeConfigReloader) error {
	var errs []error
	for _, reloader := range reloaders {
		if reloader == nil {
			continue
		}
		if err := reloader.Reload(); err != nil {
			errs = append(errs, fmt.Errorf("reload %s: %w", reloader.Name(), err))
		}
	}
	return errors.Join(errs...)
}
