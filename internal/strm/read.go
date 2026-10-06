package strm

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// MaxFileSize caps how much of a .strm file is read. Real ones are one line
// (~100 bytes); anything large isn't a .strm file.
const MaxFileSize = 64 << 10

// ErrEmpty means a .strm file contains no target.
var ErrEmpty = errors.New("strm: file has no target")

// ReadFile returns the playback target of a .strm file: its first line that
// is neither blank nor a "#" comment, trimmed (DESIGN §6). The target must be
// an absolute URL (http, https, rtsp, …) or an absolute file path.
func ReadFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }() // read-only; a close error can't lose data
	return Read(f)
}

// Read is ReadFile for any reader.
func Read(r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxFileSize+1))
	if err != nil {
		return "", err
	}
	if len(data) > MaxFileSize {
		return "", fmt.Errorf("strm: file larger than %d bytes", MaxFileSize)
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // UTF-8 BOM from Windows editors
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, len(data)+1), MaxFileSize+1)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if err := validTarget(line); err != nil {
			return "", err
		}
		return line, nil
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", ErrEmpty
}

func validTarget(t string) error {
	if filepath.IsAbs(t) {
		return nil
	}
	// scheme://host is required: "jellybird:8097/stream" parses as scheme
	// "jellybird" with an opaque path, which is a missing "http://", not a URL.
	u, err := url.Parse(t)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("strm: target %q is neither an absolute URL nor an absolute path", t)
	}
	return nil
}

// IsRemote reports whether a .strm target is a URL (as opposed to a local path).
func IsRemote(target string) bool { return !filepath.IsAbs(target) }
