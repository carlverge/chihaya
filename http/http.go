// Copyright 2015 The Chihaya Authors. All rights reserved.
// Use of this source code is governed by the BSD 2-Clause license,
// which can be found in the LICENSE file.

// Package http implements a BitTorrent tracker over the HTTP protocol as per
// BEP 3.
package http

import (
	"crypto/tls"
	"crypto/x509"

	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/julienschmidt/httprouter"
	"github.com/soheilhy/cmux"
	"github.com/tylerb/graceful"

	"github.com/chihaya/chihaya/config"
	"github.com/chihaya/chihaya/stats"
	"github.com/chihaya/chihaya/tracker"
)

type keypairReloader struct {
	certMu     sync.RWMutex
	cert       *tls.Certificate
	certPath   string
	keyPath    string
	timer      *time.Timer
	reloadTime time.Duration
}

func NewKeypairReloader(certPath, keyPath string) (*keypairReloader, error) {
	result := &keypairReloader{
		certPath: certPath,
		keyPath:  keyPath,
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}

	result.cert = &cert
	go func() {
		var wg sync.WaitGroup

		for {
			pCert, err := x509.ParseCertificate(result.cert.Certificate[0])
			if err != nil {
				glog.Errorf("Failed to parse x509 Certificate %v", err)
				result.reloadTime = time.Second * 6 * 3600 // XXX: Configuration for default cert refresh?
			} else {
				result.reloadTime = time.Until(pCert.NotAfter) - (time.Second * 3600)
				if result.reloadTime < 0 {
					result.reloadTime = time.Until(pCert.NotAfter)
				}
			}
			glog.Info("Time until certificate reload: ", result.reloadTime)
			if result.timer == nil {
				result.timer = time.AfterFunc(result.reloadTime, func() {
					if err := result.maybeReload(); err != nil {
						glog.Errorf("Keeping old TLS certificate because the new one could not be loaded: %v", err)
					} else {
						glog.Info("TLS certificate successfully reloaded")
					}
					wg.Done()
				})
				wg.Add(1)
			} else {
				result.timer.Reset(result.reloadTime)
				wg.Add(1)
			}
			wg.Wait()
		}
	}()
	return result, nil
}

func (kpr *keypairReloader) maybeReload() error {
	newCert, err := tls.LoadX509KeyPair(kpr.certPath, kpr.keyPath)
	if err != nil {
		return err
	}
	kpr.certMu.Lock()
	defer kpr.certMu.Unlock()
	kpr.cert = &newCert
	return nil
}

func (kpr *keypairReloader) GetCertificateFunc() func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		kpr.certMu.RLock()
		defer kpr.certMu.RUnlock()
		return kpr.cert, nil
	}
}

// ResponseHandler is an HTTP handler that returns a status code.
type ResponseHandler func(http.ResponseWriter, *http.Request, httprouter.Params) (int, error)

// Server represents an HTTP serving torrent tracker.
type Server struct {
	config   *config.Config
	tracker  *tracker.Tracker
	http     *graceful.Server
	https    *graceful.Server
	stopping bool
}

// makeHandler wraps our ResponseHandlers while timing requests, collecting,
// stats, logging, and handling errors.
func makeHandler(handler ResponseHandler, ssl bool) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
		start := time.Now()
		httpCode, err := handler(w, r, p)
		duration := time.Since(start)

		var msg string
		if err != nil {
			msg = err.Error()
		} else if httpCode != http.StatusOK {
			msg = http.StatusText(httpCode)
		}

		if len(msg) > 0 {
			http.Error(w, msg, httpCode)
			stats.RecordEvent(stats.ErroredRequest)
		}

		if len(msg) > 0 || glog.V(2) {
			reqString := r.URL.Path + " " + r.RemoteAddr
			if glog.V(3) {
				reqString = r.URL.RequestURI() + " " + r.RemoteAddr
			}

			httpString := "HTTP"
			if ssl {
				httpString = "HTTPS"
			}

			if len(msg) > 0 {
				glog.Errorf("[%-5s - %9s] %s (%d - %s)", httpString, duration, reqString, httpCode, msg)
			} else {
				glog.Infof("[%-5s - %9s] %s (%d)", httpString, duration, reqString, httpCode)
			}
		}

		stats.RecordEvent(stats.HandledRequest)
		stats.RecordTiming(stats.ResponseTime, duration)
	}
}

// newRouter returns a router with all the routes.
func newRouter(s *Server, ssl bool) *httprouter.Router {
	r := httprouter.New()

	r.GET("/announce", makeHandler(s.serveAnnounce, ssl))
	r.GET("/scrape", makeHandler(s.serveScrape, ssl))

	return r
}

// connState is used by graceful in order to gracefully shutdown. It also
// keeps track of connection stats.
func (s *Server) connStateSSL(conn net.Conn, state http.ConnState) {
	s.connStateImpl(conn, state, true)
}

func (s *Server) connState(conn net.Conn, state http.ConnState) {
	s.connStateImpl(conn, state, false)
}

func (s *Server) connStateImpl(conn net.Conn, state http.ConnState, ssl bool) {
	switch state {
	case http.StateNew:
		stats.RecordEvent(stats.AcceptedConnection)
		if ssl {
			stats.RecordEvent(stats.AcceptedSSLConnection)
		}

	case http.StateClosed:
		stats.RecordEvent(stats.ClosedConnection)
		if ssl {
			stats.RecordEvent(stats.ClosedSSLConnection)
		}

	case http.StateHijacked:
		panic("connection impossibly hijacked")

	// Ignore the following cases.
	case http.StateActive, http.StateIdle:

	default:
		glog.Errorf("Connection transitioned to unknown state %s (%d)", state, state)
	}
}

// Serve runs an HTTP server, blocking until the server has shut down.
func (s *Server) Serve() {
	glog.V(0).Info("Starting HTTP on ", s.config.HTTPConfig.ListenAddr)

	if s.config.HTTPConfig.ListenLimit != 0 {
		glog.V(0).Info("Limiting connections to ", s.config.HTTPConfig.ListenLimit)
	}

	l, err := net.Listen("tcp", s.config.HTTPConfig.ListenAddr)
	if err != nil {
		panic(err)
	}

	// Create a cmux.
	mux := cmux.New(l)
	httpListener := mux.Match(cmux.HTTP1Fast())

	if s.config.HTTPConfig.TLSCertPath != "" && s.config.HTTPConfig.TLSKeyPath != "" {
		glog.V(0).Info("Starting HTTPS on ", s.config.HTTPConfig.ListenAddr)

		kpr, err := NewKeypairReloader(s.config.HTTPConfig.TLSCertPath, s.config.HTTPConfig.TLSKeyPath)
		if err != nil {
			panic(err)
		}

		tlsCfg := &tls.Config{
			GetCertificate: kpr.GetCertificateFunc(),
		}

		s.https = newGraceful(s, true)
		s.https.SetKeepAlivesEnabled(false)
		s.https.ShutdownInitiated = func() { s.stopping = true; kpr.timer.Stop() }

		// Create TLS listener.
		httpsListener := tls.NewListener(mux.Match(cmux.Any()), tlsCfg)

		go func() {
			if err := s.https.Serve(httpsListener); err != nil && err != cmux.ErrListenerClosed {
				panic(err)
			}
			glog.Info("HTTPS server shut down cleanly")
		}()
	}

	s.http = newGraceful(s, false)
	s.http.SetKeepAlivesEnabled(false)
	s.http.ShutdownInitiated = func() { s.stopping = true }

	go func() {
		if err := s.http.Serve(httpListener); err != nil && err != cmux.ErrListenerClosed {
			panic(err)
		}
		glog.Info("HTTP server shut down cleanly")
	}()

	if err := mux.Serve(); !strings.Contains(err.Error(), "use of closed network connection") {
		panic(err)
	}
}

// Stop cleanly shuts down the server.
func (s *Server) Stop() {
	if !s.stopping {
		s.http.Stop(s.http.Timeout)
		if s.https != nil {
			s.https.Stop(s.https.Timeout)
		}
	}
}

func newGraceful(s *Server, ssl bool) *graceful.Server {
	connState := s.connState
	if ssl {
		connState = s.connStateSSL
	}
	return &graceful.Server{
		Timeout:     s.config.HTTPConfig.RequestTimeout.Duration,
		ConnState:   connState,
		ListenLimit: s.config.HTTPConfig.ListenLimit,

		NoSignalHandling: true,
		Server: &http.Server{
			Addr:         s.config.HTTPConfig.ListenAddr,
			Handler:      newRouter(s, ssl),
			ReadTimeout:  s.config.HTTPConfig.ReadTimeout.Duration,
			WriteTimeout: s.config.HTTPConfig.WriteTimeout.Duration,
		},
	}
}

// NewServer returns a new HTTP server for a given configuration and tracker.
func NewServer(cfg *config.Config, tkr *tracker.Tracker) *Server {
	return &Server{
		config:  cfg,
		tracker: tkr,
	}
}
