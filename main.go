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
	http.ServeFile(w, r, "./web/index.html")
}

type logHub struct {
	hubsLck sync.RWMutex
	hubs    map[string]*Hub
}

func (lhub *logHub) add(key string, hub *Hub) {
	lhub.hubsLck.Lock()
	lhub.hubs[key] = hub
	go lhub.hubs[key].run()
	lhub.hubsLck.Unlock()
}

func main() {
	var addr = flag.String("addr", ":8084", "http service address")
	var logdir = flag.String("dir", "./", "logs directory")

	flag.Parse()

	lHub := logHub{
		hubs: make(map[string]*Hub),
	}

	statusHub := newHub()
	go statusHub.run()

	logService := logTailService{}
	logService.Init(*logdir, &lHub, statusHub)

	fileServer := http.FileServer(http.Dir("./web"))
	http.Handle("/", fileServer)
	// http.Handle("/home", fileServer)
	http.HandleFunc("/downloadlog", downloadLog)
	http.HandleFunc("/streamlog", func(w http.ResponseWriter, r *http.Request) {
		if nodeLogHub, ok := lHub.hubs[r.URL.Query().Get("node")]; ok {
			streamlogWs(nodeLogHub, w, r)
		} else {
			http.Error(w, "Chain not exist", 404)
		}
	})
	http.HandleFunc("/logstatus", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Access-Control-Allow-Origin", "*")
		w.Header().Add("Access-Control-Allow-Methods", "GET")
		streamStatusWs(statusHub, w, r)
	})
	err := http.ListenAndServe(*addr, nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
