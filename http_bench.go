package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	_ "net/http/pprof"
	gourl "net/url"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	_ "embed"

	"github.com/gorilla/websocket"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"
)

//go:embed index.html
var dashboardHtml string

// ========================= function begin =========================
// template functions
func intSum(v ...int64) int64 {
	var r int64
	for _, r1 := range v {
		r += int64(r1)
	}
	return r
}

func random(min, max int64) int64 {
	rand.Seed(time.Now().UnixNano())
	return rand.Int63n(max-min) + min
}

func formatTime(now time.Time, fmt string) string {
	switch fmt {
	case "YMD":
		return now.Format("20060201")
	case "HMS":
		return now.Format("150405")
	default:
		return now.Format("20060201-150405")
	}
}

// YMD = yyyyMMdd, HMS = HHmmss, YMDHMS = yyyyMMdd-HHmmss
func date(fmt string) string {
	return formatTime(time.Now(), fmt)
}

func randomDate(fmt string) string {
	return formatTime(time.Unix(rand.Int63n(time.Now().Unix()-94608000)+94608000, 0), fmt)
}

func escape(u string) string {
	return gourl.QueryEscape(u)
}

const (
	letterIdxBits  = 6                    // 6 bits to represent a letter index
	letterIdxMask  = 1<<letterIdxBits - 1 // All 1-bits, as many as letterIdxBits
	letterIdxMax   = 63 / letterIdxBits   // # of letter indices fitting in 63 bits
	letterBytes    = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	letterNumBytes = "0123456789"
)

var (
	fnSrc = rand.NewSource(time.Now().UnixNano()) // for functions
	fnMap = template.FuncMap{
		"intSum":       intSum,
		"random":       random,
		"randomDate":   randomDate,
		"randomString": randomString,
		"randomNum":    randomNum,
		"date":         date,
		"UUID":         UUID,
		"escape":       escape,
		"getEnv":       getEnv,
	}
	fnUUID = randomString(10)

	ErrInitWsClient   = errors.New("init ws client error")
	ErrInitHttpClient = errors.New("init http client error")
	ErrUrl            = errors.New("check url error")
)

func randomN(n int, letter string) string {
	b := make([]byte, n)
	for i, cache, remain := n-1, fnSrc.Int63(), letterIdxMax; i >= 0; {
		if remain == 0 {
			cache, remain = fnSrc.Int63(), letterIdxMax
		}
		if idx := int(cache & letterIdxMask); idx < len(letter) {
			b[i] = letter[idx]
			i--
		}
		cache >>= letterIdxBits
		remain--
	}
	return string(b)
}

func randomString(n int) string {
	return randomN(n, letterBytes)
}

func randomNum(n int) string {
	return randomN(n, letterNumBytes)
}

func UUID() string {
	return fnUUID
}

func getEnv(key string) string {
	return os.Getenv(key)
}

// ========================= function end =========================

const (
	kCmdStart int = iota
	kCmdStop
	kCmdMetrics
	kScaleNum = 10000

	kTypeHttp1 = "http1"
	kTypeHttp2 = "http2"
	kTypeHttp3 = "http3"
	kTypeWs    = "ws"
	kTypeGrpc  = "grpc" // TODO: next version to support
	kIntMax    = int(^uint(0) >> 1)
	kIntMin    = ^kIntMax

	V_TRACE = 0
	V_DEBUG = 1
	V_INFO  = 2
	V_ERROR = 3
)

var resultRdMutex sync.RWMutex

type flagSlice []string

func (h *flagSlice) String() string {
	return fmt.Sprintf("%s", *h)
}

func (h *flagSlice) Set(value string) error {
	*h = append(*h, value)
	return nil
}

type StressResult struct {
	ErrCode  int    `json:"err_code"`
	ErrMsg   string `json:"err_msg"`
	AvgTotal int64  `json:"avg_total"`
	Fastest  int64  `json:"fastest"`
	Slowest  int64  `json:"slowest"`
	Average  int64  `json:"average"`
	Rps      int64  `json:"rps"`

	ErrorDist      map[string]int   `json:"error_dist"`
	StatusCodeDist map[int]int      `json:"status_code_dist"`
	Lats           map[string]int64 `json:"lats"`
	LatsTotal      int64            `json:"lats_total"`
	SizeTotal      int64            `json:"size_total"`
	Duration       int64            `json:"duration"`
	Output         string           `json:"output"`
}

func (result *StressResult) print() {
	resultRdMutex.RLock()
	defer resultRdMutex.RUnlock()
	switch result.Output {
	case "csv":
		fmt.Printf("Duration,Count\n")
		for duration, val := range result.Lats {
			fmt.Printf("%s,%d", duration, val/kScaleNum)
		}
		return
	default:
		// pass
	}
	if len(result.Lats) > 0 {
		fmt.Printf("Summary:\n")
		fmt.Printf("  Total:\t%4.3f secs\n", float32(result.Duration)/kScaleNum)
		fmt.Printf("  Slowest:\t%4.3f secs\n", float32(result.Slowest)/kScaleNum)
		fmt.Printf("  Fastest:\t%4.3f secs\n", float32(result.Fastest)/kScaleNum)
		fmt.Printf("  Average:\t%4.3f secs\n", float32(result.Average)/kScaleNum)
		fmt.Printf("  Requests/sec:\t%4.3f\n", float32(result.Rps)/kScaleNum)
		if result.SizeTotal > 1073741824 {
			fmt.Printf("  Total data:\t%4.3f GB\n", float64(result.SizeTotal)/1073741824)
		} else if result.SizeTotal > 1048576 {
			fmt.Printf("  Total data:\t%4.3f MB\n", float64(result.SizeTotal)/1048576)
		} else if result.SizeTotal > 1024 {
			fmt.Printf("  Total data:\t%4.3f KB\n", float64(result.SizeTotal)/1024)
		} else if result.SizeTotal > 0 {
			fmt.Printf("  Total data:\t%4.3f bytes\n", float64(result.SizeTotal))
		}
		fmt.Printf("  Size/request:\t%d bytes\n", result.SizeTotal/result.LatsTotal)
		result.printStatusCodes()
		result.printLatencies()
	}
	if len(result.ErrorDist) > 0 {
		result.printErrors()
	}
}

// Print latency distribution.
func (result *StressResult) printLatencies() {
	pctls := []int{10, 25, 50, 75, 90, 95, 99}
	data := make([]string, len(pctls))
	durationLats := make([]string, 0)
	for duration := range result.Lats {
		durationLats = append(durationLats, duration)
	}
	sort.Strings(durationLats)
	var j int = 0
	var current int64 = 0
	for i := 0; i < len(durationLats) && j < len(pctls); i++ {
		current = current + result.Lats[durationLats[i]]
		if int(current*100/result.LatsTotal) >= pctls[j] {
			data[j] = durationLats[i]
			j++
		}
	}
	fmt.Printf("\nLatency distribution:\n")
	for i := 0; i < len(pctls); i++ {
		fmt.Printf("  %v%% in %s secs\n", pctls[i], data[i])
	}
}

// Print status code distribution.
func (result *StressResult) printStatusCodes() {
	fmt.Printf("\nStatus code distribution:\n")
	for code, num := range result.StatusCodeDist {
		fmt.Printf("  [%d]\t%d responses\n", code, num)
	}
}

func (result *StressResult) printErrors() {
	fmt.Printf("\nError distribution:\n")
	for err, num := range result.ErrorDist {
		fmt.Printf("  [%d]\t%s", num, err)
	}
}

func (result *StressResult) marshal() ([]byte, error) {
	resultRdMutex.RLock()
	defer resultRdMutex.RUnlock()
	return json.Marshal(result)
}

func (result *StressResult) result(res *result) {
	resultRdMutex.Lock()
	defer resultRdMutex.Unlock()

	if res.err != nil {
		result.ErrorDist[res.err.Error()]++
	} else {
		result.Lats[fmt.Sprintf("%4.3f", res.duration.Seconds())]++
		duration := int64(res.duration.Seconds() * kScaleNum)
		result.LatsTotal++
		if result.Slowest < duration {
			result.Slowest = duration
		}
		if result.Fastest > duration {
			result.Fastest = duration
		}
		result.AvgTotal += duration
		result.StatusCodeDist[res.statusCode]++
		if res.contentLength > 0 {
			result.SizeTotal += res.contentLength
		}
	}
}

func (result *StressResult) combine(resultList ...StressResult) {
	resultRdMutex.RLock()
	defer resultRdMutex.RUnlock()

	for _, v := range resultList {
		if result.Slowest < v.Slowest {
			result.Slowest = v.Slowest
		}
		if result.Fastest > v.Fastest {
			result.Fastest = v.Fastest
		}
		result.LatsTotal += v.LatsTotal
		result.AvgTotal += v.AvgTotal
		for code, c := range v.StatusCodeDist {
			result.StatusCodeDist[code] += c
		}
		result.SizeTotal += v.SizeTotal
		for code, c := range v.ErrorDist {
			result.ErrorDist[code] += c
		}
		for lats, c := range v.Lats {
			result.Lats[lats] += c
		}
	}

	if result.Duration > 0 {
		result.Rps = int64((result.LatsTotal * kScaleNum * kScaleNum) / result.Duration)
	}

	if result.LatsTotal > 0 {
		result.Average = result.AvgTotal / result.LatsTotal
	}
}

type StressParameters struct {
	SequenceId         int64               `json:"sequence_id"`         // Sequence
	Cmd                int                 `json:"cmd"`                 // Commands
	RequestMethod      string              `json:"request_method"`      // Request Method.
	RequestBody        string              `json:"request_body"`        // Request Body.
	RequestScriptBody  string              `json:"request_script_body"` // Request Script Body.
	RequestHttpType    string              `json:"request_httptype"`    // Request HTTP Type
	N                  int                 `json:"n"`                   // N is the total number of requests to make.
	C                  int                 `json:"c"`                   // C is the concurrency level, the number of concurrent workers to run.
	Duration           int64               `json:"duration"`            // D is the duration for stress test
	Timeout            int                 `json:"timeout"`             // Timeout in ms.
	Qps                int                 `json:"qps"`                 // Qps is the rate limit.
	DisableCompression bool                `json:"disable_compression"` // DisableCompression is an option to disable compression in response
	DisableKeepAlives  bool                `json:"disable_keepalives"`  // DisableKeepAlives is an option to prevents re-use of TCP connections between different HTTP requests
	AuthUsername       string              `json:"auth_username"`       // Basic authentication, username:password.
	AuthPassword       string              `json:"auth_password"`
	Headers            map[string][]string `json:"headers"` // Custom HTTP header.
	Url                string              `json:"url"`     // Request url.
	Output             string              `json:"output"`  // Output represents the output type. If "csv" is provided, the output will be dumped as a csv stream.
}

func (p *StressParameters) String() string {
	if body, err := json.MarshalIndent(p, "", "\t"); err != nil {
		return err.Error()
	} else {
		return string(body)
	}
}

type (
	result struct {
		err           error
		statusCode    int
		duration      time.Duration
		contentLength int64
	}

	StressWorker struct {
		RequestParams             *StressParameters
		results                   chan *result
		resultList                []StressResult
		currentResult             StressResult
		totalTime                 time.Duration
		wg                        sync.WaitGroup // Wait some task finish
		err                       error
		bodyTemplate, urlTemplate *template.Template
	}
)

func (b *StressWorker) Start() {
	b.results = make(chan *result, 2*b.RequestParams.C+1)
	b.resultList = make([]StressResult, 0)
	b.collectReport()
	b.runWorkers()
	verbosePrint(V_INFO, "worker finished and wait result")
}

// Stop stop stress worker and wait coroutine finish
func (b *StressWorker) Stop(wait bool, err error) {
	b.RequestParams.Cmd = kCmdStop
	if err != nil {
		b.err = err
	}
	if wait {
		b.wg.Wait()
	}
}

func (b *StressWorker) IsStop() bool {
	return b.RequestParams.Cmd == kCmdStop
}

func (b *StressWorker) Append(result ...StressResult) {
	b.resultList = append(b.resultList, result...)
}

func (b *StressWorker) Wait() *StressResult {
	b.wg.Wait()
	if len(b.resultList) <= 0 {
		fmt.Fprintf(os.Stderr, "internal err: stress test result empty\n")
		return nil
	}
	b.resultList[0].combine(b.resultList[1:]...)
	verbosePrint(V_DEBUG, "result length = %d", len(b.resultList))
	return &(b.resultList[0])
}

func (b *StressWorker) runWorker(n, sleep int, client *StressClient) {
	var runCounts int = 0
	// random set seed
	rand.Seed(time.Now().UnixNano())
	for !b.IsStop() {
		if n > 0 && runCounts > n {
			break
		}

		runCounts++
		if sleep > 0 {
			time.Sleep(time.Duration(sleep) * time.Microsecond)
		}

		var t = time.Now()
		if code, size, err := b.doClient(client); err != nil {
			verbosePrint(V_ERROR, "err: %v", err)
			b.Stop(false, err)
			break
		} else {
			b.results <- &result{
				statusCode:    code,
				duration:      time.Now().Sub(t),
				err:           err,
				contentLength: size,
			}
		}
	}
}

func (b *StressWorker) runWorkers() {
	fmt.Printf("running %d connections, @ %s\n", b.RequestParams.C, b.RequestParams.Url)
	var (
		wg               sync.WaitGroup
		err              error
		start            = time.Now()
		bodyTemplateName = fmt.Sprintf("BODY-%d", b.RequestParams.SequenceId)
		urlTemplateName  = fmt.Sprintf("URL-%d", b.RequestParams.SequenceId)
	)

	if b.urlTemplate, err = template.New(urlTemplateName).Funcs(fnMap).Parse(b.RequestParams.Url); err != nil {
		verbosePrint(V_ERROR, "parse urls function err: "+err.Error()+"")
	}
	if b.bodyTemplate, err = template.New(bodyTemplateName).Funcs(fnMap).Parse(b.RequestParams.RequestBody); err != nil {
		verbosePrint(V_ERROR, "parse request body function err: "+err.Error()+"")
	}

	// ignore the case where b.RequestParams.N % b.RequestParams.C != 0.
	for i := 0; i < b.RequestParams.C && !(b.IsStop()); i++ {
		wg.Add(1)
		go func() {
			client := b.getClient()

			defer func() {
				b.closeClient(client)
				wg.Done()
				if r := recover(); r != nil {
					fmt.Fprintf(os.Stderr, "internal err: %v\n", r)
				}
			}()

			if client != nil {
				sleep := 0
				if b.RequestParams.Qps > 0 {
					sleep = 1e6 / (b.RequestParams.Qps * b.RequestParams.C) // sleep XXus send request
				}
				b.runWorker(b.RequestParams.N/b.RequestParams.C, sleep, client)
			}
		}()
	}

	wg.Wait()
	b.Stop(false, nil)
	b.totalTime = time.Now().Sub(start)
	close(b.results)
}

func (b *StressWorker) getClient() *StressClient {
	client := &StressClient{}
	switch b.RequestParams.RequestHttpType {
	case kTypeHttp3:
		client.httpClient = &http.Client{
			Timeout: time.Duration(b.RequestParams.Timeout) * time.Millisecond,
			Transport: &http3.RoundTripper{
				TLSClientConfig: &tls.Config{
					RootCAs:            http3Pool,
					InsecureSkipVerify: true,
				},
			},
		}
	case kTypeHttp2:
		client.httpClient = &http.Client{
			Timeout: time.Duration(b.RequestParams.Timeout) * time.Millisecond,
			Transport: &http2.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true,
				},
				DisableCompression: b.RequestParams.DisableCompression,
			},
		}
	case kTypeHttp1:
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
			DisableCompression:  b.RequestParams.DisableCompression,
			DisableKeepAlives:   b.RequestParams.DisableKeepAlives,
			TLSHandshakeTimeout: time.Duration(b.RequestParams.Timeout) * time.Millisecond,
			TLSNextProto:        make(map[string]func(string, *tls.Conn) http.RoundTripper),
			DialContext: (&net.Dialer{
				Timeout:   time.Duration(b.RequestParams.Timeout) * time.Second,
				KeepAlive: time.Duration(60) * time.Second,
			}).DialContext,
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 10,
			MaxConnsPerHost:     10,
			IdleConnTimeout:     time.Duration(90) * time.Second,
		}
		if proxyUrl != nil {
			tr.Proxy = http.ProxyURL(proxyUrl)
		}
		client.httpClient = &http.Client{
			Timeout:   time.Duration(b.RequestParams.Timeout) * time.Millisecond,
			Transport: tr,
		}
	case kTypeWs:
		if c, _, err := websocket.DefaultDialer.Dial(b.RequestParams.Url, b.RequestParams.Headers); err != nil {
			verbosePrint(V_ERROR, "websocket err: %s", err.Error())
			return nil
		} else {
			client.wsClient = c
		}
	}

	return client
}

func (b *StressWorker) doClient(client *StressClient) (code int, size int64, err error) {
	var urlBytes, bodyBytes bytes.Buffer
	var url = b.RequestParams.Url

	if b.urlTemplate != nil && len(url) > 0 {
		b.urlTemplate.Execute(&urlBytes, nil)
	} else {
		urlBytes.WriteString(url)
	}

	if len(b.RequestParams.RequestBody) > 0 && b.bodyTemplate != nil {
		b.bodyTemplate.Execute(&bodyBytes, nil)
	} else {
		bodyBytes.WriteString(b.RequestParams.RequestBody)
	}

	if !checkURL(urlBytes.String()) {
		err = ErrUrl
		return
	}

	verbosePrint(V_TRACE, "request url: %s", urlBytes.String())
	verbosePrint(V_TRACE, "request body: %s", bodyBytes.String())

	switch b.RequestParams.RequestHttpType {
	case kTypeHttp1, kTypeHttp2, kTypeHttp3:
		if client.httpClient == nil {
			err = ErrInitHttpClient
			return
		}
		req, reqErr := http.NewRequest(b.RequestParams.RequestMethod, urlBytes.String(), strings.NewReader(bodyBytes.String()))
		if reqErr != nil || req == nil {
			err = errors.New("request err: " + err.Error())
			return
		}
		req.Header = b.RequestParams.Headers
		resp, respErr := client.httpClient.Do(req)
		err = respErr
		if respErr == nil {
			size = resp.ContentLength
			code = resp.StatusCode
			defer resp.Body.Close()
			if n, _ := fastRead(resp.Body); size <= 0 {
				size = n
			}
		}
	case kTypeWs:
		if client.wsClient == nil {
			err = ErrInitWsClient
			return
		}
		if err = client.wsClient.WriteMessage(websocket.TextMessage, bodyBytes.Bytes()); err != nil {
			return
		}
		if _, message, readErr := client.wsClient.ReadMessage(); readErr != nil {
			err = readErr
			return
		} else {
			size = int64(len(message))
			code = http.StatusOK
		}
	default:
		// pass
	}

	return
}

func (b *StressWorker) closeClient(client *StressClient) {
	switch b.RequestParams.RequestHttpType {
	case kTypeHttp1, kTypeHttp2, kTypeHttp3:
		if client.httpClient != nil {
			client.httpClient.CloseIdleConnections()
		}
	case kTypeWs:
		if client.wsClient != nil {
			client.wsClient.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		}
	default:
		// TODO: add http3
	}
}

type StressClient struct {
	httpClient *http.Client
	wsClient   *websocket.Conn
}

func (b *StressWorker) collectReport() {
	b.wg.Add(1)

	go func() {
		timeTicker := time.NewTicker(time.Duration(b.RequestParams.Duration) * time.Second)
		defer func() {
			timeTicker.Stop()
			b.wg.Done()
		}()
		b.currentResult = StressResult{
			ErrorDist:      make(map[string]int, 0),
			StatusCodeDist: make(map[int]int, 0),
			Lats:           make(map[string]int64, 0),
			Slowest:        int64(kIntMin),
			Fastest:        int64(kIntMax),
		}
		for {
			select {
			case res, ok := <-b.results:
				if !ok {
					b.currentResult.Duration = int64(b.totalTime.Seconds() * kScaleNum)
					b.resultList = append(b.resultList, b.currentResult)
					return
				}
				b.currentResult.result(res)
			case <-timeTicker.C:
				verbosePrint(V_INFO, "time ticker upcoming, duration: %ds", b.RequestParams.Duration)
				b.Stop(false, nil) // Time ticker exec Stop commands
			}
		}
	}()
}

func usageAndExit(msg string) {
	if msg != "" {
		fmt.Fprintf(os.Stderr, msg+"\n")
	}
	flag.Usage()
	fmt.Fprintf(os.Stderr, "\n")
	os.Exit(1)
}

func fastRead(r io.Reader) (int64, error) {
	n := int64(0)
	b := make([]byte, 0, 512)
	for {
		if bsize, err := r.Read(b[0:cap(b)]); err != nil {
			if err == io.EOF {
				err = nil
			}
			return n, err
		} else {
			n += int64(bsize)
		}
	}
}

func parseInputWithRegexp(input, regx string) ([]string, error) {
	re := regexp.MustCompile(regx)
	matches := re.FindStringSubmatch(input)
	if len(matches) < 1 {
		return nil, fmt.Errorf("could not parse the provided input; input = %v", input)
	}
	return matches, nil
}

func checkURL(url string) bool {
	if _, err := gourl.ParseRequestURI(url); err != nil {
		fmt.Fprintln(os.Stderr, "parse URL err: ", err.Error())
		return false
	}
	return true
}

func parseFile(fileName string, delimiter []rune) ([]string, error) {
	var contentList []string
	file, err := os.Open(fileName)
	if err != nil {
		return contentList, err
	}

	defer file.Close()

	if content, err := ioutil.ReadAll(file); err != nil {
		return contentList, err
	} else {
		if delimiter == nil {
			return []string{string(content)}, nil
		}
		lines := strings.FieldsFunc(string(content), func(r rune) bool {
			for _, v := range delimiter {
				if r == v {
					return true
				}
			}
			return false
		})
		for _, line := range lines {
			if len(line) > 0 {
				contentList = append(contentList, line)
			}
		}
	}
	return contentList, nil
}

func verbosePrint(level int, vfmt string, args ...interface{}) {
	if *verbose > level {
		return
	}
	switch level {
	case V_TRACE:
		fmt.Printf("[VERBOSE TRACE] "+vfmt+"\n", args...)
	case V_DEBUG:
		fmt.Printf("[VERBOSE DEBUG] "+vfmt+"\n", args...)
	case V_INFO:
		fmt.Printf("[VERBOSE INFO] "+vfmt+"\n", args...)
	default:
		fmt.Printf("[VERBOSE ERROR] "+vfmt+"\n", args...)
	}
}

func parseTime(timeStr string) int64 {
	var multi int64 = 1
	if timeStrLen := len(timeStr) - 1; timeStrLen > 0 {
		switch timeStr[timeStrLen] {
		case 's':
			timeStr = timeStr[:timeStrLen]
		case 'm':
			timeStr = timeStr[:timeStrLen]
			multi = 60
		case 'h':
			timeStr = timeStr[:timeStrLen]
			multi = 3600
		}
	}
	t, err := strconv.ParseInt(timeStr, 10, 64)
	if err != nil || t <= 0 {
		usageAndExit("Duration parse err: " + err.Error())
	}
	return multi * t
}

func runStress(params StressParameters, stressTestPtr **StressWorker) *StressResult {
	var stressResult *StressResult
	var stressTest *StressWorker
	if v, ok := stressList.Load(params.SequenceId); ok && v != nil {
		stressTest = v.(*StressWorker)
	} else {
		stressTest = &StressWorker{RequestParams: &params}
		stressList.Store(params.SequenceId, stressTest)
	}
	*stressTestPtr = stressTest
	switch params.Cmd {
	case kCmdStart:
		if len(workerList) > 0 {
			jsonBody, _ := json.Marshal(params)
			resultList := requestWorkerList(jsonBody, stressTest)
			stressTest.Append(resultList...)
		} else {
			stressTest.Start()
		}
		stressResult = stressTest.Wait()
		if stressResult != nil {
			stressResult.print()
		}
		stressList.Delete(params.SequenceId)
	case kCmdStop:
		if len(workerList) > 0 {
			jsonBody, _ := json.Marshal(params)
			requestWorkerList(jsonBody, stressTest)
		}
		stressTest.Stop(true, nil)
		stressList.Delete(params.SequenceId)
	case kCmdMetrics:
		if len(workerList) > 0 {
			jsonBody, _ := json.Marshal(params)
			if resultList := requestWorkerList(jsonBody, stressTest); len(resultList) > 0 {
				stressResult = &StressResult{}
				for i := 0; i < len(resultList); i++ {
					stressResult.LatsTotal += resultList[i].LatsTotal
				} // TODO: assign other variable
			}
		} else {
			stressResult = &stressTest.currentResult
		}
	}
	if stressTest.err != nil {
		stressResult.ErrCode = -1
		stressResult.ErrMsg = stressTest.err.Error()
	}
	return stressResult
}

func handleWorker(w http.ResponseWriter, r *http.Request) {
	if reqStr, err := ioutil.ReadAll(r.Body); err == nil {
		var params StressParameters
		var result *StressResult
		if err := json.Unmarshal(reqStr, &params); err != nil {
			fmt.Fprintf(os.Stderr, "unmarshal body err: %s\n", err.Error())
			result = &StressResult{
				ErrCode: -1,
				ErrMsg:  err.Error(),
			}
		} else {
			verbosePrint(V_DEBUG, "request params: %s", params.String())
			var stressWorker *StressWorker
			result = runStress(params, &stressWorker)
		}
		if result != nil {
			if wbody, err := result.marshal(); err != nil {
				verbosePrint(V_ERROR, "marshal result: %v", err)
			} else {
				w.Write(wbody)
			}
		}
	}
}

func requestWorker(uri string, body []byte) (*StressResult, error) {
	verbosePrint(V_DEBUG, "Request body: %s", string(body))
	resp, err := http.Post(uri, "application/json", bytes.NewBuffer(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "RequestWorker addr(%s), err: %s\n", uri, err.Error())
		return nil, err
	}
	defer resp.Body.Close()
	var result StressResult
	respStr, _ := ioutil.ReadAll(resp.Body)
	err = json.Unmarshal(respStr, &result)
	return &result, err
}

var (
	stressList sync.Map
	workerList flagSlice // Worker mechine addr list.

	headerRegexp = `^([\w-]+):\s*(.+)`
	authRegexp   = `^(.+):([^\s].+)`

	proxyUrl   *gourl.URL
	stopSignal chan os.Signal

	m          = flag.String("m", "GET", "")
	body       = flag.String("body", "", "")
	authHeader = flag.String("a", "", "")

	output = flag.String("o", "", "") // Output type

	c            = flag.Int("c", 50, "")               // Number of requests to run concurrently
	n            = flag.Int("n", 0, "")                // Number of requests to run
	q            = flag.Int("q", 0, "")                // Rate limit, in seconds (QPS)
	d            = flag.String("d", "10s", "")         // Duration for stress test
	t            = flag.Int("t", 3000, "")             // Timeout in ms
	httpType     = flag.String("http", kTypeHttp1, "") // HTTP Version
	printExample = flag.Bool("example", false, "")

	cpus = flag.Int("cpus", runtime.GOMAXPROCS(-1), "")

	disableCompression = flag.Bool("disable-compression", false, "")
	disableKeepAlives  = flag.Bool("disable-keepalive", false, "")
	proxyAddr          = flag.String("x", "", "")

	urlstr    = flag.String("url", "", "")
	verbose   = flag.Int("verbose", 3, "")
	listen    = flag.String("listen", "", "")
	dashboard = flag.String("dashboard", "", "")

	urlFile           = flag.String("url-file", "", "")
	bodyFile          = flag.String("body-file", "", "")
	scriptFile        = flag.String("script", "", "")
	requestWorkerList = func(paramsJson []byte, stressTest *StressWorker) []StressResult {
		var wg sync.WaitGroup
		var stressResult []StressResult
		for _, v := range workerList {
			wg.Add(1)
			go func(workerAddr string) {
				defer wg.Done()
				if result, err := requestWorker("http://"+workerAddr+"/", paramsJson); err == nil {
					stressResult = append(stressResult, *result)
				}
			}(v)
		}
		wg.Wait()
		return stressResult
	}

	http3Pool *x509.CertPool
)

const (
	usage = `Usage: http_bench [options...] <url>
Options:
	-n  Number of requests to run.
	-c  Number of requests to run concurrently. Total number of requests cannot
		be smaller than the concurency level.
	-q  Rate limit, in seconds (QPS).
	-d  Duration of the stress test, e.g. 2s, 2m, 2h
	-t  Timeout in ms.
	-o  Output type. If none provided, a summary is printed.
		"csv" is the only supported alternative. Dumps the response
		metrics in comma-seperated values format.
	-m  HTTP method, one of GET, POST, PUT, DELETE, HEAD, OPTIONS.
	-H  Custom HTTP header. You can specify as many as needed by repeating the flag.
		for example, -H "Accept: text/html" -H "Content-Type: application/xml", 
		but "Host: ***", replace that with -host.
	-http  Support http1, http2, ws, wss (default http1).
	-body  Request body, default empty.
	-a  Basic authentication, username:password.
	-x  HTTP Proxy address as host:port.
	-disable-compression  Disable compression.
	-disable-keepalive    Disable keep-alive, prevents re-use of TCP connections between different HTTP requests.
	-cpus		Number of used cpu cores. (default for current machine is %d cores).
	-url		Request single url.
	-verbose 	Print detail logs, default 3(0:TRACE, 1:DEBUG, 2:INFO, 3:ERROR).
	-url-file 	Read url list from file and random stress test.
	-body-file	Request body from file.
	-listen 	Listen IP:PORT for distributed stress test and worker mechine (default empty). e.g. "127.0.0.1:12710".
	-dashboard 	Listen dashboard IP:PORT and operate stress params on browser.
	-W			Running distributed stress test worker mechine list. for example, -W "127.0.0.1:12710" -W "127.0.0.1:12711".
	-example 	Print some stress test examples (default false).
`

	examples = `
1.Example stress test:
	./http_bench -n 1000 -c 10 -t 3000 -m GET -url "http://127.0.0.1/test1"
	./http_bench -n 1000 -c 10 -t 3000 -m GET "http://127.0.0.1/test1"
	./http_bench -n 1000 -c 10 -t 3000 -m GET "http://127.0.0.1/test1" -url-file urls.txt
	./http_bench -d 10s -c 10 -m POST -body "{}" -url-file urls.txt

2.Example http2 test:
	./http_bench -d 10s -c 10 -http http2 -m POST "http://127.0.0.1/test1" -body "{}"

3.Example http3 test:
	./http_bench -d 10s -c 10 -http http3 -m POST "http://127.0.0.1/test1" -body "{}"

4.Example dashboard test:
	./http_bench -dashboard "127.0.0.1:12345" -verbose 1

5.Example support function and variable test:
	./http_bench -c 1 -n 1 "https://127.0.0.1:18090?data={{ randomString 10}}" -verbose 0

6.Example distributed stress test:
	(1) ./http_bench -listen "127.0.0.1:12710" -verbose 1
	(2) ./http_bench -c 1 -d 10s "http://127.0.0.1:18090/test1" -body "{}" -verbose 1 -W "127.0.0.1:12710"
`
)

func main() {
	flag.Usage = func() {
		fmt.Println(fmt.Sprintf(usage, runtime.NumCPU()))
	}

	var params StressParameters
	var headerslice flagSlice

	flag.Var(&headerslice, "H", "") // Custom HTTP header
	flag.Var(&workerList, "W", "")  // Worker mechine
	flag.Parse()

	for flag.NArg() > 0 {
		if len(*urlstr) == 0 {
			*urlstr = flag.Args()[0]
		}
		os.Args = flag.Args()[0:]
		flag.Parse()
	}

	if *printExample {
		fmt.Println(examples)
		return
	}

	runtime.GOMAXPROCS(*cpus)
	params.N = *n
	params.C = *c
	params.Qps = *q
	params.Duration = parseTime(*d)

	if params.C <= 0 {
		usageAndExit("n and c cannot be smaller than 1.")
	}

	if (params.N < params.C) && (params.Duration < 0) {
		usageAndExit("n cannot be less than c.")
	}

	var requestUrls []string
	if *urlFile == "" && len(*urlstr) > 0 {
		requestUrls = append(requestUrls, *urlstr)
	} else if len(*urlFile) > 0 {
		var err error
		if requestUrls, err = parseFile(*urlFile, []rune{'\r', '\n'}); err != nil {
			usageAndExit(*urlFile + " file read error(" + err.Error() + ").")
		}
	}

	params.RequestMethod = strings.ToUpper(*m)
	params.DisableCompression = *disableCompression
	params.DisableKeepAlives = *disableKeepAlives
	params.RequestBody = *body

	if *bodyFile != "" {
		if readBody, err := parseFile(*bodyFile, nil); err != nil {
			usageAndExit(*bodyFile + " file read error(" + err.Error() + ").")
		} else {
			if len(readBody) > 0 {
				params.RequestBody = readBody[0]
			}
		}
	}

	if *scriptFile != "" {
		if scriptBody, err := parseFile(*scriptFile, nil); err != nil {
			usageAndExit(*scriptFile + " file read error(" + err.Error() + ").")
		} else {
			if len(scriptBody) > 0 {
				params.RequestScriptBody = scriptBody[0]
			}
		}
	}

	switch strings.ToLower(*httpType) {
	case kTypeHttp1, kTypeHttp2, kTypeWs:
		params.RequestHttpType = strings.ToLower(*httpType)
	case kTypeHttp3:
		params.RequestHttpType = strings.ToLower(*httpType)
		var err error
		if http3Pool, err = x509.SystemCertPool(); err != nil {
			panic(kTypeHttp3 + " err: " + err.Error())
		}
	default:
		usageAndExit("not support -http: " + *httpType)
	}

	// set any other additional repeatable headers
	for _, h := range headerslice {
		match, err := parseInputWithRegexp(h, headerRegexp)
		if err != nil {
			usageAndExit(err.Error())
		}
		if params.Headers == nil {
			params.Headers = make(map[string][]string, 0)
		}
		params.Headers[match[1]] = []string{match[2]}
	}

	// set basic auth if set
	if *authHeader != "" {
		if match, err := parseInputWithRegexp(*authHeader, authRegexp); err != nil {
			usageAndExit(err.Error())
		} else {
			params.AuthUsername, params.AuthPassword = match[1], match[2]
		}
	}

	if *output != "csv" && *output != "" {
		usageAndExit("invalid output type; only csv is supported.")
	}

	// set request timeout
	params.Timeout = *t

	if *proxyAddr != "" {
		var err error
		if proxyUrl, err = gourl.Parse(*proxyAddr); err != nil {
			usageAndExit(err.Error())
		}
	}

	var mainServer *http.Server
	_, mainCancel := context.WithCancel(context.Background())

	// decrease gc profile
	if getEnv("BENCH_GC") == "1" {
		debug.SetGCPercent(200)
	}

	if len(*listen) > 0 {
		mux := http.NewServeMux()
		mux.HandleFunc("/", handleWorker)
		fmt.Fprintf(os.Stdout, "worker listen %s\n", *listen)
		mainServer = &http.Server{
			Addr:    *listen,
			Handler: mux,
		}
		if err := mainServer.ListenAndServe(); err != nil {
			fmt.Fprintf(os.Stderr, "worker listen err: %s\n", err.Error())
		}
	} else if len(*dashboard) > 0 {
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(dashboardHtml)) // export dashboard index.html
		})
		mux.HandleFunc("/api", handleWorker)
		fmt.Fprintf(os.Stdout, "dashboard addr %s\n", *dashboard)
		mainServer = &http.Server{
			Addr:    *dashboard,
			Handler: mux,
		}
		if err := mainServer.ListenAndServe(); err != nil {
			fmt.Fprintf(os.Stderr, "dashboard listen err: %s\n", err.Error())
		}
	} else {
		if len(requestUrls) <= 0 {
			usageAndExit("url or url-file empty.")
		}

		for _, url := range requestUrls {
			params.Url = url
			params.SequenceId = time.Now().Unix()
			params.Cmd = kCmdStart
			verbosePrint(V_DEBUG, "request params: %s", params.String())
			stopSignal = make(chan os.Signal)
			signal.Notify(stopSignal, syscall.SIGINT, syscall.SIGTERM)

			var stressTest *StressWorker
			var stressResult *StressResult

			go func() {
				<-stopSignal
				verbosePrint(V_INFO, "recv stop signal")
				params.Cmd = kCmdStop
				jsonBody, _ := json.Marshal(params)
				requestWorkerList(jsonBody, stressTest)
				stressTest.Stop(true, nil) // Recv stop signal and Stop commands
				mainCancel()
			}()

			if stressResult = runStress(params, &stressTest); stressResult != nil {
				close(stopSignal)
				stressResult.print()
			}
		}
	}
}
