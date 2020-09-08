package main

import (
	"flag"
	"log"
	"net/http"
	"strconv"
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
	hubs    map[string]*Hub
}

func main() {
	var addr = flag.String("addr", ":8084", "http service address")
	var logdir = flag.String("dir", "./", "logs directory")

	flag.Parse()

	lHub := logHub{
		hubs: make(map[string]*Hub),
	}

	lHub.hubs["beacon"] = newHub()
	go lHub.hubs["beacon"].run()
	for i := 0; i < NumberOfShards-1; i++ {
		shardChain := "shard" + strconv.Itoa(i)
		lHub.hubs[shardChain] = newHub()
		go lHub.hubs[shardChain].run()
	}

	statusHub := newHub()
	go statusHub.run()

	logService := logTailService{}
	logService.Init(*logdir, &lHub, statusHub)

	http.HandleFunc("/", serveHome)

	http.HandleFunc("/downloadlog", downloadLog)
	http.HandleFunc("/streamlog", func(w http.ResponseWriter, r *http.Request) {
		if chainLogHub, ok := lHub.hubs[r.Header.Get("chain")]; ok {
			streamlogWs(chainLogHub, w, r)
		} else {
			http.Error(w, "Chain not exist", 404)
		}
	})
	http.HandleFunc("/logstatus", func(w http.ResponseWriter, r *http.Request) {
		streamStatusWs(statusHub, w, r)
	})
	err := http.ListenAndServe(*addr, nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
