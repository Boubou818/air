package air

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// server is an HTTP server.
type server struct {
	a            *Air
	server       *http.Server
	requestPool  *sync.Pool
	responsePool *sync.Pool
}

// newServer returns a new instance of the `server` with the a.
func newServer(a *Air) *server {
	return &server{
		a:      a,
		server: &http.Server{},
		requestPool: &sync.Pool{
			New: func() interface{} {
				return &Request{}
			},
		},
		responsePool: &sync.Pool{
			New: func() interface{} {
				return &Response{}
			},
		},
	}
}

// serve starts the s.
func (s *server) serve() error {
	host, port, err := net.SplitHostPort(s.a.Address)
	if err != nil {
		return err
	}

	host = strings.ToLower(host)
	port = strings.ToLower(port)

	s.server.Addr = host + ":" + port
	s.server.Handler = s
	s.server.ReadTimeout = s.a.ReadTimeout
	s.server.ReadHeaderTimeout = s.a.ReadHeaderTimeout
	s.server.WriteTimeout = s.a.WriteTimeout
	s.server.IdleTimeout = s.a.IdleTimeout
	s.server.MaxHeaderBytes = s.a.MaxHeaderBytes
	s.server.ErrorLog = s.a.errorLogger

	idleTimeout := s.a.IdleTimeout
	if idleTimeout == 0 {
		idleTimeout = s.a.ReadTimeout
	}

	h2ch := h2c.NewHandler(s, &http2.Server{
		IdleTimeout: idleTimeout,
	})

	var hh http.Handler
	if s.a.HTTPSEnforced && port != "80" && port != "https" {
		hh = http.HandlerFunc(func(
			rw http.ResponseWriter,
			r *http.Request,
		) {
			host := r.Host
			if h, _, _ := net.SplitHostPort(host); h != "" {
				host = h
			}

			if net.ParseIP(host) == nil {
				if port != "443" && port != "https" {
					host += ":" + port
				}

				http.Redirect(
					rw,
					r,
					"https://"+host+r.RequestURI,
					http.StatusMovedPermanently,
				)
			} else {
				h2ch.ServeHTTP(rw, r)
			}
		})
	}

	if s.a.DebugMode {
		fmt.Println("air: serving in debug mode")
	}

	if s.a.TLSCertFile != "" && s.a.TLSKeyFile != "" {
		c, err := tls.LoadX509KeyPair(s.a.TLSCertFile, s.a.TLSKeyFile)
		if err != nil {
			return err
		}

		s.server.TLSConfig = &tls.Config{
			GetCertificate: func(
				chi *tls.ClientHelloInfo,
			) (*tls.Certificate, error) {
				if !s.allowedHost(chi.ServerName) {
					return nil, chi.Conn.Close()
				}

				return &c, nil
			},
		}
	} else if !s.a.DebugMode && s.a.ACMEEnabled {
		acm := &autocert.Manager{
			Prompt: autocert.AcceptTOS,
			Cache:  autocert.DirCache(s.a.ACMECertRoot),
			Client: &acme.Client{
				DirectoryURL: s.a.ACMEDirectoryURL,
			},
			Email: s.a.MaintainerEmail,
		}

		if hh != nil {
			hh = acm.HTTPHandler(hh)
		} else {
			hh = acm.HTTPHandler(h2ch)
		}

		s.server.Addr = host + ":443"
		s.server.TLSConfig = acm.TLSConfig()
		s.server.TLSConfig.GetCertificate = func(
			chi *tls.ClientHelloInfo,
		) (*tls.Certificate, error) {
			if !s.allowedHost(chi.ServerName) {
				return nil, chi.Conn.Close()
			} else if net.ParseIP(chi.ServerName) != nil {
				return nil, fmt.Errorf(
					"air: unchallengeable host: %s",
					chi.ServerName,
				)
			}

			chi.ServerName = strings.ToLower(chi.ServerName)

			return acm.GetCertificate(chi)
		}
	} else {
		s.server.Handler = http.HandlerFunc(func(
			rw http.ResponseWriter,
			r *http.Request,
		) {
			if s.allowedHost(r.Host) {
				h2ch.ServeHTTP(rw, r)
			} else if h, ok := rw.(http.Hijacker); ok {
				if c, _, _ := h.Hijack(); c != nil {
					c.Close()
				}
			}
		})

		return s.server.ListenAndServe()
	}

	if hh != nil {
		hs := &http.Server{
			Addr: host + ":80",
			Handler: http.HandlerFunc(func(
				rw http.ResponseWriter,
				r *http.Request,
			) {
				if s.allowedHost(r.Host) {
					hh.ServeHTTP(rw, r)
				} else if h, ok := rw.(http.Hijacker); ok {
					if c, _, _ := h.Hijack(); c != nil {
						c.Close()
					}
				}
			}),
			ReadTimeout:       s.a.ReadTimeout,
			ReadHeaderTimeout: s.a.ReadHeaderTimeout,
			WriteTimeout:      s.a.WriteTimeout,
			IdleTimeout:       s.a.IdleTimeout,
			MaxHeaderBytes:    s.a.MaxHeaderBytes,
			ErrorLog:          s.a.errorLogger,
		}

		go hs.ListenAndServe()
		defer hs.Close()
	}

	return s.server.ListenAndServeTLS("", "")
}

// close closes the s immediately.
func (s *server) close() error {
	return s.server.Close()
}

// shutdown gracefully shuts down the s without interrupting any active
// connections until timeout. It waits indefinitely for connections to return to
// idle and then shut down when the timeout is less than or equal to zero.
func (s *server) shutdown(timeout time.Duration) error {
	if timeout <= 0 {
		return s.server.Shutdown(context.Background())
	}

	c, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return s.server.Shutdown(c)
}

// allowedHost reports whether the host is allowed.
func (s *server) allowedHost(host string) bool {
	if s.a.DebugMode || len(s.a.HostWhitelist) == 0 {
		return true
	}

	if h, _, _ := net.SplitHostPort(host); h != "" {
		host = h
	}

	return stringSliceContainsCIly(s.a.HostWhitelist, host)
}

// ServeHTTP implements the `http.Handler`.
func (s *server) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	// Get request and response from the pool.

	req := s.requestPool.Get().(*Request)
	res := s.responsePool.Get().(*Response)

	// Tie the request body and the standard request body together.

	r.Body = &requestBody{
		r:  req,
		hr: r,
		rc: r.Body,
	}

	// Reset the request.

	req.Air = s.a
	req.SetHTTPRequest(r)
	req.res = res
	req.params = req.params[:0]
	req.routeParamNames = nil
	req.routeParamValues = nil
	req.parseRouteParamsOnce = &sync.Once{}
	req.parseOtherParamsOnce = &sync.Once{}
	req.localizedString = nil

	// Reset the response.

	res.Air = s.a
	res.SetHTTPResponseWriter(&responseWriter{
		r: res,
		w: rw,
	})
	res.Status = http.StatusOK
	res.ContentLength = -1
	res.Written = false
	res.Minified = false
	res.Gzipped = false
	res.req = req
	res.ohrw = rw
	res.servingContent = false
	res.serveContentError = nil
	res.reverseProxying = false
	res.reverseProxyError = nil
	res.deferredFuncs = res.deferredFuncs[:0]

	// Chain the gases stack.

	h := func(req *Request, res *Response) error {
		rh := s.a.router.route(req)
		h := func(req *Request, res *Response) error {
			err := rh(req, res)
			if res.Written {
				return err
			} else if err == nil {
				res.Status = http.StatusNoContent
				r.Header.Del("Content-Type")
				r.Header.Del("Content-Length")
				return res.Write(nil)
			} else if res.Status < http.StatusBadRequest {
				res.Status = http.StatusInternalServerError
			}

			return err
		}

		for i := len(s.a.Gases) - 1; i >= 0; i-- {
			h = s.a.Gases[i](h)
		}

		return h(req, res)
	}

	// Chain the pregases stack.

	for i := len(s.a.Pregases) - 1; i >= 0; i-- {
		h = s.a.Pregases[i](h)
	}

	// Execute the chain.

	if err := h(req, res); err != nil {
		s.a.ErrorHandler(err, req, res)
	}

	// Execute the deferred functions.

	for i := len(res.deferredFuncs) - 1; i >= 0; i-- {
		res.deferredFuncs[i]()
	}

	// Put the route param values back to the pool.

	if req.routeParamValues != nil {
		s.a.router.routeParamValuesPool.Put(req.routeParamValues)
	}

	// Put the request and response back to the pool.

	s.requestPool.Put(req)
	s.responsePool.Put(res)
}
