package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "xray" {
		os.Exit(runEmbedded(os.Args[2:]))
	}
	conf := flag.String("c", "/etc/panje/config.json", "config file")
	listen := flag.String("l", "", "override panel listen address")
	flag.Parse()

	st, initial, err := OpenStore(*conf)
	if err != nil {
		log.Fatal(err)
	}
	if flag.Arg(0) == "passwd" {
		if flag.NArg() < 2 {
			log.Fatal("usage: panje passwd <new password>")
		}
		st.Update(func(d *Data) { st.setPass(flag.Arg(1)) })
		fmt.Println("password updated")
		return
	}
	if initial != "" {
		fmt.Printf("\n*** first run: panel password is %s  (change: panje passwd <new>) ***\n\n", initial)
	}

	core := NewCore(st)
	d := st.Snapshot().Settings
	if d.Enabled && d.AutoStart {
		go func() {
			if err := core.Apply(); err != nil {
				core.logf("autostart: %v", err)
			}
		}()
	}
	go core.Background()

	addr := d.Listen
	if *listen != "" {
		addr = *listen
	}
	srv := &http.Server{Addr: addr, Handler: NewServer(st, core).Handler()}
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGTERM, syscall.SIGINT)
		<-c
		core.Stop()
		os.Exit(0)
	}()
	core.logf("panel on http://%s", addr)
	log.Fatal(srv.ListenAndServe())
}
