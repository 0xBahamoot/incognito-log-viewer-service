package main

import (
	"flag"
	"log"
	"net/http"
	"sync"
)

func serveHome(w http.ResponseWriter, r *http.Request) {
	log.Println(r.URL)
	if r.URL.Path != "/" {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	http.ServeFile(w, r, "home.html")
}

type logHub struct {
	hubsLck sync.RWMutex
	hubs    map[string]Hub
}

func main() {
	var addr = flag.String("addr", ":8080", "http service address")
	var logdir = flag.String("dir", "./", "logs directory")

	flag.Parse()

	lHub := logHub{
		hubs: make(map[string]Hub),
	}

	statusHub := newHub()

	logService := logTailService{}
	logService.Init(*logdir, &lHub)

	hub := newHub()
	go hub.run()
	http.HandleFunc("/", serveHome)

	http.HandleFunc("/downloadlog", downloadLog)
	http.HandleFunc("/streamlog", func(w http.ResponseWriter, r *http.Request) {
		streamlogWs(hub, w, r)
	})
	http.HandleFunc("/logstatus", func(w http.ResponseWriter, r *http.Request) {
		streamlogWs(statusHub, w, r)
	})
	err := http.ListenAndServe(*addr, nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
