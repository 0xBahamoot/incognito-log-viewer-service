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
}

type logTail struct {
	chain                      string
	nodeNumber                 int
	logDir                     string
	file                       os.FileInfo
	latestBlockProducingStatus BlockProducingStatus
	isSuspectDown              bool
	statusHub                  *Hub
	logHub                     *Hub
	resetTailLog               chan struct{}
	errorsCount                int
	latestErrorLine            string
	heightsRecordLck           sync.RWMutex
	heightsRecord              map[int]*heightRecord
}

type heightRecord struct {
	round int
	start int
	end   int
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

func (l *logTail) readLogLine(line string) {
	line = strings.ToLower(line)
	if strings.Contains(line, "consensus log") {
		var re1 = regexp.MustCompile(`(?m)bft: new round => (\w+) (\d+) (\d+)`)
		bftStatus := re1.FindAllStringSubmatch(line, -1)
		if len(bftStatus) == 1 {
			l.latestBlockProducingStatus.Phase = strings.ToUpper(bftStatus[0][1])
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
	lineCount := 1
	currentHeight := 0
	if l.latestBlockProducingStatus.BlockHeight != 0 {
		lineCount = l.heightsRecord[int(l.latestBlockProducingStatus.BlockHeight)].end
		currentHeight = int(l.latestBlockProducingStatus.BlockHeight)
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
			lineCount = 1
			currentHeight = 0
			l.heightsRecord = make(map[int]*heightRecord)
			log.Println("Reset tailler successful")
		case line := <-t.Lines:
			l.isSuspectDown = false
			lineCount++
			//update errors
			if strings.Contains(strings.ToLower(line.Text), "[err]") {
				l.errorsCount++
				l.latestErrorLine = strings.ToLower(line.Text)
			}

			//update consensus
			if strings.Contains(line.Text, "BFT: new round") {
				var re1 = regexp.MustCompile(`(?m)BFT: new round => (\w+) (\d+) (\d+)`)
				bftStatus := re1.FindAllStringSubmatch(line.Text, -1)
				if len(bftStatus) == 1 {
					height, _ := strconv.Atoi(bftStatus[0][2])
					round, _ := strconv.Atoi(bftStatus[0][3])
					if currentHeight != height && currentHeight != 0 {
						record := l.heightsRecord[currentHeight]
						record.end = lineCount - 1
						record.round = round
						// log.Println(l.chain, l.nodeNumber, currentHeight, l.heightsRecord[currentHeight])
					}
					if currentHeight == height {
						record := l.heightsRecord[currentHeight]
						record.round = round
					}
					currentHeight = height
					if _, ok := l.heightsRecord[currentHeight]; !ok {
						record := heightRecord{
							start: lineCount - 1,
							round: round,
						}
						l.heightsRecord[currentHeight] = &record
					}
				}
			}
			if currentHeight != 0 {
				record := l.heightsRecord[currentHeight]
				record.end = lineCount - 1
			}
			l.readLogLine(line.Text)
			l.logHub.broadcast <- []byte(line.Text)
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
	currentReadLine := 0
	line := ""
	for {
		cursor-- //going backward
		fileHandle.Seek(cursor, io.SeekEnd)
		fileHandle.Read(char)
		if cursor != -1 && (char[0] == 10 || char[0] == 13) { // stop if we find a line
			currentReadLine++
			result = append(result, line)
			line = ""
			continue
		}
		line = fmt.Sprintf("%s%s", string(char), line) // there is more efficient way
		if cursor == -filesize {                       // stop if we are at the begining
			break
		}
		if currentReadLine >= lines {
			break
		}
	}
	return result
}

func (l *logTail) Run() {

	l.heightsRecord = make(map[int]*heightRecord)
	//count and get latest error of log
	fileHandle, err := os.OpenFile(l.logDir+"/"+l.file.Name(), os.O_RDONLY, 0666)
	if err != nil {
		log.Fatal("Cannot open file")
	}

	lineCount := 1
	scanner := bufio.NewScanner(fileHandle)
	currentHeight := 0
	for scanner.Scan() {
		line := scanner.Text()
		lineCount++
		if strings.Contains(strings.ToLower(line), "[err]") {
			l.errorsCount++
			l.latestErrorLine = scanner.Text()
		}
		if strings.Contains(line, "BFT: new round") {
			var re1 = regexp.MustCompile(`(?m)BFT: new round => (\w+) (\d+) (\d+)`)
			bftStatus := re1.FindAllStringSubmatch(line, -1)
			if len(bftStatus) == 1 {
				height, _ := strconv.Atoi(bftStatus[0][2])
				round, _ := strconv.Atoi(bftStatus[0][3])
				if currentHeight != height && currentHeight != 0 {
					record := l.heightsRecord[currentHeight]
					record.end = lineCount - 1
					record.round = round
				}
				if currentHeight == height {
					record := l.heightsRecord[currentHeight]
					record.round = round
				}
				currentHeight = height
				if _, ok := l.heightsRecord[currentHeight]; !ok {
					record := heightRecord{
						start: lineCount - 1,
						round: round,
					}
					l.heightsRecord[currentHeight] = &record
				}
			}
		}
		if currentHeight != 0 {
			record := l.heightsRecord[currentHeight]
			record.end = lineCount - 1
		}

		l.readLogLine(line)
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

	t := time.NewTicker(6 * time.Second)
	for {
		<-t.C
		statusBytes, _ := json.Marshal(LogStatusReponse{
			Node:            l.nodeNumber,
			Chain:           l.chain,
			ProducingStatus: l.latestBlockProducingStatus,
			IsSuspectDown:   l.isSuspectDown,
			ErrorsCount:     l.errorsCount,
			LatestErrorLine: l.latestErrorLine,
		})
		l.statusHub.broadcast <- statusBytes
	}
}

func (l *logTail) suspectDown() {
	t := time.NewTicker(1 * time.Minute)
	for {
		<-t.C
		l.isSuspectDown = true
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
	Round  int
	Height int
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
		result = append(result, BlockInfo{Round: l.heightsRecord[height].round, Height: height})
	}
	l.heightsRecordLck.RUnlock()
	return result
}

func getLogFileName(chain string, nodeNumber int) (string, string) {
	date := time.Now().Format("2006-01-02")
	filePrefix := chain + strconv.Itoa(nodeNumber) + "_new"
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
