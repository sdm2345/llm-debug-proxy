package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/sashabaranov/go-openai"
	"gopkg.in/yaml.v3"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
)

var (
	port     int
	upstream string
	logDir   string
)

func init() {
	flag.IntVar(&port, "port", 8800, "Port to run the proxy server on")
	flag.StringVar(&logDir, "log-dir", "./logs", "Directory to save logs")
	flag.StringVar(&upstream, "upstream", "", "Upstream server URL")
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	flag.Parse()

	if upstream == "" {
		log.Fatal("upstream is required")
	}

	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.Printf("Failed to create log directory: %v", err)
		return
	}

	upstreamURL, err := url.Parse(upstream)
	if err != nil {
		log.Fatalf("Invalid upstream URL: %v", err)
	}

	proxy := httputil.NewSingleHostReverseProxy(upstreamURL)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Request: %s %s", r.Method, r.URL.Path)
		handleRequestAndLog(w, r, proxy)
	})

	address := fmt.Sprintf(":%d", port)
	log.Printf("Starting proxy server on %s, upstram:%s", address, upstreamURL.String())
	log.Fatal(http.ListenAndServe(address, nil))
}

// /openai/deployments/{model}/chat/completions?api-version=2024-02-15-preview
var reg = regexp.MustCompile(`^/openai/deployments/([^/]+)/chat/completions$`)

func handleRequestAndLog(w http.ResponseWriter, r *http.Request, proxy *httputil.ReverseProxy) {

	arr := reg.FindAllStringSubmatch(r.URL.Path, -1)
	if len(arr) > 0 {
		model := arr[0][1]
		log.Printf("Model: %s", model)
		logRequestResponse(w, r, proxy, model)
	} else {
		proxy.ServeHTTP(w, r)
	}

}

func logRequestResponse(w http.ResponseWriter, r *http.Request, proxy *httputil.ReverseProxy, model string) {
	// Read the request body
	requestBody, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Failed to read request body: %v", err)
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}
	r.Body.Close() // Close the original body

	// Set the new body for the request
	r.Body = io.NopCloser(bytes.NewBuffer(requestBody))

	// Create a new response recorder
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, r)

	// Copy the recorded response to the original response writer
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	w.Write(rec.Body.Bytes())

	// Log the request and response
	go logToFile(r, rec, model, requestBody)
}

func logToFile(r *http.Request, rec *httptest.ResponseRecorder, model string, requestBody []byte) {
	var err error
	timestamp := time.Now().Format("2006-01-02/T15:04:05")
	seq := time.Now().Unix()
	filename := fmt.Sprintf("POST.azure.%s.%s.%d.yaml", timestamp, model, seq)
	fullName := filepath.Join(logDir, filename)
	parent := filepath.Dir(fullName)
	_, stats := os.Stat(parent)
	if os.IsNotExist(stats) {
		err = os.MkdirAll(parent, 0755)
		if err != nil {
			log.Printf("Failed to create log directory: %v", err)
			return
		}
	}
	log.Printf("Write log to %s", fullName)

	r.Body = io.NopCloser(bytes.NewBuffer(requestBody))
	response := rec.Result()
	var responseBody []byte
	if response.Body != nil {
		responseBody, err = io.ReadAll(response.Body)
		if err != nil {
			log.Printf("Failed to dump response: %v", err)
			return
		}
	}

	//log.Printf("Response: [%s]\n", strings.TrimSpace(string(responseBody)))
	var chatRes = &openai.ChatCompletionResponse{}
	err = json.Unmarshal([]byte(strings.TrimSpace(string(responseBody))), &chatRes)
	if err != nil {
		log.Printf("Failed to unmarshal response: %v", err)
		//return
		chatRes = nil
	}
	var chatReq = openai.ChatCompletionRequest{}
	err = json.Unmarshal(requestBody, &chatReq)
	if err != nil {
		log.Printf("Failed to unmarshal request: %v", err)
		return

	}
	tmp := ""
	for _, v := range chatReq.Messages {
		tmp += "- role: " + v.Role + "\n"
		tmp += "  content: |\n" + indent(v.Content, "    ") + "\n"
	}

	res := ""
	if chatRes != nil {
		for _, v := range chatRes.Choices {
			res += "role: " + v.Message.Role + "\n"
			res += "content: | \n" + indent(v.Message.Content, "  ") + "\n"
			if v.Message.FunctionCall != nil {
				res += "function_call:\n"
				buf, _ := yaml.Marshal(v.Message.FunctionCall)
				res += indent("name:", string(buf))
			}
			if v.Message.ToolCalls != nil {
				res += "tool_calls:\n"
				buf, _ := yaml.Marshal(v.Message.ToolCalls)
				res += indent("name:", string(buf))
			}
		}
	} else {
		lines := strings.Split(string(responseBody), "\n")
		if IsStream(lines) {
			payloads := FilterLines(lines)
			mergeBody, content := MergeLines(payloads)
			res += "content: | \n" + indent(content, "  ") + "\n"
			if content == "" {
				buf, _ := json.MarshalIndent(mergeBody, "", "  ")
				if len(buf) > 0 {
					res += "raw_response: | \n" + indent(string(buf), "  ") + "\n"
				}
			}

		} else {
			res = "raw_response: | \n" + indent(string(responseBody), "  ") + "\n"
		}
	}

	logContent := fmt.Sprintf("request:\n  method: POST\n  url: %s\ninput: \n%s\noutput: \n%s\n",
		r.URL.Path, indent(string(tmp), "  "), indent(string(res), "  "))
	log.Printf("Log content: %s", logContent)
	if err := os.WriteFile(fullName, []byte(logContent), 0644); err != nil {
		log.Printf("Failed to write log file: %v", err)
	}
}

type StreamItem struct {
	Choices []struct {
		ContentFilterResults map[string]any `json:"content_filter_results"`
		Delta                struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason interface{} `json:"finish_reason"`
		Index        int         `json:"index"`
		Logprobs     interface{} `json:"logprobs"`
	} `json:"choices"`
	Created           int    `json:"created"`
	Id                string `json:"id"`
	Model             string `json:"model"`
	Object            string `json:"object"`
	SystemFingerprint string `json:"system_fingerprint"`
}

func MergeLines(lines []string) (StreamItem, string) {

	var data, tmp StreamItem
	var content bytes.Buffer

	for _, v := range lines {

		err := json.Unmarshal([]byte(v), &tmp)
		if err != nil {
			log.Printf("get error:%v,buf:[%s]", err, v)
			continue
		}

		mergeStructs(&data, &tmp)
		if len(tmp.Choices) > 0 {
			content.WriteString(tmp.Choices[0].Delta.Content)
		}
	}
	return data, content.String()
}

// 合并结构体的函数
func mergeStructs(dst, src interface{}) {
	srcVal := reflect.ValueOf(src).Elem()
	dstVal := reflect.ValueOf(dst).Elem()

	for i := 0; i < srcVal.NumField(); i++ {
		srcField := srcVal.Field(i)
		dstField := dstVal.Field(i)

		// 如果字段是结构体，则递归合并
		if srcField.Kind() == reflect.Struct {
			mergeStructs(dstField.Addr().Interface(), srcField.Addr().Interface())
		} else {
			// 检查字段是否是零值
			if !isZeroValue(srcField) {
				dstField.Set(srcField)
			}
		}
	}
}

// 检查字段是否是零值的辅助函数
func isZeroValue(v reflect.Value) bool {
	return reflect.DeepEqual(v.Interface(), reflect.Zero(v.Type()).Interface())
}

func FilterLines(lines []string) []string {
	var arr []string
	for _, v := range lines {
		v = strings.TrimSpace(v)
		if v == "data: [DONE]" || v == "" {
			continue
		}
		payload := strings.TrimPrefix(v, "data: ")
		arr = append(arr, payload)
	}
	return arr
}
func IsStream(lines []string) bool {
	c := 0
	for _, v := range lines {
		v = strings.TrimSpace(v)
		if v == "data: [DONE]" {
			continue
		}
		if strings.HasPrefix(v, "data:") {
			payload := strings.TrimPrefix(v, "data: ")
			err := json.Unmarshal([]byte(payload), &StreamItem{})
			if err != nil {
				break
			}
			c++
			if c > 2 {
				return true
			}
		}
	}
	return false
}
func indent(text, prefix string) string {
	return prefix + strings.ReplaceAll(text, "\n", "\n"+prefix)
}
