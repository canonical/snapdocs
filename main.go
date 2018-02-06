package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/golang/snappy"
	"log"
	"net/http"
	"os"
	"text/template"
	"time"

	"golang.org/x/crypto/acme/autocert"
	"io/ioutil"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

var (
	httpFlag  = flag.String("http", ":8080", "Serve HTTP at given address")
	httpsFlag = flag.String("https", "", "Serve HTTPS at given address")
	certFlag  = flag.String("cert", "", "Use the provided TLS certificate")
	keyFlag   = flag.String("key", "", "Use the provided TLS key")
	acmeFlag  = flag.String("acme", "", "Auto-request TLS certs and store in given directory")
	domainsFlag = flag.String("domains", "", "Comma-separated domain list for TLS")
)

var httpClient = &http.Client{
	Timeout: 10 * time.Second,
}

var httpServer = &http.Server{
	ReadTimeout:  30 * time.Second,
	WriteTimeout: 30 * time.Second,
}

func main() {
	if err := run(); err != nil {
		fmt.Errorf("error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	flag.Parse()

	http.HandleFunc("/", handler)

	if *httpFlag == "" && *httpsFlag == "" {
		return fmt.Errorf("must provide -http and/or -https")
	}
	if *acmeFlag != "" && *httpsFlag == "" {
		return fmt.Errorf("cannot use -acme without -https")
	}
	if *acmeFlag != "" && (*certFlag != "" || *keyFlag != "") {
		return fmt.Errorf("cannot provide -acme with -key or -cert")
	}
	if *acmeFlag == "" && (*httpsFlag != "" || *certFlag != "" || *keyFlag != "") && (*httpsFlag == "" || *certFlag == "" || *keyFlag == "") {
		return fmt.Errorf("-https -cert and -key must be used together")
	}

	ch := make(chan error, 2)

	if *acmeFlag != "" {
		// So a potential error is seen upfront.
		if err := os.MkdirAll(*acmeFlag, 0700); err != nil {
			return err
		}
	}

	if *httpFlag != "" && (*httpsFlag == "" || *acmeFlag == "") {
		server := *httpServer
		server.Addr = *httpFlag
		go func() {
			ch <- server.ListenAndServe()
		}()
	}
	if *httpsFlag != "" {
		server := *httpServer
		server.Addr = *httpsFlag
		if *acmeFlag != "" {
			domains := append([]string{"localhost"}, strings.Split(*domainsFlag, ",")...)
			m := autocert.Manager{
				Prompt:      autocert.AcceptTOS,
				Cache:       autocert.DirCache(*acmeFlag),
				RenewBefore: 24 * 30 * time.Hour,
				HostPolicy: autocert.HostWhitelist(domains...),
				Email: "gustavo@niemeyer.net",
			}
			server.TLSConfig = &tls.Config{
				GetCertificate: m.GetCertificate,
			}
			go func() {
				ch <- http.ListenAndServe(":80", m.HTTPHandler(nil))
			}()
		}
		go func() {
			ch <- server.ListenAndServeTLS(*certFlag, *keyFlag)
		}()
	}
	log.Printf("Started!")
	return <-ch
}

var pagePathPattern = regexp.MustCompile("^(?:/([a-z0-9-]+))?/([0-9]+)(?:/[0-9]+)?$")

func topicPathID(path string) (int, error) {
	m := pagePathPattern.FindStringSubmatch(path)
	if m == nil {
		return 0, fmt.Errorf("unsupported URL path")
	}
	id, err := strconv.Atoi(m[2])
	if err != nil {
		return 0, fmt.Errorf("internal error: URL pattern matched with non-int page ID")
	}
	return id, nil
}

func sendNotFound(resp http.ResponseWriter, msg string, args ...interface{}) {
	if len(args) > 0 {
		msg = fmt.Sprintf(msg, args...)
	}
	resp.WriteHeader(http.StatusNotFound)
	resp.Write([]byte(msg))
}

func handler(resp http.ResponseWriter, req *http.Request) {
	if req.Method != "GET" {
		resp.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if req.URL.Path == "/health-check" {
		resp.Write([]byte("ok"))
		return
	}
	if req.URL.Path == "/favicon.ico" {
		resp.WriteHeader(http.StatusNotFound)
		return
	}
	if strings.HasPrefix(req.URL.Path, "/t/") {
		log.Printf("Got request for %s from %s: redirecting to strip /t/", req.URL, req.RemoteAddr)
		resp.Header().Set("Location", strings.TrimPrefix(req.URL.Path, "/t"))
		resp.WriteHeader(http.StatusPermanentRedirect)
		return
	}

	log.Printf("Got request for %s from %s", req.URL, req.RemoteAddr)

	req.ParseForm()

	if req.URL.Path == "/" {
		req.URL.Path = indexPagePath
	}

	var topic *Topic
	var err error

	m := pagePathPattern.FindStringSubmatch(req.URL.Path)
	if m == nil {
		err = fmt.Errorf("invalid URL pattern")
	} else {
		if len(req.Form["refresh"]) > 0 {
			forum.Refresh(req.URL.Path)
		}
		topic, err = forum.Topic(req.URL.Path)
	}
	if err != nil {
		log.Printf("Cannot send %s to %s: %v", req.URL, req.RemoteAddr, err)
		resp.Header().Set("Location", "/")
		resp.WriteHeader(http.StatusTemporaryRedirect)
		return
	}

	if topic.Category != docCategory {
		log.Printf("Cannot send %s to %s: %v", req.URL, req.RemoteAddr, err)
		resp.Header().Set("Location", topic.ForumURL())
		resp.WriteHeader(http.StatusTemporaryRedirect)
		return
	}

	resp.Header().Set("Content-Type", "text/html")
	renderPage(resp, req, topic)
}

const docCategory = 15

type Topic struct {
	ID       int    `json:"id"`
	Slug     string `json:"slug"`
	Title    string `json:"title"`
	Category int    `json:"category_id"`

	Post    *Post
	content []byte
}

func (t *Topic) String() string {
	return fmt.Sprintf("/%s/%d", t.Slug, t.ID)
}

func (t *Topic) ForumURL() string {
	return fmt.Sprintf("https://forum.snapcraft.io/t/%s/%d", t.Slug, t.ID)
}

func (t *Topic) Content() string {
	content, err := snappy.Decode(nil, t.content)
	if err != nil {
		log.Printf("internal error: cannot decompress content of %s: %v", t, err)
		return "Internal error: cannot decompress content. Please report!"
	}
	return string(content)
}

type Post struct {
	Username  string    `json:"username"`
	Cooked    string    `json:"cooked"`
	UpdatedAt time.Time `json:"updated_at"`
}

var forum Forum

type Forum struct {
	cache map[int]*topicCache
	mu    sync.Mutex
}

type topicCache struct {
	mu    sync.Mutex
	time  time.Time
	topic *Topic
}

const topicCacheTimeout = 1 * time.Hour
const topicCacheFallback = 7 * 24 * time.Hour

func (f *Forum) Refresh(path string) {
	id, err := topicPathID(path)
	if err == nil {
		f.mu.Lock()
		if _, ok := f.cache[id]; ok {
			log.Printf("Asked to refresh %s: discarding topic cache", path)
		} else {
			log.Printf("Asked to refresh %s: topic was not cached", path)
		}
		delete(f.cache, id)
		f.mu.Unlock()
	}
}

func (f *Forum) Topic(path string) (topic *Topic, err error) {
	id, err := topicPathID(path)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	f.mu.Lock()
	if f.cache == nil {
		f.cache = make(map[int]*topicCache)
	}
	cache, ok := f.cache[id]
	if !ok {
		cache = &topicCache{}
		f.cache[id] = cache
	}
	f.mu.Unlock()

	cache.mu.Lock()
	defer cache.mu.Unlock()

	if cache.time.Add(topicCacheTimeout).After(now) {
		return cache.topic, nil
	}

	defer func() {
		if err != nil {
			if cache.topic != nil && cache.time.Add(topicCacheFallback).After(now) {
				topic = cache.topic
				err = nil
			} else {
				f.mu.Lock()
				delete(f.cache, id)
				f.mu.Unlock()
			}
		}
	}()

	log.Printf("Fetching content for %s...", path)

	resp, err := httpClient.Get("https://forum.snapcraft.io/t/" + strings.Trim(path, "/") + ".json")
	if err != nil {
		return nil, fmt.Errorf("cannot obtain documentation page: %v", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case 200:
		// ok
	case 401, 404:
		return nil, fmt.Errorf("documentation page not found")

	default:
		return nil, fmt.Errorf("cannot obtain documentation page: got %v status", resp.StatusCode)
	}

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("cannot read documentation page: %v", err)
	}

	var result struct {
		*Topic
		PostStream struct {
			Posts []*Post
		} `json:"post_stream"`
	}
	err = json.Unmarshal(data, &result)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal documentation page: %v", err)
	}

	if result.Topic == nil || len(result.PostStream.Posts) == 0 {
		return nil, fmt.Errorf("internal error: documentation page seems empty!?", err)
	}

	topic = result.Topic
	topic.Post = result.PostStream.Posts[0]
	content := topic.Post.Cooked
	topic.Post.Cooked = ""
	content = strings.Replace(content, `href="/`, `href="https://forum.snapcraft.io/`, -1)
	content = strings.Replace(content, `href="https://forum.snapcraft.io/t/`, `href="/`, -1)
	topic.content = snappy.Encode(nil, []byte(content))

	cache.topic = topic
	cache.time = time.Now()

	return topic, nil
}

type pageData struct {
	Topic   *Topic
	Index   string
	Content string
}

var (
	indexPagePath = "/documentation-outline/3781"
	indexPageSep = "<h1>Content</h1>"
	indexPageTitle = "Welcome"
)


func renderPage(resp http.ResponseWriter, req *http.Request, topic *Topic) {
	index, err := forum.Topic(indexPagePath)
	if err != nil {
		log.Printf("Cannot obtain documentation index: %v", err)
	}

	data := &pageData{
		Topic:   topic,
		Index:   index.Content(),
		Content: topic.Content(),
	}

	sep := strings.Index(data.Index, indexPageSep)
	if sep >= 0 {
		data.Index = data.Index[sep+len(indexPageSep):]
		if topic.ID == index.ID {
			index.Title = indexPageTitle
			data.Content = data.Content[:sep]
		}
	}

	err = pageTemplate.Execute(resp, data)
	if err != nil {
		log.Printf("Cannot execute page template: %v", err)
	}
}

var pageTemplate *template.Template

var pageFuncs = template.FuncMap{
	"formatTime":    formatTime,
	"stringBetween": stringBetween,
}

func formatTime(t time.Time) string {
	return t.Format("2006-01-02 15:04:05 UTC")
}

func stringBetween(after, until, content string) string {
	afterExp, err := regexp.Compile(after)
	if err != nil {
		log.Printf("internal error: cannot compile after expression: %q", after)
	} else {
		m := afterExp.FindStringSubmatchIndex(content)
		switch {
		case len(m) > 2:
			content = content[m[2]:m[3]] + content[m[1]:]
		case len(m) > 0:
			content = content[m[1]:]
		}
	}
	untilExp, err := regexp.Compile(until)
	if err != nil {
		log.Printf("internal error: cannot compile until expression: %q", until)
	} else {
		m := untilExp.FindStringSubmatchIndex(content)
		switch {
		case len(m) > 2:
			content = content[:m[0]] + content[m[2]:m[3]]
		case len(m) > 0:
			content = content[:m[0]]
		}
	}
	return content
}

func init() {
	var err error
	pageTemplate, err = template.New("page").Funcs(pageFuncs).Parse(pageTemplateString)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: parsing page template failed: %s\n", err)
		os.Exit(1)
	}
}

const pageTemplateString = `<!DOCTYPE html>
<html>

<head>

<meta charset="utf-8">
<title>{{.Topic.Title}} - Snap Docs</title>
<meta name="viewport" content="width=device-width, initial-scale=1.0, minimum-scale=1.0, maximum-scale=1.0, user-scalable=no">
<link href="//maxcdn.bootstrapcdn.com/bootstrap/3.3.7/css/bootstrap.min.css" rel="stylesheet" integrity="sha384-BVYiiSIFeK1dGmJRAkycuHAHRg32OmUcww7on3RYdg4Va+PmSTsz/K68vbdEjh4u" crossorigin="anonymous">

<style>

html body {
	height: 100%;
}

body {
	font-family: Helvetica, Arial, sans-serif;
}

blockquote {
	font-family: Helvetica, Arial, sans-serif;
	font-size: 14px;
	border: 1px solid #eee;
	border-left: 5px solid #eee;
}

code {
	background-color: #f5f5f5;
	/*border: 1px solid #ccc;*/
	color: #333;
}

pre, pre code {
	font-family: Consolas, Menlo, Monaco, "Lucida Console", "Liberation Mono", "DejaVu Sans Mono", "Bitstream Vera Sans Mono", "Courier New", monospace;
	white-space: pre !important;
	overflow: auto;
	max-height: 500px;
}

.page-body h1, .index h1 {
	margin-top: 30px;
	font-size: 1.6em;
}
.page-body h2, .index h2 {
	font-size: 1.4em;
}
.page-body h3, .index h3 {
	font-size: 1.2em;
}

.page-footer {
	margin-bottom: 100px;
}

.index ul {
	padding-left: 0;
	list-style: none;
	display: table;
	content: " ";
	clear: both;
}
.index li {
	display: block;
	clear: both;
}
.index a {
	padding: 5px 10px;
	text-decoration: none;
	display: block;
}

.sidebar {
	position: fixed;
	top: 0;
	bottom: 0;
	left: 0;
	z-index: 1000;
	display: block;
	padding: 20px;
	overflow-x: hidden;
	overflow-y: auto;
	color: rgba(0,0,0,.85);
	border-right: 1px solid rgba(0,0,0,.1);
	background-color: white;
	max-width: 300px;
}

@media (max-width: 768px) {
	.sidebar {
		position: relative;
	}
}

.post-info {
	font-family: "Helvetica Neue",Helvetica,Arial,sans-serif;
	font-size: 14px;
}

img.emoji {
	width: 20px;
	height: 20px;
	vertical-align: middle;
}

img:not(.thumbnail) {
    max-width: 690px;
    max-height: 500px;
}

table {
	border-collapse: collapse;
	border-spacing: 0;
}

table thead {
	border-bottom: 2px solid #eee;
}

table thead th {
	padding-bottom: 2px;
}

table tr {
	border-bottom: 1px solid #eee;
}

table td {
	padding: 3px 3px 3px 10px;
}

</style>

</head>

<body>

<div class="container">
	<div class="row">
		<div class="index sidebar col-sm-3">
			<h1>Documentation</h1>
			<div>
			{{.Index}}
			</div>
		</div>
		<div class="content col-sm-9 col-sm-offset-3">
			<div class="page-header">
				<h1>{{.Topic.Title}}</h1>
			</div>
			<div class="alert alert-info" role="alert">This content is <strong>experimental</strong>. Make sure to visit the <a href="https://docs.snapcraft.io/">official site</a>.</div>
			<div class="page-body">
				{{.Content}}
			</div>
			<div class="page-footer">
				<hr>
				<div class="text-muted credit">
				<div>For questions and comments see <a href="{{.Topic.ForumURL}}">the forum topic</a>.</div>
				<div>Last update on {{formatTime .Topic.Post.UpdatedAt}}.</div>
				</div>
			</div>
		</div>
	</div>
</div>

</body>

</html>
`
