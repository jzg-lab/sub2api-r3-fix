package transport

import (
	"bufio"
	"io"
)

// A framed Responses terminal settles the probe even if the upstream keeps the
// HTTP body open. EOF or a deadline alone never certifies completion.
func readProbeSSE(body io.Reader, limit int) (raw []byte, observation responseObserver, err error) {
	observation.sse = true
	reader := bufio.NewReader(io.LimitReader(body, int64(limit)+1))
	for {
		line, readErr := reader.ReadSlice('\n')
		raw = append(raw, line...)
		if len(raw) > limit {
			return raw, observation, nil
		}
		observation.write(line)
		if observation.settledSSE() {
			return raw, observation, nil
		}
		switch readErr {
		case nil, bufio.ErrBufferFull:
		case io.EOF:
			return raw, observation, nil
		default:
			return raw, observation, readErr
		}
	}
}
