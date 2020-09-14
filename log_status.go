package main

import (
	"log"
	"net/http"
)

type LogStatusReponse struct {
	Node            int
	Chain           string
	ProducingStatus BlockProducingStatus
	IsSuspectDown   bool
	ErrorsCount     int
	LatestErrorLine string
}

type BlockProducingStatus struct {
	Phase           string
	IsBlockReceived bool
	IsVoteSent      bool
	VoteCount       int
	BlockHeight     int64
	Round           int
}

// streamlogWs handles websocket requests from the peer.
func streamStatusWs(hub *Hub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	client := &LogStreamer{hub: hub, conn: conn, send: make(chan []byte, 256), id: HashH([]byte(r.RemoteAddr))}
	client.hub.register <- client

	// Allow collection of memory referenced by the caller by doing all work in
	// new goroutines.
	go client.writePump()
	// go client.readPump()
}
