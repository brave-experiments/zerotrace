package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// serveFormTemplate serves the form
func serveFormTemplate(w http.ResponseWriter) {
	if err := measureTemplate.Execute(w, nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// measureHandler serves the form which collects user's contact data and ground-truth (VPN/Direct) before experiment begins
func measureHandler(w http.ResponseWriter, r *http.Request) {
	if checkHTTPParams(w, r, "/measure") {
		return
	}
	if r.Method == "GET" {
		serveFormTemplate(w)
	} else {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		details, err := validateForm(r.FormValue("email"), r.FormValue("exp_type"), r.FormValue("device"), r.FormValue("location_vpn"), r.FormValue("location_user"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		logAsJson(details)
		http.Redirect(w, r, "/ping?uuid="+details.UUID, 302)
	}
}

// indexHandler serves the default index page with reasons for scanning IPs on this server and point of contact
func indexHandler(w http.ResponseWriter, r *http.Request) {
	if checkHTTPParams(w, r, "/") {
		return
	}
	fmt.Fprint(w, indexPage)
}

// pingHandler for ICMP measurements which also serves the webpage via a template
func pingHandler(w http.ResponseWriter, r *http.Request) {
	if checkHTTPParams(w, r, "/ping") {
		return
	}
	var uuid string
	for k, v := range r.URL.Query() {
		if k == "uuid" && isValidUUID(v[0]) {
			uuid = v[0]
		} else {
			http.Error(w, "Invalid UUID", http.StatusInternalServerError)
			return
		}
	}

	clientIPstr := r.RemoteAddr
	clientIP, _, _ := net.SplitHostPort(clientIPstr)

	icmpResults, err := icmpPinger(clientIP)
	if err != nil {
		l.Println("ICMP Ping Error: ", err)
	}

	// Combine all results
	results := Results{
		UUID:   uuid,
		IPaddr: clientIP,
		//RFC3339 style UTC date time with added seconds information
		Timestamp:  time.Now().UTC().Format("2006-01-02T15:04:05.000000"),
		IcmpPing:   *icmpResults,
		MinIcmpRtt: icmpResults.MinRtt,
	}
	logAsJson(results)
	if err := pingTemplate.Execute(w, results); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// traceHandler speaks WebSocket for extracting underlying connection to use for 0trace
func traceHandler(w http.ResponseWriter, r *http.Request) {
	if checkHTTPParams(w, r, "/trace") {
		return
	}
	var uuid string
	for k, v := range r.URL.Query() {
		if k == "uuid" && isValidUUID(v[0]) {
			uuid = v[0]
		} else {
			http.Error(w, "Invalid UUID", http.StatusInternalServerError)
			return
		}
	}
	var upgrader = websocket.Upgrader{}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		l.Println("upgrade:", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer c.Close()
	myConn := c.UnderlyingConn()

	zeroTraceInstance := newZeroTrace(ifaceName, myConn, uuid)

	err = zeroTraceInstance.Run()
	if err != nil {
		l.Println("ZeroTrace Run Error: ", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// echoHandler for the echo webserver that speaks WebSocket
func echoHandler(w http.ResponseWriter, r *http.Request) {
	if checkHTTPParams(w, r, "/echo") {
		return
	}
	var upgrader = websocket.Upgrader{}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		l.Println("upgrade:", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return

	}
	defer c.Close()
	for {
		mt, message, err := c.ReadMessage()
		if err != nil {
			l.Println("read:", err)
			break
		}
		// ReadMessage() returns messageType int, p []byte, err error]
		var wsData map[string]interface{}
		if err := json.Unmarshal(message, &wsData); err != nil {
			l.Println("unmarshal:", err)
			break
		}
		if wsData["type"] != "ws-latency" {
			if wsUUID, ok := wsData["UUID"].(string); ok {
				// Only log the final message with all latencies calculated, and don't log other unsolicited echo messages
				if isValidUUID(string(wsUUID)) {
					l.Println(string(message))
				}
			}
		}
		err = c.WriteMessage(mt, message)
		if err != nil {
			l.Println("write:", err)
			break
		}
	}
}
