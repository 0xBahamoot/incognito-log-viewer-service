package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hpcloud/tail"
)

type logTailService struct {
	currentReaderLck sync.RWMutex
	currentReader    map[string]*logTail
}

type blockProducingStatus struct {
	Phase           string
	IsBlockReceived bool
	IsVoteSent      bool
	VoteCount       int
	BlockHeight     int64
	Round           int
}
type logTail struct {
	chain                      string
	nodeNumber                 int
	logDir                     string
	streamCh                   chan []byte
	latestBlockProducingStatus blockProducingStatus
	file                       os.FileInfo
	latestLine                 string
	isSuspectDown              bool
}

func openLatestLogForStream(logDir, chain string, nodeNumber int, fileList []os.FileInfo) *logTail {
	date := time.Now().Format("2006-01-02")
	filePrefix := chain + strconv.Itoa(nodeNumber) + "_new"
	fileSuffix := date + ".log"
	fmt.Println(filePrefix, fileSuffix)
	var logFile os.FileInfo
OPENLATEST:
	for _, file := range fileList {
		if strings.HasPrefix(file.Name(), filePrefix) {
			if strings.HasSuffix(file.Name(), fileSuffix) {
				if logFile != nil {
					if logFile.ModTime().Unix() < file.ModTime().Unix() {
						logFile = file
						continue
					}
				}
				logFile = file
			}
		}
	}
	if logFile != nil {
		newReader := &logTail{
			streamCh:   make(chan []byte),
			nodeNumber: nodeNumber,
			logDir:     logDir,
			chain:      chain,
		}
		newReader.file = logFile
		return newReader
	}
	//the wanted logFile not exist yet so wait for it
	log.Printf("log file of %v not found, retrying...\n", chain+strconv.Itoa(nodeNumber))
	time.Sleep(10 * time.Minute)
	fileList, err := ioutil.ReadDir(logDir)
	if err != nil {
		log.Fatal(err)
	}
	goto OPENLATEST
}

func (lsrv *logTailService) Init(logDir string, lHub *logHub) {
	lsrv.currentReader = make(map[string]*logTail)
	files, err := ioutil.ReadDir(logDir)
	if err != nil {
		log.Fatal(err)
	}

	for i := 0; i < NumberOfBeaconNode-1; i++ {
		go func(node int) {
			n := "beacon" + strconv.Itoa(node)
			streamer := openLatestLogForStream(logDir, "beacon", node, files)
			lsrv.addLogStreamer(n, streamer)
			go streamer.read()
		}(i)
	}

	for s := 0; s < NumberOfShards; s++ {
		for i := 0; i < NumberOfNodePerShard; i++ {
			go func(shard int, node int) {
				shardChain := "shard" + strconv.Itoa(shard)
				n := shardChain + strconv.Itoa(node)
				streamer := openLatestLogForStream(logDir, shardChain, node, files)
				lsrv.addLogStreamer(n, streamer)
				go streamer.read()
			}(s, i)
		}
	}
}

func (lsrv *logTailService) addLogStreamer(node string, streamer *logTail) {
	lsrv.currentReaderLck.Lock()
	lsrv.currentReader[node] = streamer
	lsrv.currentReaderLck.Unlock()
}

func (l *logTail) readLogLine(line string) {
	log.Println(line)
	l.latestBlockProducingStatus = blockProducingStatus{}
}

func (l *logTail) read() {
	go l.suspectDown()
	t, err := tail.TailFile(l.logDir+"/"+l.file.Name(), tail.Config{
		Follow:   true,
		Location: &tail.SeekInfo{0, os.SEEK_END},
	})
	if err != nil {
		log.Fatal(err)
	}
	for line := range t.Lines {
		l.readLogLine(line.Text)
		l.isSuspectDown = false
	}
}

func (l *logTail) suspectDown() {
	t := time.NewTicker(5 * time.Minute)
	for {
		<-t.C
		l.isSuspectDown = true
	}
}
