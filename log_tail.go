package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hpcloud/tail"
)

type logTailService struct {
	currentTailerLck sync.RWMutex
	currentTailer    map[string]*logTail
}

type BlockProducingStatus struct {
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
	latestBlockProducingStatus BlockProducingStatus
	file                       os.FileInfo
	latestLine                 string
	isSuspectDown              bool
	statusHub                  *Hub
	logHub                     *Hub
}

func openLatestLogForStream(logDir, chain string, nodeNumber int, fileList []os.FileInfo, lHub *Hub, statusHub *Hub) *logTail {
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
		newTailer := &logTail{
			nodeNumber: nodeNumber,
			logDir:     logDir,
			chain:      chain,
			logHub:     lHub,
			statusHub:  statusHub,
		}
		newTailer.file = logFile
		return newTailer
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

func (lsrv *logTailService) Init(logDir string, lHub *logHub, statusHub *Hub) {
	lsrv.currentTailer = make(map[string]*logTail)
	files, err := ioutil.ReadDir(logDir)
	if err != nil {
		log.Fatal(err)
	}

	for i := 0; i <= NumberOfBeaconNode-1; i++ {
		go func(node int) {
			n := "beacon" + strconv.Itoa(node)
			hub := newHub()
			lHub.add(n, hub)
			streamer := openLatestLogForStream(logDir, "beacon", node, files, hub, statusHub)
			lsrv.addLogStreamer(n, streamer)
			go streamer.Run()
		}(i)
	}

	for s := 0; s <= NumberOfShards-1; s++ {
		for i := 0; i <= NumberOfNodePerShard-1; i++ {
			go func(shard int, node int) {
				shardChain := "shard" + strconv.Itoa(shard)
				n := shardChain + strconv.Itoa(node)
				hub := newHub()
				lHub.add(n, hub)
				streamer := openLatestLogForStream(logDir, shardChain, node, files, hub, statusHub)
				lsrv.addLogStreamer(n, streamer)
				go streamer.Run()
			}(s, i)
		}
	}
}

func (lsrv *logTailService) addLogStreamer(node string, streamer *logTail) {
	lsrv.currentTailerLck.Lock()
	lsrv.currentTailer[node] = streamer
	lsrv.currentTailerLck.Unlock()
}

func (l *logTail) readLogLine(line string) {
	if strings.Contains(line, "Consensus log") {
		var re1 = regexp.MustCompile(`(?m)BFT: new round => (\w+) (\d+) (\d+)`)
		bftStatus := re1.FindAllStringSubmatch(line, -1)
		if len(bftStatus) == 1 {
			l.latestBlockProducingStatus.Phase = bftStatus[0][1]
			height, _ := strconv.Atoi(bftStatus[0][2])
			l.latestBlockProducingStatus.BlockHeight = int64(height)
			round, _ := strconv.Atoi(bftStatus[0][3])
			l.latestBlockProducingStatus.Round = round
			l.latestBlockProducingStatus.IsBlockReceived = false
			l.latestBlockProducingStatus.IsVoteSent = false
			l.latestBlockProducingStatus.VoteCount = 0
			return
		}
		line = strings.ToLower(line)
		// "receive block" doesn't mean node has received the right block!
		if strings.Contains(line, "enter voting phase") || strings.Contains(line, "sending vote...") {
			l.latestBlockProducingStatus.Phase = "VOTING"
			l.latestBlockProducingStatus.IsBlockReceived = true
			if strings.Contains(line, "sending vote...") {
				l.latestBlockProducingStatus.IsVoteSent = true
				return
			}
			return
		}
		if strings.Contains(line, "vote added") {
			l.latestBlockProducingStatus.VoteCount++
			return
		}
		if strings.Contains(line, "commit block") {
			l.latestBlockProducingStatus.Phase = "COMMIT"
			return
		}
	}
}

func (l *logTail) tailLog() {
	t, err := tail.TailFile(l.logDir+"/"+l.file.Name(), tail.Config{
		Follow:   true,
		Location: &tail.SeekInfo{0, io.SeekEnd},
	})
	if err != nil {
		log.Fatal(err)
	}
	for line := range t.Lines {
		l.logHub.broadcast <- []byte(line.Text)
	}
}

func (l *logTail) Run() {
	go l.suspectDown()
	go l.getLatestConsensusStatus()
	go l.tailLog()
}

func (l *logTail) getLatestConsensusStatus() {
	previousFileSize := int64(0)
	t := time.NewTicker(6 * time.Second)
	//scan file in reverse byte by byte until we find a new line -> analyze that line if it have 'BFT: new round' then read forward from that line until EOF to get latest consensus status

	char := make([]byte, 1)
	line := ""
	var cursor int64
	readBackward := true
	for {
		<-t.C
		fileHandle, err := os.OpenFile(l.logDir+"/"+l.file.Name(), os.O_RDONLY, 0666)
		if err != nil {
			log.Fatal("Cannot open file")
		}
		cursor = 0
		stat, _ := fileHandle.Stat()
		filesize := stat.Size()
		readBackward = true
		if previousFileSize != filesize {
			l.isSuspectDown = false
		}
		for {
			if readBackward {
				cursor-- //going backward
				fileHandle.Seek(cursor, io.SeekEnd)
				fileHandle.Read(char)
				if cursor != -1 && (char[0] == 10 || char[0] == 13) { // stop if we find a line
					if strings.Contains(line, "BFT: new round") {
						readBackward = false
						line = ""
						continue
					}
					line = ""
					continue
				}
				line = fmt.Sprintf("%s%s", string(char), line) // there is more efficient way
				if cursor == -filesize {                       // stop if we are at the begining
					break
				}
			} else {
				scanner := bufio.NewScanner(fileHandle)
				for scanner.Scan() {
					l.readLogLine(scanner.Text())
				}
				statusBytes, _ := json.Marshal(LogStatusReponse{
					Node:            l.nodeNumber,
					Chain:           l.chain,
					ProducingStatus: l.latestBlockProducingStatus,
					IsSuspectDown:   l.isSuspectDown,
				})
				l.statusHub.broadcast <- statusBytes
				break
			}
		}
		previousFileSize = filesize
		fileHandle.Close()
	}
}

func (l *logTail) suspectDown() {
	t := time.NewTicker(2 * time.Minute)
	for {
		<-t.C
		l.isSuspectDown = true
	}
}
