// Copyright 2015 The Chihaya Authors. All rights reserved.
// Use of this source code is governed by the BSD 2-Clause license,
// which can be found in the LICENSE file.

// Package http implements a BitTorrent tracker over the HTTP protocol as per
// BEP 3.
package http

import (
	"crypto/tls"

	"net"
	"net/http"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/julienschmidt/httprouter"
	"github.com/soheilhy/cmux"
	"github.com/tylerb/graceful"

	"github.com/chihaya/chihaya/config"
	"github.com/chihaya/chihaya/stats"
	"github.com/chihaya/chihaya/tracker"
)

// ResponseHandler is an HTTP handler that returns a status code.
type ResponseHandler func(http.ResponseWriter, *http.Request, httprouter.Params) (int, error)

// Server represents an HTTP serving torrent tracker.
type Server struct {
	config   *config.Config
	tracker  *tracker.Tracker
	grace    *graceful.Server
	stopping bool
}

// makeHandler wraps our ResponseHandlers while timing requests, collecting,
// stats, logging, and handling errors.
func makeHandler(handler ResponseHandler) httprouter.Handle {
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

			if len(msg) > 0 {
				glog.Errorf("[HTTP - %9s] %s (%d - %s)", duration, reqString, httpCode, msg)
			} else {
				glog.Infof("[HTTP - %9s] %s (%d)", duration, reqString, httpCode)
			}
		}

		stats.RecordEvent(stats.HandledRequest)
		stats.RecordTiming(stats.ResponseTime, duration)
	}
}

// newRouter returns a router with all the routes.
func newRouter(s *Server) *httprouter.Router {
	r := httprouter.New()

	r.GET("/announce", makeHandler(s.serveAnnounce))
	r.GET("/scrape", makeHandler(s.serveScrape))

	return r
}

// connState is used by graceful in order to gracefully shutdown. It also
// keeps track of connection stats.
func (s *Server) connState(conn net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		stats.RecordEvent(stats.AcceptedConnection)

	case http.StateClosed:
		stats.RecordEvent(stats.ClosedConnection)

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

	grace := &graceful.Server{
		Timeout:     s.config.HTTPConfig.RequestTimeout.Duration,
		ConnState:   s.connState,
		ListenLimit: s.config.HTTPConfig.ListenLimit,

		NoSignalHandling: true,
		Server: &http.Server{
			Addr:         s.config.HTTPConfig.ListenAddr,
			Handler:      newRouter(s),
			ReadTimeout:  s.config.HTTPConfig.ReadTimeout.Duration,
			WriteTimeout: s.config.HTTPConfig.WriteTimeout.Duration,
		},
	}

	l, err := net.Listen("tcp", s.config.HTTPConfig.ListenAddr)
	if err != nil {
		panic(err)
	}

	// Create a cmux.
	m := cmux.New(l)
	httpl := m.Match(cmux.HTTP1Fast())
	go grace.Serve(httpl)
	if s.config.HTTPConfig.TLSCertPath != "" && s.config.HTTPConfig.TLSKeyPath != "" {
		tlsl := m.Match(cmux.Any())

		certificate, err := tls.LoadX509KeyPair(s.config.HTTPConfig.TLSCertPath, s.config.HTTPConfig.TLSKeyPath)
		if err != nil {
			panic(err)
		}
		config := &tls.Config{
			Certificates: []tls.Certificate{certificate},
		}

		// Create TLS listener.
		tlslL := tls.NewListener(tlsl, config)

		// Serve HTTP over TLS.
		go grace.Serve(tlslL)
	}

	s.grace = grace
	grace.SetKeepAlivesEnabled(false)
	grace.ShutdownInitiated = func() { s.stopping = true }

	if err := m.Serve(); !strings.Contains(err.Error(), "use of closed network connection") {
		panic(err)
	}
	/*
		if err := grace.ListenAndServe(); err != nil {
			if opErr, ok := err.(*net.OpError); !ok || (ok && opErr.Op != "accept") {
				glog.Errorf("Failed to gracefully run HTTP server: %s", err.Error())
				return
			}
		}
	*/

	glog.Info("HTTP server shut down cleanly")
}

// Stop cleanly shuts down the server.
func (s *Server) Stop() {
	if !s.stopping {
		s.grace.Stop(s.grace.Timeout)
	}
}

// NewServer returns a new HTTP server for a given configuration and tracker.
func NewServer(cfg *config.Config, tkr *tracker.Tracker) *Server {
	return &Server{
		config:  cfg,
		tracker: tkr,
	}
}
