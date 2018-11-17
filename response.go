package air

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/golang/protobuf/proto"
	"github.com/gorilla/websocket"
	"github.com/vmihailenco/msgpack"
	"golang.org/x/net/html"
)

// Response is an HTTP response.
type Response struct {
	Air           *Air
	Status        int
	Header        http.Header
	Body          io.Writer
	ContentLength int64
	Written       bool

	req  *Request
	hrw  http.ResponseWriter
	ohrw http.ResponseWriter
}

// HTTPResponseWriter returns the underlying `http.ResponseWriter` of the r.
//
// ATTENTION: You should never call this method unless you know what you are
// doing. And, be sure to call the `Response#SetHTTPResponseWriter()` when you
// have modified it.
func (r *Response) HTTPResponseWriter() http.ResponseWriter {
	return r.hrw
}

func (r *Response) SetHTTPResponseWriter(hrw http.ResponseWriter) {
	r.Header = hrw.Header()
	r.hrw = hrw
}

// SetCookie sets the c to the `r#Header`.
func (r *Response) SetCookie(c *http.Cookie) {
	if v := c.String(); v != "" {
		r.Header.Add("Set-Cookie", v)
	}
}

// Write responds to the client with the content.
func (r *Response) Write(content io.ReadSeeker) error {
	if r.Written {
		var err error
		if r.req.Method != http.MethodHead {
			_, err = io.Copy(r.hrw, content)
		}

		return err
	}

	lm, _ := http.ParseTime(r.Header.Get("Last-Modified"))
	http.ServeContent(r.hrw, r.req.HTTPRequest(), "", lm, content)

	return nil
}

// WriteBlob responds to the client with the content b.
func (r *Response) WriteBlob(b []byte) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		var err error
		if b, err = r.Air.minifier.minify(ct, b); err != nil {
			return err
		}
	}

	return r.Write(bytes.NewReader(b))
}

// WriteString responds to the client with the "text/plain" content s.
func (r *Response) WriteString(s string) error {
	r.Header.Set("Content-Type", "text/plain; charset=utf-8")
	return r.WriteBlob([]byte(s))
}

// WriteJSON responds to the client with the "application/json" content v.
func (r *Response) WriteJSON(v interface{}) error {
	var (
		b   []byte
		err error
	)

	if r.Air.DebugMode {
		b, err = json.MarshalIndent(v, "", "\t")
	} else {
		b, err = json.Marshal(v)
	}

	if err != nil {
		return err
	}

	r.Header.Set("Content-Type", "application/json; charset=utf-8")

	return r.WriteBlob(b)
}

// WriteXML responds to the client with the "application/xml" content v.
func (r *Response) WriteXML(v interface{}) error {
	var (
		b   []byte
		err error
	)

	if r.Air.DebugMode {
		b, err = xml.MarshalIndent(v, "", "\t")
	} else {
		b, err = xml.Marshal(v)
	}

	if err != nil {
		return err
	}

	r.Header.Set("Content-Type", "application/xml; charset=utf-8")

	return r.WriteBlob(append([]byte(xml.Header), b...))
}

// WriteMsgpack responds to the client with the "application/msgpack" content v.
func (r *Response) WriteMsgpack(v interface{}) error {
	b, err := msgpack.Marshal(v)
	if err != nil {
		return err
	}

	r.Header.Set("Content-Type", "application/msgpack")

	return r.WriteBlob(b)
}

// WriteProtobuf responds to the client with the "application/protobuf" content
// v.
func (r *Response) WriteProtobuf(v interface{}) error {
	b, err := proto.Marshal(v.(proto.Message))
	if err != nil {
		return err
	}

	r.Header.Set("Content-Type", "application/protobuf")

	return r.WriteBlob(b)
}

// WriteTOML responds to the client with the "application/toml" content v.
func (r *Response) WriteTOML(v interface{}) error {
	buf := &bytes.Buffer{}
	if err := toml.NewEncoder(buf).Encode(v); err != nil {
		return err
	}

	r.Header.Set("Content-Type", "application/toml; charset=utf-8")

	return r.WriteBlob(buf.Bytes())
}

// WriteHTML responds to the client with the "text/html" content h.
func (r *Response) WriteHTML(h string) error {
	if r.Air.AutoPushEnabled && r.req.HTTPRequest().ProtoMajor == 2 {
		tree, err := html.Parse(strings.NewReader(h))
		if err != nil {
			return err
		}

		var f func(*html.Node)
		f = func(n *html.Node) {
			if n.Type == html.ElementNode {
				target := ""
				switch n.Data {
				case "link":
					for _, a := range n.Attr {
						if a.Key == "href" {
							target = a.Val
							break
						}
					}
				case "img", "script":
					for _, a := range n.Attr {
						if a.Key == "src" {
							target = a.Val
							break
						}
					}
				}

				if path.IsAbs(target) {
					r.Push(target, nil)
				}
			}

			for c := n.FirstChild; c != nil; c = c.NextSibling {
				f(c)
			}
		}

		f(tree)
	}

	r.Header.Set("Content-Type", "text/html; charset=utf-8")

	return r.WriteBlob([]byte(h))
}

// Render renders one or more HTML templates with the m and responds to the
// client with the "text/html" content. The results rendered by the former can
// be inherited by accessing the `m["InheritedHTML"]`.
func (r *Response) Render(m map[string]interface{}, templates ...string) error {
	buf := bytes.Buffer{}
	for _, t := range templates {
		m["InheritedHTML"] = template.HTML(buf.String())
		buf.Reset()
		err := r.Air.renderer.render(&buf, t, m, r.req.LocalizedString)
		if err != nil {
			return err
		}
	}

	return r.WriteHTML(buf.String())
}

// WriteFile responds to the client with a file content with the filename.
func (r *Response) WriteFile(filename string) error {
	filename, err := filepath.Abs(filename)
	if err != nil {
		return err
	} else if fi, err := os.Stat(filename); err != nil {
		return err
	} else if fi.IsDir() {
		hru := r.req.HTTPRequest().URL
		if p := hru.EscapedPath(); !hasLastSlash(p) {
			p = path.Base(p) + "/"
			if q := hru.RawQuery; q != "" {
				p += "?" + q
			}

			r.Status = http.StatusMovedPermanently

			return r.Redirect(p)
		}

		filename += "index.html"
	}

	var (
		c  io.ReadSeeker
		ct string
		et []byte
		mt time.Time
	)

	if a, err := r.Air.coffer.asset(filename); err != nil {
		return err
	} else if a != nil {
		c = bytes.NewReader(a.content)
		ct = a.mimeType
		et = a.checksum[:]
		mt = a.modTime
	} else {
		f, err := os.Open(filename)
		if err != nil {
			return err
		}
		defer f.Close()

		fi, err := f.Stat()
		if err != nil {
			return err
		}

		c = f
		mt = fi.ModTime()
	}

	if r.Header.Get("Content-Type") == "" {
		if ct == "" {
			ct = mime.TypeByExtension(filepath.Ext(filename))
		}

		if ct != "" { // Don't worry, someone will check it later
			r.Header.Set("Content-Type", ct)
		}
	}

	if r.Header.Get("ETag") == "" {
		if et == nil {
			h := sha256.New()
			if _, err := io.Copy(h, c); err != nil {
				return err
			}

			et = h.Sum(nil)
		}

		r.Header.Set("ETag", fmt.Sprintf(`"%x"`, et))
	}

	if r.Header.Get("Last-Modified") == "" {
		r.Header.Set("Last-Modified", mt.UTC().Format(http.TimeFormat))
	}

	return r.Write(c)
}

// Redirect responds to the client with a redirection to the url.
func (r *Response) Redirect(url string) error {
	if r.Status < http.StatusMultipleChoices ||
		r.Status >= http.StatusBadRequest {
		r.Status = http.StatusFound
	}

	http.Redirect(r.hrw, r.req.HTTPRequest(), url, r.Status)

	return nil
}

// WebSocket switches the connection to the WebSocket protocol.
func (r *Response) WebSocket() (*WebSocket, error) {
	r.Status = http.StatusSwitchingProtocols
	r.Written = true

	wsu := &websocket.Upgrader{
		HandshakeTimeout: r.Air.WebSocketHandshakeTimeout,
		Error: func(
			_ http.ResponseWriter,
			_ *http.Request,
			status int,
			_ error,
		) {
			r.Status = status
			r.Written = false
		},
		CheckOrigin: func(*http.Request) bool {
			return true
		},
	}
	if len(r.Air.WebSocketSubprotocols) > 0 {
		wsu.Subprotocols = r.Air.WebSocketSubprotocols
	}

	conn, err := wsu.Upgrade(r.ohrw, r.req.HTTPRequest(), r.Header)
	if err != nil {
		return nil, err
	}

	ws := &WebSocket{
		conn: conn,
	}

	conn.SetCloseHandler(func(status int, reason string) error {
		ws.closed = true

		if ws.ConnectionCloseHandler != nil {
			return ws.ConnectionCloseHandler(status, reason)
		}

		conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(status, ""),
			time.Now().Add(time.Second),
		)

		return nil
	})

	conn.SetPingHandler(func(appData string) error {
		if ws.PingHandler != nil {
			return ws.PingHandler(appData)
		}

		err := conn.WriteControl(
			websocket.PongMessage,
			[]byte(appData),
			time.Now().Add(time.Second),
		)
		if err == websocket.ErrCloseSent {
			return nil
		} else if e, ok := err.(net.Error); ok && e.Temporary() {
			return nil
		}

		return err
	})

	conn.SetPongHandler(func(appData string) error {
		if ws.PongHandler != nil {
			return ws.PongHandler(appData)
		}

		return nil
	})

	return ws, nil
}

// Push initiates an HTTP/2 server push. This constructs a synthetic request
// using the target and the pos, serializes that request into a PUSH_PROMISE
// frame, then dispatches that request using the server's request handler. If
// pos is nil, default options are used.
//
// The target must either be an absolute path (like "/path") or an absolute URL
// that contains a valid authority and the same scheme as the parent request. If
// the target is a path, it will inherit the scheme and authority of the parent
// request.
//
// It returns `http.ErrNotSupported` if the client has disabled push or if push
// is not supported on the underlying connection.
func (r *Response) Push(target string, pos *http.PushOptions) error {
	p, ok := r.ohrw.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}

	return p.Push(target, pos)
}

// responseBody provides a convenient way to continuously write content to the
// client.
type responseBody struct {
	r *Response
}

// Write implements the `io.Writer`.
func (rb *responseBody) Write(b []byte) (int, error) {
	if !rb.r.Written {
		if err := rb.r.Write(nil); err != nil {
			return 0, err
		}
	}

	return rb.r.hrw.Write(b)
}

// responseWriter used to tie the `Response` and the `http.ResponseWriter`
// together.
type responseWriter struct {
	r *Response
	w http.ResponseWriter
}

// Header implements the `http.ResponseWriter`.
func (rw *responseWriter) Header() http.Header {
	return rw.w.Header()
}

// Write implements the `http.ResponseWriter`.
func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.r.Written {
		rw.WriteHeader(rw.r.Status)
	}

	n, err := rw.w.Write(b)
	if err != nil {
		return 0, err
	}

	rw.r.ContentLength += int64(n)

	return n, nil
}

// WriteHeader implements the `http.ResponseWriter`.
func (rw *responseWriter) WriteHeader(status int) {
	if rw.r.Written {
		return
	}

	if status == http.StatusOK && status != rw.r.Status {
		status = rw.r.Status
	}

	h := rw.w.Header()
	if !rw.r.Air.DebugMode &&
		rw.r.Air.HTTPSEnforced &&
		rw.r.Air.server.server.TLSConfig != nil &&
		h.Get("Strict-Transport-Security") == "" {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}

	h.Set("Server", "Air")

	rw.w.WriteHeader(status)

	rw.r.Status = status
	rw.r.Written = true
}

// Push implements the `http.Pusher`.
func (rw *responseWriter) Push(target string, pos *http.PushOptions) error {
	p, ok := rw.w.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}

	return p.Push(target, pos)
}
