package fasthttp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"sync"
)

// Request represents HTTP request.
//
// It is forbidden copying Request instances. Create new instances
// and use CopyTo() instead.
//
// It is unsafe modifying/reading Request instance from concurrently
// running goroutines.
type Request struct {
	// Request header
	//
	// Copying Header by value is forbidden. Use pointer to Header instead.
	Header RequestHeader

	body []byte
	w    requestBodyWriter

	uri       URI
	parsedURI bool

	postArgs       Args
	parsedPostArgs bool

	multipartForm *multipart.Form
}

// Response represents HTTP response.
//
// It is forbidden copying Response instances. Create new instances
// and use CopyTo() instead.
//
// It is unsafe modifying/reading Response instance from concurrently
// running goroutines.
type Response struct {
	// Response header
	//
	// Copying Header by value is forbidden. Use pointer to Header instead.
	Header ResponseHeader

	// Response.Read() skips reading body if set to true.
	// Use it for HEAD requests.
	SkipBody bool

	body []byte
	w    responseBodyWriter

	bodyStream io.Reader
}

// SetRequestURI sets RequestURI.
func (req *Request) SetRequestURI(requestURI string) {
	req.Header.SetRequestURI(requestURI)
}

// SetRequestURIBytes sets RequestURI.
func (req *Request) SetRequestURIBytes(requestURI []byte) {
	req.Header.SetRequestURIBytes(requestURI)
}

// StatusCode returns response status code.
func (resp *Response) StatusCode() int {
	return resp.Header.StatusCode()
}

// SetStatusCode sets response status code.
func (resp *Response) SetStatusCode(statusCode int) {
	resp.Header.SetStatusCode(statusCode)
}

// ConnectionClose returns true if 'Connection: close' header is set.
func (resp *Response) ConnectionClose() bool {
	return resp.Header.ConnectionClose()
}

// SetConnectionClose sets 'Connection: close' header.
func (resp *Response) SetConnectionClose() {
	resp.Header.SetConnectionClose()
}

// ConnectionClose returns true if 'Connection: close' header is set.
func (req *Request) ConnectionClose() bool {
	return req.Header.ConnectionClose()
}

// SetConnectionClose sets 'Connection: close' header.
func (req *Request) SetConnectionClose() {
	req.Header.SetConnectionClose()
}

// SetBodyStream sets response body stream and, optionally body size.
//
// If bodySize is >= 0, then bodySize bytes are read from bodyStream
// and used as response body.
//
// If bodySize < 0, then bodyStream is read until io.EOF.
//
// bodyStream.Close() is called after finishing reading all body data
// if it implements io.Closer.
func (resp *Response) SetBodyStream(bodyStream io.Reader, bodySize int) {
	resp.body = resp.body[:0]
	resp.bodyStream = bodyStream
	resp.Header.SetContentLength(bodySize)
}

// BodyWriter returns writer for populating response body.
func (resp *Response) BodyWriter() io.Writer {
	resp.w.r = resp
	return &resp.w
}

// BodyWriter returns writer for populating request body.
func (req *Request) BodyWriter() io.Writer {
	req.w.r = req
	return &req.w
}

type responseBodyWriter struct {
	r *Response
}

func (w *responseBodyWriter) Write(p []byte) (int, error) {
	w.r.body = append(w.r.body, p...)
	return len(p), nil
}

type requestBodyWriter struct {
	r *Request
}

func (w *requestBodyWriter) Write(p []byte) (int, error) {
	w.r.body = append(w.r.body, p...)
	return len(p), nil
}

// Body returns response body.
func (resp *Response) Body() []byte {
	return resp.body
}

// SetBody sets response body.
func (resp *Response) SetBody(body []byte) {
	resp.bodyStream = nil
	resp.body = append(resp.body[:0], body...)
}

// ResetBody resets response body.
func (resp *Response) ResetBody() {
	resp.bodyStream = nil
	resp.body = resp.body[:0]
}

// Body returns request body.
func (req *Request) Body() []byte {
	return req.body
}

// SetBody sets request body.
func (req *Request) SetBody(body []byte) {
	req.body = append(req.body[:0], body...)
}

// ResetBody resets request body.
func (req *Request) ResetBody() {
	req.body = req.body[:0]
}

// CopyTo copies req contents to dst.
func (req *Request) CopyTo(dst *Request) {
	dst.Reset()
	req.Header.CopyTo(&dst.Header)
	dst.body = append(dst.body[:0], req.body...)

	req.uri.CopyTo(&dst.uri)
	dst.parsedURI = req.parsedURI

	req.postArgs.CopyTo(&dst.postArgs)
	dst.parsedPostArgs = req.parsedPostArgs

	// do not copy multipartForm - it will be automatically
	// re-created on the first call to MultipartForm.
}

// CopyTo copies resp contents to dst except of BodyStream.
func (resp *Response) CopyTo(dst *Response) {
	dst.Reset()
	resp.Header.CopyTo(&dst.Header)
	dst.body = append(dst.body[:0], resp.body...)
	dst.SkipBody = resp.SkipBody
}

// URI returns request URI
func (req *Request) URI() *URI {
	req.parseURI()
	return &req.uri
}

func (req *Request) parseURI() {
	if req.parsedURI {
		return
	}
	req.parsedURI = true

	req.uri.parseQuick(req.Header.RequestURI(), &req.Header)
}

// PostArgs returns POST arguments.
func (req *Request) PostArgs() *Args {
	req.parsePostArgs()
	return &req.postArgs
}

func (req *Request) parsePostArgs() {
	if req.parsedPostArgs {
		return
	}
	req.parsedPostArgs = true

	if !req.Header.IsPost() {
		return
	}
	if !bytes.Equal(req.Header.ContentType(), strPostArgsContentType) {
		return
	}
	req.postArgs.ParseBytes(req.body)
	return
}

// ErrNoMultipartForm means that the request's Content-Type
// isn't 'multipart/form-data'.
var ErrNoMultipartForm = errors.New("request has no multipart/form-data Content-Type")

// MultipartForm returns requests's multipart form.
//
// Returns ErrNoMultipartForm if request's content-type
// isn't 'multipart/form-data'.
func (req *Request) MultipartForm() (*multipart.Form, error) {
	if req.multipartForm != nil {
		return req.multipartForm, nil
	}

	boundary := req.Header.MultipartFormBoundary()
	if len(boundary) == 0 {
		return nil, ErrNoMultipartForm
	}
	f, err := readMultipartFormBody(bytes.NewReader(req.body), boundary, 0, len(req.body))
	if err != nil {
		return nil, err
	}
	req.multipartForm = f
	return f, nil
}

func readMultipartFormBody(r io.Reader, boundary []byte, maxBodySize, maxInMemoryFileSize int) (*multipart.Form, error) {
	// Do not care about memory allocations here, since they are tiny
	// compared to multipart data (aka multi-MB files) usually sent
	// in multipart/form-data requests.

	if maxBodySize > 0 {
		r = io.LimitReader(r, int64(maxBodySize))
	}
	mr := multipart.NewReader(r, string(boundary))
	f, err := mr.ReadForm(int64(maxInMemoryFileSize))
	if err != nil {
		return nil, fmt.Errorf("cannot read multipart/form-data body: %s", err)
	}
	return f, nil
}

// Reset clears request contents.
func (req *Request) Reset() {
	req.Header.Reset()
	req.clearSkipHeader()
}

func (req *Request) clearSkipHeader() {
	req.body = req.body[:0]
	req.uri.Reset()
	req.parsedURI = false
	req.postArgs.Reset()
	req.parsedPostArgs = false
	req.RemoveMultipartFormFiles()
}

// RemoveMultipartFormFiles removes multipart/form-data temporary files
// associated with the request.
func (req *Request) RemoveMultipartFormFiles() {
	if req.multipartForm != nil {
		// Do not check for error, since these files may be deleted or moved
		// to new places by user code.
		req.multipartForm.RemoveAll()
		req.multipartForm = nil
	}
}

// Reset clears response contents.
func (resp *Response) Reset() {
	resp.Header.Reset()
	resp.clearSkipHeader()
	resp.SkipBody = false
}

func (resp *Response) clearSkipHeader() {
	resp.body = resp.body[:0]
	resp.bodyStream = nil
}

// Read reads request (including body) from the given r.
//
// RemoveMultipartFormFiles or Reset must be called after
// reading multipart/form-data request in order to delete temporarily
// uploaded files.
func (req *Request) Read(r *bufio.Reader) error {
	return req.ReadLimitBody(r, 0)
}

const defaultMaxInMemoryFileSize = 16 * 1024 * 1024

var errGetOnly = errors.New("non-GET request received")

// ReadLimitBody reads request from the given r, limiting the body size.
//
// If maxBodySize > 0 and the body size exceeds maxBodySize,
// then ErrBodyTooLarge is returned.
//
// RemoveMultipartFormFiles or Reset must be called after
// reading multipart/form-data request in order to delete temporarily
// uploaded files.
func (req *Request) ReadLimitBody(r *bufio.Reader, maxBodySize int) error {
	return req.readLimitBody(r, maxBodySize, false)
}

func (req *Request) readLimitBody(r *bufio.Reader, maxBodySize int, getOnly bool) error {
	req.clearSkipHeader()
	err := req.Header.Read(r)
	if err != nil {
		return err
	}
	if getOnly && !req.Header.IsGet() {
		return errGetOnly
	}

	if !req.Header.noBody() {
		contentLength := req.Header.ContentLength()
		if contentLength > 0 {
			// Pre-read multipart form data of known length.
			// This way we limit memory usage for large file uploads, since their contents
			// is streamed into temporary files if file size exceeds defaultMaxInMemoryFileSize.
			boundary := req.Header.MultipartFormBoundary()
			if len(boundary) > 0 {
				req.multipartForm, err = readMultipartFormBody(r, boundary, maxBodySize, defaultMaxInMemoryFileSize)
				if err != nil {
					req.Reset()
				}
				return err
			}
		}

		req.body, err = readBody(r, contentLength, maxBodySize, req.body)
		if err != nil {
			req.Reset()
			return err
		}
		req.Header.SetContentLength(len(req.body))
	}
	return nil
}

// Read reads response (including body) from the given r.
func (resp *Response) Read(r *bufio.Reader) error {
	return resp.ReadLimitBody(r, 0)
}

// ReadLimitBody reads response from the given r, limiting the body size.
//
// If maxBodySize > 0 and the body size exceeds maxBodySize,
// then ErrBodyTooLarge is returned.
func (resp *Response) ReadLimitBody(r *bufio.Reader, maxBodySize int) error {
	resp.clearSkipHeader()
	err := resp.Header.Read(r)
	if err != nil {
		return err
	}

	if !isSkipResponseBody(resp.Header.StatusCode()) && !resp.SkipBody {
		resp.body, err = readBody(r, resp.Header.ContentLength(), maxBodySize, resp.body)
		if err != nil {
			resp.Reset()
			return err
		}
		resp.Header.SetContentLength(len(resp.body))
	}
	return nil
}

func isSkipResponseBody(statusCode int) bool {
	// From http/1.1 specs:
	// All 1xx (informational), 204 (no content), and 304 (not modified) responses MUST NOT include a message-body
	if statusCode >= 100 && statusCode < 200 {
		return true
	}
	return statusCode == StatusNoContent || statusCode == StatusNotModified
}

var errRequestHostRequired = errors.New("Missing required Host header in request")

// Write writes request to w.
//
// Write doesn't flush request to w for performance reasons.
func (req *Request) Write(w *bufio.Writer) error {
	if len(req.Header.Host()) == 0 {
		uri := req.URI()
		host := uri.Host()
		if len(host) == 0 {
			return errRequestHostRequired
		}
		req.Header.SetHostBytes(host)
		req.Header.SetRequestURIBytes(uri.RequestURI())
	}
	req.Header.SetContentLength(len(req.body))
	err := req.Header.Write(w)
	if err != nil {
		return err
	}
	if !req.Header.noBody() {
		_, err = w.Write(req.body)
	} else if len(req.body) > 0 {
		return fmt.Errorf("Non-zero body for non-POST request. body=%q", req.body)
	}
	return err
}

// Write writes response to w.
//
// Write doesn't flush response to w for performance reasons.
func (resp *Response) Write(w *bufio.Writer) error {
	var err error
	if resp.bodyStream != nil {
		contentLength := resp.Header.ContentLength()
		if contentLength >= 0 {
			if err = resp.Header.Write(w); err != nil {
				return err
			}
			if err = writeBodyFixedSize(w, resp.bodyStream, contentLength); err != nil {
				return err
			}
		} else {
			resp.Header.SetContentLength(-1)
			if err = resp.Header.Write(w); err != nil {
				return err
			}
			if err = writeBodyChunked(w, resp.bodyStream); err != nil {
				return err
			}
		}
		if bsc, ok := resp.bodyStream.(io.Closer); ok {
			err = bsc.Close()
		}
		return err
	}

	resp.Header.SetContentLength(len(resp.body))
	if err = resp.Header.Write(w); err != nil {
		return err
	}
	_, err = w.Write(resp.body)
	return err
}

// String returns request representation.
//
// Returns error message instead of request representation on error.
//
// Use Write instead of String for performance-critical code.
func (req *Request) String() string {
	return getHTTPString(req)
}

// String returns response representation.
//
// Returns error message instead of response representation on error.
//
// Use Write instead of String for performance-critical code.
func (resp *Response) String() string {
	return getHTTPString(resp)
}

func getHTTPString(hw httpWriter) string {
	var w bytes.Buffer
	bw := bufio.NewWriter(&w)
	if err := hw.Write(bw); err != nil {
		return err.Error()
	}
	if err := bw.Flush(); err != nil {
		return err.Error()
	}
	return string(w.Bytes())
}

type httpWriter interface {
	Write(w *bufio.Writer) error
}

func writeBodyChunked(w *bufio.Writer, r io.Reader) error {
	vbuf := copyBufPool.Get()
	if vbuf == nil {
		vbuf = make([]byte, 4096)
	}
	buf := vbuf.([]byte)

	var err error
	var n int
	for {
		n, err = r.Read(buf)
		if n == 0 {
			if err == nil {
				panic("BUG: io.Reader returned 0, nil")
			}
			if err == io.EOF {
				if err = writeChunk(w, buf[:0]); err != nil {
					break
				}
				err = nil
			}
			break
		}
		if err = writeChunk(w, buf[:n]); err != nil {
			break
		}
	}

	copyBufPool.Put(vbuf)
	return err
}

var limitReaderPool sync.Pool

func writeBodyFixedSize(w *bufio.Writer, r io.Reader, size int) error {
	vbuf := copyBufPool.Get()
	if vbuf == nil {
		vbuf = make([]byte, 4096)
	}
	buf := vbuf.([]byte)

	vlr := limitReaderPool.Get()
	if vlr == nil {
		vlr = &io.LimitedReader{}
	}
	lr := vlr.(*io.LimitedReader)
	lr.R = r
	lr.N = int64(size)

	n, err := io.CopyBuffer(w, lr, buf)

	limitReaderPool.Put(vlr)
	copyBufPool.Put(vbuf)

	if n != int64(size) && err == nil {
		err = fmt.Errorf("read %d bytes from BodyStream instead of %d bytes", n, size)
	}
	return err
}

func writeChunk(w *bufio.Writer, b []byte) error {
	n := len(b)
	writeHexInt(w, n)
	w.Write(strCRLF)
	w.Write(b)
	_, err := w.Write(strCRLF)
	return err
}

var copyBufPool sync.Pool

// ErrBodyTooLarge is returned if either request or response body exceeds
// the given limit.
var ErrBodyTooLarge = errors.New("body size exceeds the given limit")

func readBody(r *bufio.Reader, contentLength int, maxBodySize int, dst []byte) ([]byte, error) {
	dst = dst[:0]
	if contentLength >= 0 {
		if maxBodySize > 0 && contentLength > maxBodySize {
			return dst, ErrBodyTooLarge
		}
		return appendBodyFixedSize(r, dst, contentLength)
	}
	if contentLength == -1 {
		return readBodyChunked(r, maxBodySize, dst)
	}
	return readBodyIdentity(r, maxBodySize, dst)
}

func readBodyIdentity(r *bufio.Reader, maxBodySize int, dst []byte) ([]byte, error) {
	dst = dst[:cap(dst)]
	if len(dst) == 0 {
		dst = make([]byte, 1024)
	}
	offset := 0
	for {
		nn, err := r.Read(dst[offset:])
		if nn <= 0 {
			if err != nil {
				if err == io.EOF {
					return dst[:offset], nil
				}
				return dst[:offset], err
			}
			panic(fmt.Sprintf("BUG: bufio.Read() returned (%d, nil)", nn))
		}
		offset += nn
		if maxBodySize > 0 && offset > maxBodySize {
			return dst[:offset], ErrBodyTooLarge
		}
		if len(dst) == offset {
			n := round2(2 * offset)
			if maxBodySize > 0 && n > maxBodySize {
				n = maxBodySize + 1
			}
			b := make([]byte, n)
			copy(b, dst)
			dst = b
		}
	}
}

func appendBodyFixedSize(r *bufio.Reader, dst []byte, n int) ([]byte, error) {
	if n == 0 {
		return dst, nil
	}

	offset := len(dst)
	dstLen := offset + n
	if cap(dst) < dstLen {
		b := make([]byte, round2(dstLen))
		copy(b, dst)
		dst = b
	}
	dst = dst[:dstLen]

	for {
		nn, err := r.Read(dst[offset:])
		if nn <= 0 {
			if err != nil {
				if err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				return dst[:offset], err
			}
			panic(fmt.Sprintf("BUG: bufio.Read() returned (%d, nil)", nn))
		}
		offset += nn
		if offset == dstLen {
			return dst, nil
		}
	}
}

func readBodyChunked(r *bufio.Reader, maxBodySize int, dst []byte) ([]byte, error) {
	if len(dst) > 0 {
		panic("BUG: expected zero-length buffer")
	}

	strCRLFLen := len(strCRLF)
	for {
		chunkSize, err := parseChunkSize(r)
		if err != nil {
			return dst, err
		}
		if maxBodySize > 0 && len(dst)+chunkSize > maxBodySize {
			return dst, ErrBodyTooLarge
		}
		dst, err = appendBodyFixedSize(r, dst, chunkSize+strCRLFLen)
		if err != nil {
			return dst, err
		}
		if !bytes.Equal(dst[len(dst)-strCRLFLen:], strCRLF) {
			return dst, fmt.Errorf("cannot find crlf at the end of chunk")
		}
		dst = dst[:len(dst)-strCRLFLen]
		if chunkSize == 0 {
			return dst, nil
		}
	}
}

func parseChunkSize(r *bufio.Reader) (int, error) {
	n, err := readHexInt(r)
	if err != nil {
		return -1, err
	}
	c, err := r.ReadByte()
	if err != nil {
		return -1, fmt.Errorf("cannot read '\r' char at the end of chunk size: %s", err)
	}
	if c != '\r' {
		return -1, fmt.Errorf("unexpected char %q at the end of chunk size. Expected %q", c, '\r')
	}
	c, err = r.ReadByte()
	if err != nil {
		return -1, fmt.Errorf("cannot read '\n' char at the end of chunk size: %s", err)
	}
	if c != '\n' {
		return -1, fmt.Errorf("unexpected char %q at the end of chunk size. Expected %q", c, '\n')
	}
	return n, nil
}

func round2(n int) int {
	if n <= 0 {
		return 0
	}
	n--
	x := uint(0)
	for n > 0 {
		n >>= 1
		x++
	}
	return 1 << x
}
