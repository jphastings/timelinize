/*
	Timelinize
	Copyright (c) 2013 Matthew Holt

	This program is free software: you can redistribute it and/or modify
	it under the terms of the GNU Affero General Public License as published
	by the Free Software Foundation, either version 3 of the License, or
	(at your option) any later version.

	This program is distributed in the hope that it will be useful,
	but WITHOUT ANY WARRANTY; without even the implied warranty of
	MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
	GNU Affero General Public License for more details.

	You should have received a copy of the GNU Affero General Public License
	along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

// Borrowed from Caddy in early 2024

package tlzapp

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
)

// responseWriterWrapper wraps an underlying ResponseWriter and
// promotes its Pusher method as well. To use this type, embed
// a pointer to it within your own struct type that implements
// the http.ResponseWriter interface, then call methods on the
// embedded value.
type responseWriterWrapper struct {
	http.ResponseWriter
}

// Push implements http.Pusher. It simply calls the underlying
// ResponseWriter's Push method if there is one, or returns
// ErrNotImplemented otherwise.
func (rww *responseWriterWrapper) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := rww.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return ErrNotImplemented
}

// ReadFrom implements io.ReaderFrom. It simply calls io.Copy,
// which uses io.ReaderFrom if available.
func (rww *responseWriterWrapper) ReadFrom(r io.Reader) (n int64, err error) {
	return io.Copy(rww.ResponseWriter, r)
}

// Unwrap returns the underlying ResponseWriter, necessary for
// http.ResponseController to work correctly.
func (rww *responseWriterWrapper) Unwrap() http.ResponseWriter {
	return rww.ResponseWriter
}

// ErrNotImplemented is returned when an underlying
// ResponseWriter does not implement the required method.
var ErrNotImplemented = errors.New("method not implemented")

type responseRecorder struct {
	*responseWriterWrapper
	statusCode   int
	buf          *bytes.Buffer
	shouldBuffer shouldBufferFunc
	size         int
	wroteHeader  bool
	stream       bool
}

// newResponseRecorder returns a new ResponseRecorder that can be
// used instead of a standard http.ResponseWriter. The recorder is
// useful for middlewares which need to buffer a response and
// potentially process its entire body before actually writing the
// response to the underlying writer. Of course, buffering the entire
// body has a memory overhead, but sometimes there is no way to avoid
// buffering the whole response, hence the existence of this type.
// Still, if at all practical, handlers should strive to stream
// responses by wrapping Write and WriteHeader methods instead of
// buffering whole response bodies.
//
// Buffering is actually optional. The shouldBuffer function will
// be called just before the headers are written. If it returns
// true, the headers and body will be buffered by this recorder
// and not written to the underlying writer; if false, the headers
// will be written immediately and the body will be streamed out
// directly to the underlying writer. If shouldBuffer is nil,
// the response will never be buffered and will always be streamed
// directly to the writer.
//
// You can know if shouldBuffer returned true by calling Buffered().
//
// The provided buffer buf should be obtained from a pool for best
// performance (see the sync.Pool type).
//
// Proper usage of a recorder looks like this:
//
//	rec := caddyhttp.NewResponseRecorder(w, buf, shouldBuffer)
//	err := next.ServeHTTP(rec, req)
//	if err != nil {
//	    return err
//	}
//	if !rec.Buffered() {
//	    return nil
//	}
//	// process the buffered response here
//
// The header map is not buffered; i.e. the ResponseRecorder's Header()
// method returns the same header map of the underlying ResponseWriter.
// This is a crucial design decision to allow HTTP trailers to be
// flushed properly (https://github.com/caddyserver/caddy/issues/3236).
//
// Once you are ready to write the response, there are two ways you can
// do it. The easier way is to have the recorder do it:
//
//	rec.WriteResponse()
//
// This writes the recorded response headers as well as the buffered body.
// Or, you may wish to do it yourself, especially if you manipulated the
// buffered body. First you will need to write the headers with the
// recorded status code, then write the body (this example writes the
// recorder's body buffer, but you might have your own body to write
// instead):
//
//	w.WriteHeader(rec.Status())
//	io.Copy(w, rec.Buffer())
//
// As a special case, 1xx responses are not buffered nor recorded
// because they are not the final response; they are passed through
// directly to the underlying ResponseWriter.
func newResponseRecorder(w http.ResponseWriter, buf *bytes.Buffer, shouldBuffer shouldBufferFunc) *responseRecorder {
	return &responseRecorder{
		responseWriterWrapper: &responseWriterWrapper{ResponseWriter: w},
		buf:                   buf,
		shouldBuffer:          shouldBuffer,
	}
}

// WriteHeader writes the headers with statusCode to the wrapped
// ResponseWriter unless the response is to be buffered instead.
// 1xx responses are never buffered.
func (rr *responseRecorder) WriteHeader(statusCode int) {
	if rr.wroteHeader {
		return
	}

	// save statusCode always, in case HTTP middleware upgrades websocket
	// connections by manually setting headers and writing status 101
	rr.statusCode = statusCode

	// 1xx responses aren't final; just informational
	if statusCode < 100 || statusCode > 199 {
		rr.wroteHeader = true

		// decide whether we should buffer the response
		if rr.shouldBuffer == nil {
			rr.stream = true
		} else {
			rr.stream = !rr.shouldBuffer(rr.statusCode, rr.responseWriterWrapper.Header())
		}
	}

	// if informational or not buffered, immediately write header
	if rr.stream || (100 <= statusCode && statusCode <= 199) {
		rr.responseWriterWrapper.WriteHeader(statusCode)
	}
}

func (rr *responseRecorder) Write(data []byte) (int, error) {
	rr.WriteHeader(http.StatusOK)
	var n int
	var err error
	if rr.stream {
		n, err = rr.responseWriterWrapper.Write(data)
	} else {
		n, err = rr.buf.Write(data)
	}

	rr.size += n
	return n, err
}

func (rr *responseRecorder) ReadFrom(r io.Reader) (int64, error) {
	rr.WriteHeader(http.StatusOK)
	var n int64
	var err error
	if rr.stream {
		n, err = rr.responseWriterWrapper.ReadFrom(r)
	} else {
		n, err = rr.buf.ReadFrom(r)
	}

	rr.size += int(n)
	return n, err
}

// Status returns the status code that was written, if any.
func (rr *responseRecorder) Status() int {
	return rr.statusCode
}

// Size returns the number of bytes written,
// not including the response headers.
func (rr *responseRecorder) Size() int {
	return rr.size
}

// Buffer returns the body buffer that rr was created with.
// You should still have your original pointer, though.
func (rr *responseRecorder) Buffer() *bytes.Buffer {
	return rr.buf
}

// Buffered returns whether rr has decided to buffer the response.
func (rr *responseRecorder) Buffered() bool {
	return !rr.stream
}

func (rr *responseRecorder) WriteResponse() error {
	if rr.stream {
		return nil
	}
	if rr.statusCode == 0 {
		// could happen if no handlers actually wrote anything,
		// and this prevents a panic; status must be > 0
		rr.statusCode = http.StatusOK
	}
	rr.responseWriterWrapper.WriteHeader(rr.statusCode)
	_, err := io.Copy(rr.responseWriterWrapper, rr.buf)
	return err
}

func (rr *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := http.NewResponseController(rr.responseWriterWrapper).Hijack()
	if err != nil {
		return nil, nil, err
	}
	// Per http documentation, returned bufio.Writer is empty, but bufio.Read maybe not
	conn = &hijackedConn{conn, rr}
	brw.Writer.Reset(conn)
	return conn, brw, nil
}

// used to track the size of hijacked response writers
type hijackedConn struct {
	net.Conn
	rr *responseRecorder
}

func (hc *hijackedConn) Write(p []byte) (int, error) {
	n, err := hc.Conn.Write(p)
	hc.rr.size += n
	return n, err
}

func (hc *hijackedConn) ReadFrom(r io.Reader) (int64, error) {
	n, err := io.Copy(hc.Conn, r)
	hc.rr.size += int(n)
	return n, err
}

// shouldBufferFunc is a function that returns true if the
// response should be buffered, given the pending HTTP status
// code and response headers.
type shouldBufferFunc func(status int, header http.Header) bool

// Interface guards
var (
	_ http.ResponseWriter = (*responseWriterWrapper)(nil)

	// Implementing ReaderFrom can be such a significant
	// optimization that it should probably be required!
	// see PR #5022 (25%-50% speedup)
	_ io.ReaderFrom = (*responseWriterWrapper)(nil)
	_ io.ReaderFrom = (*responseRecorder)(nil)
	_ io.ReaderFrom = (*hijackedConn)(nil)
)
