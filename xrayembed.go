package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/xtls/xray-core/core"
	_ "github.com/xtls/xray-core/main/distro/all"
	_ "github.com/xtls/xray-core/main/json"
)

// runEmbedded is the entry point of `panje xray [-test] -c file`: the Xray
// engine compiled into this binary. Panje runs it as a child process of
// itself so a crash or restart never takes the panel down.
func runEmbedded(args []string) int {
	fs := flag.NewFlagSet("xray", flag.ExitOnError)
	conf := fs.String("c", "", "config file")
	test := fs.Bool("test", false, "validate config only")
	fs.Parse(args)
	f, err := os.Open(*conf)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cfg, err := core.LoadConfig("json", f)
	f.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return 1
	}
	srv, err := core.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start error:", err)
		return 1
	}
	if *test {
		return 0
	}
	if err := srv.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start error:", err)
		return 1
	}
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM, syscall.SIGINT)
	<-c
	srv.Close()
	return 0
}
