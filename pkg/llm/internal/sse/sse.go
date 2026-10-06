// Package sse reads Server-Sent Events for the pkg/llm stream decoders.
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
)

// Reader reads SSE events one at a time. A line longer than max bytes fails
// the read (the cap resets per line), so a vendor that never ends a line
// cannot grow the heap.
type Reader struct {
	br      *bufio.Reader
	max     int
	tooLong error
	eof     bool
}

// NewReader reads events from r; a line over max bytes returns tooLong.
func NewReader(r io.Reader, max int, tooLong error) *Reader {
	return &Reader{br: bufio.NewReader(r), max: max, tooLong: tooLong}
}

// Next returns the next event: its "event:" name ("" if none) and its
// "data:" lines joined by "\n". Comments (":") and other fields are ignored,
// and CRLF line ends are accepted. An event cut by the end of the stream
// (no blank line after it) is still returned; io.EOF follows. Any other read
// error is returned as is, discarding the partial event.
func (s *Reader) Next() (event, data string, err error) {
	var lines []string
	for {
		if s.eof {
			if lines != nil {
				return event, strings.Join(lines, "\n"), nil
			}
			return "", "", io.EOF
		}
		line, rerr := s.line()
		if rerr == io.EOF {
			s.eof = true
		} else if rerr != nil {
			return "", "", rerr
		}
		switch {
		case line == "":
			if lines != nil && !s.eof {
				return event, strings.Join(lines, "\n"), nil
			}
			if !s.eof {
				event = "" // a blank line ends an event with no data
			}
		case strings.HasPrefix(line, "data:"):
			lines = append(lines, strings.TrimPrefix(line[5:], " "))
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[6:])
		}
	}
}

// line reads one line without its line end, capped at s.max bytes.
func (s *Reader) line() (string, error) {
	var buf []byte
	for {
		frag, err := s.br.ReadSlice('\n')
		if len(buf)+len(frag) > s.max+2 { // +2: the CRLF itself
			return "", s.tooLong
		}
		buf = append(buf, frag...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return string(bytes.TrimRight(buf, "\r\n")), err
	}
}

// Checked returns a stream of r's events, re-framed one by one, each passed
// to check first: check's error ends the stream with that error after the
// events before it. An event cut by the end of r is re-framed whole, so a
// reader of the result sees the same events, cut one included, then io.EOF.
// Lines over max bytes fail with tooLong, as for NewReader.
func Checked(r io.Reader, max int, tooLong error, check func(data []byte) error) io.Reader {
	return &checked{src: NewReader(r, max, tooLong), check: check}
}

type checked struct {
	src   *Reader
	check func([]byte) error
	buf   []byte
	err   error
}

func (c *checked) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		if c.err != nil {
			return 0, c.err
		}
		event, data, err := c.src.Next()
		if err == nil {
			err = c.check([]byte(data))
		}
		if err != nil {
			c.err = err
			continue
		}
		if event != "" {
			c.buf = append(append(append(c.buf, "event: "...), event...), '\n')
		}
		for _, line := range strings.Split(data, "\n") {
			c.buf = append(append(append(c.buf, "data: "...), line...), '\n')
		}
		c.buf = append(c.buf, '\n')
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}
