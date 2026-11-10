package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"syscall"
)

// OpenRegular opens a transcript for reading, and refuses anything but a regular
// file. A hook supplies these paths, and the daemon reads them: a pipe would block it
// on open and a device could feed it forever. Opening without blocking, then checking
// what was opened, leaves no gap between the check and the read.
func OpenRegular(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", path)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// readRegular reads a small file through OpenRegular, up to MaxLine bytes.
func readRegular(path string) ([]byte, error) {
	f, err := OpenRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, int64(MaxLine)))
}

// MaxLine is the longest transcript record aiq reads, terminal and web alike.
// It is set high: the large records are tool results carrying images, whose
// content the parsers skip anyway. A parser reads past a longer record and
// counts it in Skipped, so whoever shows the session can say one was left out.
var MaxLine = 32 << 20

// readLine reads one line into *buf, which keeps its storage for the next.
// A line longer than MaxLine is read to its end without being kept past
// MaxLine bytes, and over is set: line is then only its opening.
func readLine(r *bufio.Reader, buf *[]byte) (line []byte, over bool, err error) {
	b := (*buf)[:0]
	for {
		var chunk []byte
		chunk, err = r.ReadSlice('\n')
		if !over {
			if len(b)+len(chunk) > MaxLine {
				b, over = append(b, chunk[:max(MaxLine-len(b), 0)]...), true
			} else {
				b = append(b, chunk...)
			}
		}
		if err != bufio.ErrBufferFull {
			*buf = b
			return b, over, err
		}
	}
}

// ClaudeTitle reads the latest saved display name without parsing the
// conversation. A rename can occur long after the session's launch.
func ClaudeTitle(path string) string {
	f, err := OpenRegular(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	title := ""
	r := bufio.NewReader(f)
	var buf []byte
	for {
		line, over, err := readLine(r, &buf)
		if !over && bytes.Contains(line, []byte(`"custom-title"`)) {
			var rec struct {
				Type  string `json:"type"`
				Title string `json:"customTitle"`
			}
			if json.Unmarshal(line, &rec) == nil && rec.Type == "custom-title" {
				title = rec.Title
			}
		}
		if err != nil {
			if err != io.EOF {
				return ""
			}
			return title
		}
	}
}

// CodexTitle reads the last name recorded for id in the account's index.
// The index is shared with the real home by account overlays.
func CodexTitle(index, id string) string {
	return codexNames([]string{index})[id]
}
