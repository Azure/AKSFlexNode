package arc

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
)

const azureConfigDir = config.ConfigDir + "/azure"

var azureAuthFiles = []string{
	"azureProfile.json",
	"msal_token_cache.json",
	"msal_token_cache.bin",
	"clouds.config",
}

// copyAzureCLIAuth copies only Azure CLI auth files from the current user's
// $HOME/.azure into a root-owned directory. During start this is expected to run
// as root, so the source is intentionally /root/.azure rather than SUDO_USER's
// profile.
func copyAzureCLIAuth() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	sourceDir := filepath.Join(home, ".azure")

	if err := os.MkdirAll(azureConfigDir, 0o700); err != nil {
		return fmt.Errorf("create azure config dir %s: %w", azureConfigDir, err)
	}
	if err := os.Chown(azureConfigDir, 0, 0); err != nil {
		return fmt.Errorf("chown azure config dir %s: %w", azureConfigDir, err)
	}
	if err := os.Chmod(azureConfigDir, 0o700); err != nil { // #nosec G302 -- directory must be traversable by root and inaccessible to other users
		return fmt.Errorf("chmod azure config dir %s: %w", azureConfigDir, err)
	}

	for _, name := range azureAuthFiles {
		sourcePath := filepath.Join(sourceDir, name)
		data, err := os.ReadFile(filepath.Clean(sourcePath)) // #nosec G304 -- fixed auth filenames under $HOME/.azure
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read azure auth file %s: %w", sourcePath, err)
		}

		destPath := filepath.Join(azureConfigDir, name)
		if err := utilio.WriteFile(destPath, data, 0o600); err != nil {
			return fmt.Errorf("write azure auth file %s: %w", destPath, err)
		}
		if err := os.Chown(destPath, 0, 0); err != nil {
			return fmt.Errorf("chown azure auth file %s: %w", destPath, err)
		}
		if err := os.Chmod(destPath, 0o600); err != nil {
			return fmt.Errorf("chmod azure auth file %s: %w", destPath, err)
		}
	}

	return nil
}
