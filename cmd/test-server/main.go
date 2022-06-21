// Reference for webserver that speaks websocket: https://github.com/gorilla/websocket
// Reference for client side websocket code: https://web.archive.org/web/20210614154432/https://incolumitas.com/2021/06/07/detecting-proxies-and-vpn-with-latencies/
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/go-ping/ping"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"html/template"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path"
	"github.com/montanaflynn/stats"
	"strings"
	"time"
)

const ICMPCount = 5
const ICMPTimeout = time.Second * 10
const TCPCounter = 5
const TCPTimeout = time.Duration(1000) * time.Millisecond // TCP RTO is 1s (RFC 6298), so having a 1s timeout for RTT measurement makes sense
const TCPInterval = time.Duration(1100) * time.Millisecond

var PortsToTest = [...]int{53, 80, 443, 3389, 8080, 8443, 9100}
var WebTemplate, _ = template.ParseFiles("index.html")

// Use with default options
var upgrader = websocket.Upgrader{}

var (
	InfoLogger *log.Logger
)
var (
	ErrLogger *log.Logger
)

type RtItem struct {
	IP        string
	PktSent   int
	PktRecv   int
	PktLoss   float64
	MinRtt    float64
	AvgRtt    float64
	MaxRtt    float64
	StdDevRtt float64
}

type tcpProbeResult struct {
	Destination    string
	SequenceNumber uint64
	Timeinms       float64
}

type tcpResult struct {
	Destination string
	TimesInms   []float64
	MinRtt    float64
	AvgRtt    float64
	MaxRtt    float64
	StdDevRtt float64
}

type Results struct {
	UUID     string
	IPaddr   string
	IcmpPing []RtItem
	TcpPing  []tcpResult
}

// Implementing this since Golang time.Milliseconds() function only returns an int64 value
func fmtTimeMs(value time.Duration) float64 {
	return (float64(value) / float64(time.Millisecond))
}

// Handler for the echo webserver that speaks WebSocket
func echoHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/echo" {
		http.NotFound(w, r)
		return
	}
	if r.Method != "GET" {
		w.WriteHeader(http.StatusNotImplemented)
		w.Write([]byte(http.StatusText(http.StatusNotImplemented)))
		return
	}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		ErrLogger.Println("upgrade:", err)
		return
	}
	defer c.Close()
	for {
		mt, message, err := c.ReadMessage()
		if err != nil {
			ErrLogger.Println("read:", err)
			break
		}
		// ReadMessage() returns messageType int, p []byte, err error]
		var wsData map[string]interface{}
		json.Unmarshal(message, &wsData)
		if wsData["type"] != "ws-latency" {
			// Only log the final message with all latencies calculated
			InfoLogger.Println(string(message))
		}
		err = c.WriteMessage(mt, message)
		if err != nil {
			ErrLogger.Println("write:", err)
			break
		}
	}
}

func getStats(arr []float64) (float64, float64, float64, float64) {
	data := stats.LoadRawData(arr)
	min, _ := stats.Min(data)
	avg, _ := stats.Mean(data)
	max, _ := stats.Max(data)
	stddev, _ := stats.StandardDeviation(data)
	return min, avg, max, stddev
}

// Function that sends out TcpPing
func pingTcp(dst string, seq uint64, timeout time.Duration) float64 {
	startTime := time.Now()
	conn, err := net.DialTimeout("tcp", dst, timeout)
	endTime := time.Now()
	if err == nil || strings.Contains(err.Error(), "connection refused") {
		if err == nil {
			defer conn.Close()
		}
		var t = fmtTimeMs(endTime.Sub(startTime))
		result := tcpProbeResult{dst, seq, t}
		resultJson, parseErr := json.Marshal(result)
		if parseErr != nil {
			ErrLogger.Println("JSON Error in TCPing: ", parseErr)
		} else {
			resultString := string(resultJson)
			// Intermediate results also logged to ErrLogger
			ErrLogger.Println(resultString)
		}
		return t
	} else {
		ErrLogger.Println(dst, " connection failed with:", err)
	}
	return 0
}

// Handler for ICMP and TCP measurements which also serves the webpage via a template
func pingHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/ping" {
		http.NotFound(w, r)
		return
	}
	if r.Method != "GET" {
		w.WriteHeader(http.StatusNotImplemented)
		w.Write([]byte(http.StatusText(http.StatusNotImplemented)))
		return
	}
	clientIPstr := r.RemoteAddr
	clientIP, _, _ := net.SplitHostPort(clientIPstr)
	var expUUID = uuid.NewString()

	// ICMP Pinger
	pinger, err := ping.NewPinger(clientIP)
	if err != nil {
		panic(err)
	}
	pinger.Count = ICMPCount
	pinger.Timeout = ICMPTimeout
	err = pinger.Run() // Blocks until finished.
	if err != nil {
		panic(err)
	}
	stat := pinger.Statistics()
	var icmp []RtItem
	icmp = append(icmp, RtItem{clientIP, stat.PacketsSent, stat.PacketsRecv, stat.PacketLoss, fmtTimeMs(stat.MinRtt), fmtTimeMs(stat.AvgRtt), fmtTimeMs(stat.MaxRtt), fmtTimeMs(stat.StdDevRtt)})

	// TCP Pinger
	var tcpResultArr []tcpResult
	rand.Seed(time.Now().UnixNano()) // Or each time we restart server the sequences would repeat
	for _, port := range PortsToTest {
		var seqNumber uint64 = uint64(rand.Uint32())
		var dst = fmt.Sprintf("%s:%d", clientIP, port)
		ticker := time.NewTicker(TCPInterval)
		var tResult []float64
		for x := 0; x < TCPCounter; x++ {
			seqNumber++
			select {
			case <-ticker.C:
				tResult = append(tResult, pingTcp(dst, seqNumber, TCPTimeout))
			}
		}
		ticker.Stop()
		min, avg, max, stddev := getStats(tResult)
		tcpResultArr = append(tcpResultArr, tcpResult{dst, tResult, min, avg, max, stddev})
	}

	// Combine all results
	results := Results{
		UUID:     expUUID,
		IPaddr:   clientIP,
		IcmpPing: icmp,
		TcpPing:  tcpResultArr}
	jsObj, err := json.Marshal(results)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resultString := string(jsObj)
	InfoLogger.Println(resultString)
	WebTemplate.Execute(w, results)
}

func main() {
	var logfilePath string
	var errlogPath string
	flag.StringVar(&logfilePath, "logfile", "logFile.jsonl", "Path to log file")
	flag.StringVar(&errlogPath, "errlog", "errlog.txt", "Path to err log file")
	flag.Parse()
	file, err := os.OpenFile(logfilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		log.Fatal(err)
	}
	errFile, err := os.OpenFile(errlogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		log.Fatal(err)
	}

	InfoLogger = log.New(file, "", 0)
	ErrLogger = log.New(errFile, "", log.Ldate|log.Ltime)
	certPath := "/etc/letsencrypt/live/test.reethika.info/"
	fullChain := path.Join(certPath, "fullchain.pem")
	privKey := path.Join(certPath, "privkey.pem")
	http.HandleFunc("/ping", pingHandler)
	http.HandleFunc("/echo", echoHandler)
	http.ListenAndServeTLS(":443", fullChain, privKey, nil)
}
