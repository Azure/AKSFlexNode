package spec

import "os"

// RemoveManagedClusterSpecSnapshot removes the managed cluster spec snapshot file.
//
// It returns (removed=false, err=nil) when the file does not exist.
func RemoveManagedClusterSpecSnapshot() (removed bool, err error) {
	return RemoveManagedClusterSpecSnapshotAtPath(GetManagedClusterSpecFilePath())
}

// RemoveManagedClusterSpecSnapshotAtPath removes the managed cluster spec snapshot file at the given path.
//
// It returns (removed=false, err=nil) when the file does not exist.
func RemoveManagedClusterSpecSnapshotAtPath(path string) (removed bool, err error) {
	if path == "" {
		return false, nil
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
