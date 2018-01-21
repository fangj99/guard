package main

/*
guard is a high performance circuit breaker written in Go.

workflow:

1. register URL patterns to router
2. find if router exist by HTTP `Host` field, if not found, return 404
3. request -> query router
            \
             -> (handler not exist?) -> return 404
             -> (handler exist but method not allowed?) -> return 405
             -> (handler exist)
                                \
                                 -> query timeline, circuit breaker not open yet? -> proxy and return, then save the response status
                                 -> circuot breaker is open? return 429 too many requests
*/

import (
	"encoding/json"
	"flag"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

var (
	// global variable
	breaker    = NewBreaker()
	adminPort  = flag.String("adminPort", ":12345", "admin listen at")
	proxyPort  = flag.String("proxyPort", ":23456", "proxy listen at")
	configPath = flag.String("configPath", "", "config file path")
	child      = flag.Bool("child", false, "is child process")
)

// APP registration infomation
type APP struct {
	Name     string    `json:"name"`
	URLs     []string  `json:"urls"`
	Methods  []string  `json:"methods"`
	Backends []Backend `json:"backends"`
}

func fakeProxyHandler(w http.ResponseWriter, r *http.Request, _ Params) {}

func overrideAPP(breaker *Breaker, app APP) {
	breaker.UpdateAPP(app.Name)
	router := breaker.routers[app.Name]

	for i, url := range app.URLs {
		router.Handle(strings.ToUpper(app.Methods[i]), url, fakeProxyHandler)
	}
	breaker.balancers[app.Name] = NewWRR(app.Backends...)
}

func loadConfiguration() {
	var app APP
	var err error

	if *configPath == "" {
		log.Panicf("configPath not set! got: %s", *configPath)
	}
	log.Printf("gonna using config: %s", *configPath)

	body, err := ioutil.ReadFile(*configPath)
	if err != nil {
		log.Panicf("failed to read config file: %s", err)
	}
	if err = json.Unmarshal(body, &app); err != nil {
		log.Panicf("failed to load config : %s", err)
	}
	if len(app.Methods) != len(app.URLs) {
		log.Panicf("failed: methods and urls should have same length and 1:1")
	}

	log.Printf("gonna insert or over write app %s's configuration", app.Name)
	overrideAPP(breaker, app)
}

func inspectAPPHandler(w http.ResponseWriter, r *http.Request, ps Params) {
	type jsonObject map[string]interface{}

	app := ps.ByName("app")
	jsonBody := jsonObject{}
	jsonBytes := []byte{}
	tl := breaker.timelines[app]
	if tl == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	tl.lock.RLock()
	defer tl.lock.RUnlock()

	cursor := tl.head
	jsonBody["app"] = app
	counters := []Counter{}

	for cursor != nil {
		counters = append(counters, cursor.counter)
		cursor = cursor.next
	}

	jsonBody["counters"] = counters

	jsonBytes, _ = json.Marshal(jsonBody)
	w.Header().Set("Content-Type", "application/json")
	w.Write(jsonBytes)
}

func proxy(proxyLn net.Listener) {
	log.Fatal(http.Serve(proxyLn, breaker))
}

// https://jiajunhuang.com/articles/2017_10_25-golang_graceful_restart.md.html
func forkAndRun(proxyLn net.Listener, adminLn net.Listener, cpuSeq int) {
	pl := proxyLn.(*net.TCPListener)
	plFile, _ := pl.File()
	al := adminLn.(*net.TCPListener)
	alFile, _ := al.File()

	cmd := exec.Command(
		"taskset",
		"-c",
		strconv.Itoa(cpuSeq),
		os.Args[0],
		"-adminPort="+*adminPort,
		"-proxyPort="+*proxyPort,
		"-child=true",
		"-configPath="+*configPath,
	)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{plFile, alFile}
	cmd.Run()
}

func main() {
	flag.Parse()

	loadConfiguration()

	var proxyLn, adminLn net.Listener
	var err error

	if !*child {
		proxyLn, err = net.Listen("tcp", *proxyPort)
		if err != nil {
			log.Panicf("failed to listen for proxy: %s", err)
		}
		adminLn, err = net.Listen("tcp", *adminPort)
		if err != nil {
			log.Panicf("failed to listen for admin: %s", err)
		}

		for i := 0; i < runtime.NumCPU(); i++ {
			go forkAndRun(proxyLn, adminLn, i)
		}
	} else {
		proxyLn, err = net.FileListener(os.NewFile(3, "proxy fd"))
		if err != nil {
			log.Panicf("failed to listen for proxy: %s", err)
		}
		adminLn, err = net.FileListener(os.NewFile(4, "admin fd"))
		if err != nil {
			log.Panicf("failed to listen for admin: %s", err)
		}
	}

	runtime.GOMAXPROCS(2)

	router := NewRouter()
	router.GET("/inspect/:app", inspectAPPHandler)

	go proxy(proxyLn)
	log.Fatal(http.Serve(adminLn, router))
}
