package agents

import (
	"bufio"
	"errors"
)

const MaxAgentLineBytes = 64 << 20

var ErrLineTooLarge = errors.New("单条记录超过读取上限，请在电脑查看")

// ReadBoundedLine reads at most limit bytes (including the newline). ReadSlice
// fragments are bounded by the reader buffer; oversized records are drained
// through their newline without retaining the rest. The following call starts
// at the next complete record, not in the middle of discarded JSON.
// At EOF an ordinary final unterminated record is returned with io.EOF.
func ReadBoundedLine(reader *bufio.Reader, limit int) ([]byte, error) {
	return readBoundedLine(reader, limit, nil)
}

func readBoundedLine(reader *bufio.Reader, limit int, onOversized func()) ([]byte, error) {
	if limit <= 0 || limit > MaxAgentLineBytes {
		limit = MaxAgentLineBytes
	}
	var line []byte
	tooLarge := false
	for {
		fragment, err := reader.ReadSlice('\n')
		if !tooLarge {
			if len(fragment) > limit-len(line) {
				line = nil
				tooLarge = true
				if onOversized != nil {
					onOversized()
				}
			} else {
				required := len(line) + len(fragment)
				if required > cap(line) {
					capacity := cap(line) * 2
					if capacity < required {
						capacity = required
					}
					if capacity > limit {
						capacity = limit
					}
					next := make([]byte, len(line), capacity)
					copy(next, line)
					line = next
				}
				line = append(line, fragment...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if tooLarge {
			return nil, ErrLineTooLarge
		}
		return line, err
	}
}
