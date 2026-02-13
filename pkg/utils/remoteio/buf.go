package remoteio

import (
	"errors"
	"fmt"
	"io"
)

var ErrFileTooLarge = errors.New("file exceeds maximum allowed size")

// ReadAll1GiB reads all data from the provided reader, but returns an error if the data exceeds 1 GiB in size.
func ReadAll1GiB(r io.Reader) ([]byte, error) {
	const maxFileSize = 1 * 1024 * 1024 * 1024 // 1 GiB

	lr := io.LimitReader(r, maxFileSize+1) // +1 to detect if file exceeds limited size
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxFileSize {
		return nil, fmt.Errorf("%w: 4 GiB", ErrFileTooLarge)
	}
	return data, nil
}
