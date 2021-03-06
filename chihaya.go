// Copyright 2015 The Chihaya Authors. All rights reserved.
// Use of this source code is governed by the BSD 2-Clause license,
// which can be found in the LICENSE file.

// Package chihaya implements the ability to boot the Chihaya BitTorrent
// tracker with your own imports that can dynamically register additional
// functionality.
package chihaya

import (
	"flag"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"

	"github.com/golang/glog"

	"github.com/psaab/chihaya/api"
	"github.com/psaab/chihaya/config"
	"github.com/psaab/chihaya/http"
	"github.com/psaab/chihaya/stats"
	"github.com/psaab/chihaya/tracker"
	"github.com/psaab/chihaya/udp"
)

var (
	maxProcs   int
	configPath string
)

func init() {
	flag.IntVar(&maxProcs, "maxprocs", runtime.NumCPU(), "maximum parallel threads")
	flag.StringVar(&configPath, "config", "", "path to the configuration file")
}

type server interface {
	Serve()
	Stop()
}

// Boot starts Chihaya. By exporting this function, anyone can import their own
// custom drivers into their own package main and then call chihaya.Boot.
func Boot() {
	defer glog.Flush()

	flag.Parse()

	runtime.GOMAXPROCS(maxProcs)
	glog.V(1).Info("Set max threads to ", maxProcs)

	debugBoot()
	defer debugShutdown()

	cfg, err := config.Open(configPath)
	if err != nil {
		glog.Fatalf("Failed to parse configuration file: %s\n", err)
	}

	if cfg == &config.DefaultConfig {
		glog.V(1).Info("Using default config")
	} else {
		glog.V(1).Infof("Loaded config file: %s", configPath)
	}

	stats.DefaultStats = stats.New(cfg.StatsConfig)

	tkr, err := tracker.New(cfg)
	if err != nil {
		glog.Fatal("New: ", err)
	}

	var servers []server

	if cfg.APIConfig.ListenAddr != "" {
		servers = append(servers, api.NewServer(cfg, tkr))
	}

	if cfg.HTTPConfig.ListenAddr != "" {
		servers = append(servers, http.NewServer(cfg, tkr))
	}

	if cfg.UDPConfig.ListenAddr != "" {
		servers = append(servers, udp.NewServer(cfg, tkr))
	}

	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)

		// If you don't explicitly pass the server, every goroutine captures the
		// last server in the list.
		go func(srv server) {
			defer wg.Done()
			srv.Serve()
		}(srv)
	}

	shutdown := make(chan os.Signal)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		wg.Wait()
		signal.Stop(shutdown)
		close(shutdown)
	}()

	<-shutdown
	glog.Info("Shutting down...")

	for _, srv := range servers {
		srv.Stop()
	}

	<-shutdown

	if err := tkr.Close(); err != nil {
		glog.Errorf("Failed to shut down tracker cleanly: %s", err.Error())
	}
}
