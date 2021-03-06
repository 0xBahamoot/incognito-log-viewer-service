package main

import (
	"encoding/json"
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
	go watchDiskUsage(*logdir)
	logService := logTailService{}
	logService.Init(*logdir, &lHub, statusHub)

	fileServer := http.FileServer(http.Dir("./web"))
	http.Handle("/", fileServer)
	staticHandler := http.StripPrefix("/logviewer", http.FileServer(http.Dir("./web")))
	http.HandleFunc("/getdiskleft", diskLeftHandler)
	http.Handle("/logviewer", staticHandler)
	http.HandleFunc("/downloadlog", downloadLog)
	http.HandleFunc("/streamlog", func(w http.ResponseWriter, r *http.Request) {
		node := r.URL.Query().Get("node")

		if nodeLogHub, ok := lHub.hubs[node]; ok {
			//retrieve lines from EOF
			lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
			preStreamLog := []string{}
			if lines > 0 {
				preStreamLog = logService.currentTailer[node].RetrieveLineFromEOF(lines)
			}
			streamlogWs(nodeLogHub, w, r, preStreamLog)
		} else {
			http.Error(w, "Chain not exist", 404)
		}
	})
	http.HandleFunc("/getnodesheight", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		node := r.URL.Query().Get("node")
		if _, ok := lHub.hubs[node]; ok {
			heights := logService.currentTailer[node].GetHeightsRecord()
			heightsByte, _ := json.Marshal(heights)
			w.Write(heightsByte)
			return
		} else {
			http.Error(w, "Chain not exist", 404)
		}

	})
	http.HandleFunc("/getheightlog", func(w http.ResponseWriter, r *http.Request) {
		node := r.URL.Query().Get("node")
		if _, ok := lHub.hubs[node]; ok {
			height, _ := strconv.Atoi(r.URL.Query().Get("height"))
			heightlogs := []string{}
			if height > 0 {
				heightlogs = logService.currentTailer[node].GetLogOfHeight(height)
			}
			streamOnceWs(w, r, heightlogs)
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
