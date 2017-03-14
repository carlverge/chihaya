// Copyright 2015 The Chihaya Authors. All rights reserved.
// Use of this source code is governed by the BSD 2-Clause license,
// which can be found in the LICENSE file.

// Package udp implements a BitTorrent tracker over the UDP protocol as per
// BEP 15.
package udp

import (
	"errors"
	"net"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/pushrax/bufferpool"

	"github.com/psaab/chihaya/config"
	"github.com/psaab/chihaya/tracker"
)

// Server represents a UDP torrent tracker.
type Server struct {
	config    *config.Config
	tracker   *tracker.Tracker
	sock      *net.UDPConn
	connIDGen *ConnectionIDGenerator

	closing chan struct{}
	booting chan struct{}
	wg      sync.WaitGroup
}

func (s *Server) serve() error {
	if s.sock != nil {
		return errors.New("server already booted")
	}

	udpAddr, err := net.ResolveUDPAddr("udp", s.config.UDPConfig.ListenAddr)
	if err != nil {
		close(s.booting)
		return err
	}

	sock, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		close(s.booting)
		return err
	}
	defer sock.Close()

	if s.config.UDPConfig.ReadBufferSize > 0 {
		sock.SetReadBuffer(s.config.UDPConfig.ReadBufferSize)
	}

	pool := bufferpool.New(1000, 2048)
	s.sock = sock
	close(s.booting)

	for {
		select {
		case <-s.closing:
			return nil
		default:
		}
		buffer := pool.TakeSlice()
		sock.SetReadDeadline(time.Now().Add(time.Second))
		n, addr, err := sock.ReadFromUDP(buffer)

		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Temporary() {
				pool.GiveSlice(buffer)
				continue
			}
			return err
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			start := time.Now()
			response, action, err := s.handlePacket(buffer[:n], addr)
			defer pool.GiveSlice(buffer)
			duration := time.Since(start)

			if len(response) > 0 {
				sock.WriteToUDP(response, addr)
			}

			if glog.V(2) {
				if err != nil {
					glog.Infof("[UDP - %9s] %s %s (%s)", duration, action, addr, err)
				} else {
					glog.Infof("[UDP - %9s] %s %s", duration, action, addr)
				}
			}
		}()
	}

	return nil
}

// Serve runs a UDP server, blocking until the server has shut down.
func (s *Server) Serve() {
	glog.V(0).Info("Starting UDP on ", s.config.UDPConfig.ListenAddr)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			select {
			case <-s.closing:
				return
			case <-time.After(time.Hour):
				s.connIDGen.NewIV()
			}
		}
	}()

	if err := s.serve(); err != nil {
		glog.Errorf("Failed to run UDP server: %s", err.Error())
	} else {
		glog.Info("UDP server shut down cleanly")
	}
}

// Stop cleanly shuts down the server.
func (s *Server) Stop() {
	close(s.closing)
	s.sock.SetReadDeadline(time.Now())
	s.wg.Wait()
}

// NewServer returns a new UDP server for a given configuration and tracker.
func NewServer(cfg *config.Config, tkr *tracker.Tracker) *Server {
	gen, err := NewConnectionIDGenerator()
	if err != nil {
		panic(err)
	}

	return &Server{
		config:    cfg,
		tracker:   tkr,
		connIDGen: gen,
		closing:   make(chan struct{}),
		booting:   make(chan struct{}),
	}
}
