package utilio

import "os"

func IsExecutable(filePath string) bool {
	s, err := os.Stat(filePath)
	if err != nil {
		return false
	}
	return !s.IsDir() && s.Mode()&0111 != 0
}
