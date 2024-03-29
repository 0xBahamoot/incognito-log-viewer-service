package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hpcloud/tail"
)

type logTailService struct {
	currentTailerLck sync.RWMutex
	currentTailer    map[string]*logTail
	chainBlockHeight map[string]int
	notiChan         chan string
	notiArray        []string
}

type logTail struct {
	chain                      string
	nodeNumber                 int
	logDir                     string
	file                       os.FileInfo
	latestBlockProducingStatus BlockProducingStatus
	isSuspectDown              bool
	isSuspectDownCount         int
	statusHub                  *Hub
	logHub                     *Hub
	resetTailLog               chan struct{}
	errorsCount                int
	latestErrorLine            string
	heightsRecordLck           sync.RWMutex
	heightsRecord              map[int]*heightRecord
	internalBuf                []byte
	logService                 *logTailService
	lastAlertSend              time.Time
}

type heightRecord struct {
	round      int
	start      int
	end        int
	startTime  string
	errorCount int
}

func openLatestLogForStream(logDir, chain string, nodeNumber int, fileList []os.FileInfo, lHub *Hub, statusHub *Hub) *logTail {
	filePrefix, fileSuffix := getLogFileName(chain, nodeNumber)
OPENLATEST:
	logFile := getLogFileForFileList(filePrefix, fileSuffix, fileList)
	if logFile != nil {
		newTailer := &logTail{
			chain:        chain,
			nodeNumber:   nodeNumber,
			logDir:       logDir,
			logHub:       lHub,
			statusHub:    statusHub,
			file:         logFile,
			resetTailLog: make(chan struct{}),
		}
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
	lsrv.chainBlockHeight = make(map[string]int)
	files, err := ioutil.ReadDir(logDir)
	if err != nil {
		log.Fatal(err)
	}
	go lsrv.slackHook()
	for i := 0; i <= NumberOfBeaconNode-1; i++ {
		go func(node int) {
			n := "beacon" + strconv.Itoa(node)
			hub := newHub()
			lHub.add(n, hub)
			streamer := openLatestLogForStream(logDir, "beacon", node, files, hub, statusHub)
			streamer.logService = lsrv
			lsrv.addLogStreamer(n, streamer)
			streamer.Run()
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
				streamer.logService = lsrv
				lsrv.addLogStreamer(n, streamer)
				streamer.Run()
			}(s, i)
		}
	}
}

func (lsrv *logTailService) addLogStreamer(node string, streamer *logTail) {
	lsrv.currentTailerLck.Lock()
	lsrv.currentTailer[node] = streamer
	lsrv.currentTailerLck.Unlock()
}

func (lsrv *logTailService) updateBlockHeight(chain string, height int) {
	lsrv.currentTailerLck.Lock()
	defer lsrv.currentTailerLck.Unlock()
	if h, ok := lsrv.chainBlockHeight[chain]; ok {
		if height > h {
			lsrv.chainBlockHeight[chain] = height
		}
	} else {
		lsrv.chainBlockHeight[chain] = height
	}
	return
}

func (lsrv *logTailService) getBlockHeight(chain string) int {
	lsrv.currentTailerLck.RLock()
	defer lsrv.currentTailerLck.RUnlock()
	if h, ok := lsrv.chainBlockHeight[chain]; ok {
		return h
	}
	return 0
}

func (lsrv *logTailService) slackHook() {
	lsrv.notiChan = make(chan string)
	t := time.NewTicker(30 * time.Second)
	for {
		select {
		case noti := <-lsrv.notiChan:
			lsrv.currentTailerLck.Lock()
			lsrv.notiArray = append(lsrv.notiArray, noti)
			lsrv.currentTailerLck.Unlock()
		case <-t.C:
			lsrv.currentTailerLck.Lock()
			if len(lsrv.notiArray) > 0 {
				texts := strings.Join(lsrv.notiArray, "\n")
				sendSlackNoti(texts)
				lsrv.notiArray = []string{}
			}
			lsrv.currentTailerLck.Unlock()
		}
	}
}

func (l *logTail) readLogLine(line string, lineCount int) {
	line = strings.ToLower(line)
	currentHeight := int(l.latestBlockProducingStatus.BlockHeight)
	if strings.Contains(line, "consensus log") {
		var re1 = regexp.MustCompile(`(?m)(\w+) ts: (\d+), (\w+) block (\d+), round (\d+)`)
		bftStatus := re1.FindAllStringSubmatch(line, -1)
		if len(bftStatus) == 1 {
			timeslot, _ := strconv.Atoi(bftStatus[0][2])
			height, _ := strconv.Atoi(bftStatus[0][4])
			round, _ := strconv.Atoi(bftStatus[0][5])
			l.latestBlockProducingStatus.Phase = strings.ToUpper(bftStatus[0][3])
			l.latestBlockProducingStatus.BlockHeight = int64(height)
			l.latestBlockProducingStatus.Round = round
			l.latestBlockProducingStatus.Timeslot = timeslot
			l.latestBlockProducingStatus.IsBlockReceived = false
			l.latestBlockProducingStatus.IsVoteSent = false
			l.latestBlockProducingStatus.VoteCount = 0

			if currentHeight != height && currentHeight != 0 {
				record := l.heightsRecord[currentHeight]
				record.end = lineCount - 1
			}
			if currentHeight == height {
				record := l.heightsRecord[currentHeight]
				record.round = round
			}
			currentHeight = height
			if _, ok := l.heightsRecord[currentHeight]; !ok {
				sline := strings.Split(line, " ")
				record := heightRecord{
					start:     lineCount - 1,
					round:     round,
					startTime: sline[1],
				}
				l.heightsRecord[currentHeight] = &record
				l.logService.updateBlockHeight(l.chain, currentHeight)
			}

			return
		}

		if strings.Contains(line, "sending vote...") {
			l.latestBlockProducingStatus.Phase = "VOTING"
			l.latestBlockProducingStatus.IsBlockReceived = true
			if strings.Contains(line, "sending vote...") {
				l.latestBlockProducingStatus.IsVoteSent = true
				return
			}
			return
		}
		if strings.Contains(line, "receive vote") {
			var re1 = regexp.MustCompile(`(?m)(\w+) receive vote \((\d+)\) for block (\w+) from validator (\d+) (\w+)`)
			vote := re1.FindAllStringSubmatch(line, -1)
			if len(vote) == 1 {
				l.latestBlockProducingStatus.VoteCount, _ = strconv.Atoi(vote[0][2])
			}
			return
		}
		if strings.Contains(line, "commit block") {
			l.latestBlockProducingStatus.Phase = "COMMIT"
			return
		}
	}

	//update errors
	if strings.Contains(line, "[err]") {
		l.errorsCount++
		l.latestErrorLine = line
		if l.latestBlockProducingStatus.BlockHeight != 0 {
			record := l.heightsRecord[int(l.latestBlockProducingStatus.BlockHeight)]
			record.errorCount += 1
		}
	}

	if currentHeight != 0 {
		record := l.heightsRecord[currentHeight]
		record.end = lineCount - 1
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
	lineCount := 0
	if l.latestBlockProducingStatus.BlockHeight != 0 {
		lineCount = l.heightsRecord[int(l.latestBlockProducingStatus.BlockHeight)].end
	}
	for {
		select {
		case <-l.resetTailLog:
			t.Stop()
			l.errorsCount = 0
			l.latestErrorLine = ""
			t, err = tail.TailFile(l.logDir+"/"+l.file.Name(), tail.Config{
				Follow:   true,
				Location: &tail.SeekInfo{0, io.SeekStart},
			})
			if err != nil {
				log.Fatal(err)
			}
			lineCount = 0
			l.heightsRecord = make(map[int]*heightRecord)
			l.latestBlockProducingStatus = BlockProducingStatus{}
			l.isSuspectDownCount = 0
			log.Println("Reset tailler successful")
		case line := <-t.Lines:
			lineCount++
			l.readLogLine(line.Text, lineCount)
			l.isSuspectDownCount = 0
			go func() {
				l.logHub.broadcast <- []byte(line.Text)
			}()
		}

	}
}

func (l *logTail) RetrieveLineFromEOF(lines int) []string {
	fileHandle, err := os.OpenFile(l.logDir+"/"+l.file.Name(), os.O_RDONLY, 0666)
	if err != nil {
		log.Fatal("Cannot open file")
	}
	defer fileHandle.Close()
	result := []string{}
	cursor := int64(0)
	stat, _ := fileHandle.Stat()
	filesize := stat.Size()
	char := make([]byte, 1)
	currentReadLines := 0
	line := ""
	for {
		cursor-- //going backward
		fileHandle.Seek(cursor, io.SeekEnd)
		fileHandle.Read(char)
		if cursor != -1 && (char[0] == 10 || char[0] == 13) { // stop if we find a line
			currentReadLines++
			result = append(result, line)
			line = ""
			continue
		}
		line = fmt.Sprintf("%s%s", string(char), line) // there is more efficient way
		if cursor == -filesize {                       // stop if we are at the begining
			break
		}
		if currentReadLines >= lines {
			break
		}
	}
	return result
}

func (l *logTail) Run() {
	l.heightsRecord = make(map[int]*heightRecord)
	fileHandle, err := os.OpenFile(l.logDir+"/"+l.file.Name(), os.O_RDONLY, 0666)
	if err != nil {
		log.Fatal("Cannot open file")
	}
	lineCount := 1
	scanner := bufio.NewScanner(fileHandle)
	l.internalBuf = make([]byte, 0, 64*1024)
	scanner.Buffer(l.internalBuf, 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		lineCount++
		l.readLogLine(line, lineCount)
	}
	if err := scanner.Err(); err != nil {
		log.Fatal("Something went wrong here!", lineCount, err)
		panic(1)
	}

	go l.tailLog()
	go l.suspectDown()
	go l.sendLatestConsensusStatus()
	go func() {
	DAYWATCH:
		nextDate, _ := time.Parse("2006-01-02", time.Now().AddDate(0, 0, 1).Format("2006-01-02"))
		t := time.NewTimer(time.Until(nextDate.Add(10 * time.Second)))
		<-t.C
		log.Println("Resetting log tailler")
		filePrefix, fileSuffix := getLogFileName(l.chain, l.nodeNumber)
	GETLOGFILE:
		fileList, err := ioutil.ReadDir(l.logDir)
		if err != nil {
			log.Fatal(err)
		}
		logFile := getLogFileForFileList(filePrefix, fileSuffix, fileList)
		if logFile == nil {
			time.Sleep(5 * time.Second)
			goto GETLOGFILE
		}
		l.file = logFile
		l.resetTailLog <- struct{}{}
		goto DAYWATCH
	}()
}

func (l *logTail) sendLatestConsensusStatus() {
	t := time.NewTicker(3 * time.Second)
	for {
		<-t.C
		status := LogStatusReponse{
			Node:            l.nodeNumber,
			Chain:           l.chain,
			ProducingStatus: l.latestBlockProducingStatus,
			IsSuspectDown:   l.isSuspectDown,
			ErrorsCount:     l.errorsCount,
			LatestErrorLine: l.latestErrorLine,
		}
		if chainHeight := l.logService.getBlockHeight(l.chain); int(l.latestBlockProducingStatus.BlockHeight) <= chainHeight-5 && l.latestBlockProducingStatus.BlockHeight != 0 {
			status.IsSuspectDown = true
			if time.Now().Sub(l.lastAlertSend) > time.Hour {
				node := l.chain + strconv.Itoa(l.nodeNumber)
				line := fmt.Sprintf("Node %v block height is behind %v 😱", node, chainHeight-int(l.latestBlockProducingStatus.BlockHeight))
				log.Println(line)
				l.logService.notiChan <- line
				l.lastAlertSend = time.Now()
			}
		}

		if l.isSuspectDown && l.isSuspectDownCount > 10 && time.Now().Sub(l.lastAlertSend) > time.Hour {
			node := l.chain + strconv.Itoa(l.nodeNumber)
			line := fmt.Sprintf("Node %v stopped logging 😱", node)
			log.Println(line)
			l.logService.notiChan <- line
			l.lastAlertSend = time.Now()
		}
		statusBytes, _ := json.Marshal(status)
		l.statusHub.broadcast <- statusBytes
	}
}

func (l *logTail) suspectDown() {
	t := time.NewTicker(30 * time.Second)
	for {
		<-t.C
		l.isSuspectDownCount++
		if l.isSuspectDownCount >= 10 {
			l.isSuspectDown = true
		} else {
			l.isSuspectDown = false
		}
	}
}

func (l *logTail) GetLogOfHeight(height int) []string {
	var result []string
	blkHeight, ok := l.heightsRecord[height]
	if !ok {
		return nil
	}

	fileHandle, err := os.OpenFile(l.logDir+"/"+l.file.Name(), os.O_RDONLY, 0666)
	if err != nil {
		log.Fatal("Cannot open file")
	}

	fileHandle.Seek(0, io.SeekStart)
	scanner := bufio.NewScanner(fileHandle)
	currentLine := 1
	scanner.Buffer(l.internalBuf, 1024*1024)
	for scanner.Scan() {
		if currentLine >= blkHeight.start {
			result = append(result, scanner.Text())
		}
		currentLine++
		if (blkHeight.end != 0) && ((currentLine - blkHeight.start) == (blkHeight.end - blkHeight.start)) {
			break
		}
	}
	return result
}

type BlockInfo struct {
	Round      int
	Height     int
	ErrorCount int
	StartTime  string
}

func (l *logTail) GetHeightsRecord() []BlockInfo {
	var result []BlockInfo
	l.heightsRecordLck.RLock()
	var sortRecord []int
	for h := range l.heightsRecord {
		sortRecord = append(sortRecord, h)
	}
	sort.Ints(sortRecord)

	for _, height := range sortRecord {
		result = append(result, BlockInfo{Round: l.heightsRecord[height].round, Height: height, ErrorCount: l.heightsRecord[height].errorCount, StartTime: l.heightsRecord[height].startTime})
	}
	l.heightsRecordLck.RUnlock()
	return result
}

func getLogFileName(chain string, nodeNumber int) (string, string) {
	date := time.Now().Format("2006-01-02")
	filePrefix := chain + strconv.Itoa(nodeNumber)
	if chain == "beacon" {
		filePrefix += "_fullnode"
	} else {
		filePrefix += "_new"
	}
	fileSuffix := date + ".log"
	return filePrefix, fileSuffix
}

//getLogFileForFileList is to guarantee to get the latest log file in case of IP change mid day
func getLogFileForFileList(filePrefix, fileSuffix string, fileList []os.FileInfo) os.FileInfo {
	var logFile os.FileInfo
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
	return logFile
}

func sendSlackNoti(text string) {
	content := struct {
		Text string `json:"text"`
	}{
		Text: text,
	}
	contentBytes, err := json.Marshal(content)
	if err != nil {
		log.Println(err)
		return
	}
	httpClient := http.DefaultClient
	resp, err := httpClient.Post(os.Getenv("SLACKHOOK"), "application/json", bytes.NewReader(contentBytes))
	if resp.Status != "200" || err != nil {
		log.Println(err)
		body, _ := ioutil.ReadAll(resp.Body)
		log.Println(string(body))
	}
	defer resp.Body.Close()
	return
}
