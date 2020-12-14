package main

import (
	"encoding/json"
	"net/http"
	"syscall"
	"time"
)

var diskLeft uint64

type systemInfo struct {
	Diskleft uint64
}

func watchDiskUsage(dir string) {
	for {
		var stat syscall.Statfs_t
		syscall.Statfs(dir, &stat)
		diskLeft = stat.Bavail * uint64(stat.Bsize) / 1000000000
		if diskLeft <= 2 {
			sendSlackNoti("Low disk!!!")
		}
		time.Sleep(30 * time.Minute)
	}
}

func diskLeftHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sys := systemInfo{
		Diskleft: diskLeft,
	}
	sysBytes, err := json.Marshal(sys)
	if err != nil {
		panic(err)
	}
	w.WriteHeader(200)
	_, err = w.Write(sysBytes)
	if err != nil {
		panic(err)
	}
	return
}
