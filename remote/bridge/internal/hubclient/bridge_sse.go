package hubclient

import (
	"bufio"
	"errors"
	"io"
	"strings"

	"salcara/bridge/internal/protocol"
)

var errBridgeSSETooLarge = errors.New("服务器事件超过读取上限")

// Budget the complete frame, not only an individual data line. A source
// envelope may contain the full escaped station policy and a large command.
func readBridgeSSE(body io.Reader, traffic func(), event func(string, string)) error {
	rd := bufio.NewReaderSize(body, 64*1024)
	var name string
	var data strings.Builder
	frameBytes := 0
	for {
		var line []byte
		for {
			part, err := rd.ReadSlice('\n')
			if len(part) > protocol.MaxSourcePolicyWireBytes-frameBytes-len(line) {
				return errBridgeSSETooLarge
			}
			line = append(line, part...)
			if err == bufio.ErrBufferFull {
				continue
			}
			if err != nil {
				return err
			}
			break
		}
		frameBytes += len(line)
		traffic()
		value := strings.TrimRight(string(line), "\r\n")
		switch {
		case value == "":
			if data.Len() > 0 {
				event(name, data.String())
			}
			name = ""
			data.Reset()
			frameBytes = 0
		case strings.HasPrefix(value, ":"):
			// Keep-alive comments count toward the same frame budget.
		case strings.HasPrefix(value, "event:"):
			name = strings.TrimSpace(value[6:])
		case strings.HasPrefix(value, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(value[5:], " "))
		}
	}
}
