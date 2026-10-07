//go:build !unix

package transcode

import (
	"errors"
	"os"
)

// Without job-control signals sessions aren't throttled.
func pauseProcess(*os.Process) error  { return errors.ErrUnsupported }
func resumeProcess(*os.Process) error { return errors.ErrUnsupported }
