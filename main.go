package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/golang/snappy"
	"html/template"
	"log"
	"net/http"
	"os"
	"time"

	"golang.org/x/crypto/acme/autocert"
	"io/ioutil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

var (
	httpFlag    = flag.String("http", ":8080", "Serve HTTP at given address")
	httpsFlag   = flag.String("https", "", "Serve HTTPS at given address")
	certFlag    = flag.String("cert", "", "Use the provided TLS certificate")
	keyFlag     = flag.String("key", "", "Use the provided TLS key")
	acmeFlag    = flag.String("acme", "", "Auto-request TLS certs and store in given directory")
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
				HostPolicy:  autocert.HostWhitelist(domains...),
				Email:       "gustavo@niemeyer.net",
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

	if req.URL.Path == "/" {
		req.URL.Path = indexPagePath
	}

	req.ParseForm()

	var results []*Topic
	var topic *Topic
	var err error

	if req.URL.Path == "/search" {
		results, err = forum.Search(req.Form.Get("q"))
	} else if m := pagePathPattern.FindStringSubmatch(req.URL.Path); m != nil {
		if len(req.Form["refresh"]) > 0 {
			forum.Refresh(req.URL.Path)
		}
		topic, err = forum.Topic(req.URL.Path)
	} else {
		err = fmt.Errorf("invalid URL pattern")
	}
	if err != nil {
		log.Printf("Cannot send %s to %s: %v", req.URL, req.RemoteAddr, err)
		resp.Header().Set("Location", "/")
		resp.WriteHeader(http.StatusTemporaryRedirect)
		return
	}

	if topic != nil && topic.Category != docCategory {
		log.Printf("Cannot send %s to %s: %v", req.URL, req.RemoteAddr, err)
		resp.Header().Set("Location", topic.ForumURL())
		resp.WriteHeader(http.StatusTemporaryRedirect)
		return
	}

	resp.Header().Set("Content-Type", "text/html")
	renderPage(resp, req, topic, results)
}

const docCategory = 15

type Topic struct {
	ID       int       `json:"id"`
	Slug     string    `json:"slug"`
	Title    string    `json:"title"`
	Category int       `json:"category_id"`
	BumpedAt time.Time `json:"bumped_at"`

	Post    *Post
	content []byte
}

func (t *Topic) String() string {
	return fmt.Sprintf("/%s/%d", t.Slug, t.ID)
}

func (t *Topic) ForumURL() string {
	return fmt.Sprintf("https://forum.snapcraft.io/t/%s/%d", t.Slug, t.ID)
}

func (t *Topic) setPost(post *Post) {
	t.Post = post
	content := t.Post.Cooked
	t.Post.Cooked = ""
	content = strings.Replace(content, `href="/`, `href="https://forum.snapcraft.io/`, -1)
	content = strings.Replace(content, `href="https://forum.snapcraft.io/t/`, `href="/`, -1)
	t.content = snappy.Encode(nil, []byte(content))
}

func (t *Topic) Content() string {
	content, err := snappy.Decode(nil, t.content)
	if err != nil {
		log.Printf("internal error: cannot decompress content of %s: %v", t, err)
		return "Internal error: cannot decompress content. Please report!"
	}
	return string(content)
}

func (t *Topic) LastUpdate() time.Time {
	if t.Post == nil || t.Post.UpdatedAt.IsZero() {
		// Search results do not include updated_at. That's the next best thing.
		return t.BumpedAt
	}
	return t.Post.UpdatedAt
}

func (t *Topic) Blurb() string {
	if t.Post != nil {
		return t.Post.Blurb
	}
	return ""
}

type Post struct {
	Username  string    `json:"username"`
	Cooked    string    `json:"cooked"`
	UpdatedAt time.Time `json:"updated_at"`
	TopicID   int       `json:"topic_id"`
	Blurb     string    `json:"blurb"`
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

func (f *Forum) Search(query string) ([]*Topic, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}

	log.Printf("Fetching search results for: %s", query)

	q := url.Values{"q": []string{"#doc @wiki " + query}}.Encode()

	resp, err := httpClient.Get("https://forum.snapcraft.io/search.json?" + q)
	if err != nil {
		return nil, fmt.Errorf("cannot obtain search results: %v", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case 200:
		// ok
	default:
		return nil, fmt.Errorf("cannot obtain search results: got %v status", resp.StatusCode)
	}

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("cannot read search results: %v", err)
	}

	var result struct {
		Posts  []*Post
		Topics []*Topic
	}
	err = json.Unmarshal(data, &result)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal search results: %v", err)
	}

	topicID := make(map[int]*Topic, len(result.Topics))
	for _, topic := range result.Topics {
		topicID[topic.ID] = topic
	}

	var topics []*Topic
	for _, post := range result.Posts {
		if topic, ok := topicID[post.TopicID]; ok && topic.ID != indexPageID {
			topic.setPost(post)
			topics = append(topics, topic)
		}
	}

	// Take the chance we have the content at hand and replace all cached posts.
	now := time.Now()
	f.mu.Lock()
	if f.cache == nil {
		f.cache = make(map[int]*topicCache)
	}
	for _, topic := range topics {
		f.cache[topic.ID] = &topicCache{
			topic: topic,
			time:  now,
		}
	}
	f.mu.Unlock()

	return topics, nil
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

	result.Topic.setPost(result.PostStream.Posts[0])

	cache.topic = result.Topic
	cache.time = time.Now()

	return result.Topic, nil
}

type pageData struct {
	Index   string
	Topic   *Topic
	Content string
	Query   string
	Results []*Topic
	Logo    string
}

var (
	indexPagePath  = "/documentation-outline/3781"
	indexPageID    = 0
	indexPageSep   = "<h1>Content</h1>"
	indexPageTitle = "Welcome"
)

func init() {
	var err error
	indexPageID, err = topicPathID(indexPagePath)
	if err != nil {
		panic(fmt.Errorf("internal error: cannot parse indexPagePath ID: %s", indexPagePath))
	}
}

func renderPage(resp http.ResponseWriter, req *http.Request, topic *Topic, results []*Topic) {
	index, err := forum.Topic(indexPagePath)
	if err != nil {
		log.Printf("Cannot obtain documentation index: %v", err)
	}

	data := &pageData{
		Index:   index.Content(),
		Query:   req.Form.Get("q"),
		Results: results,
		Logo:    logoString,
	}

	if topic != nil {
		data.Topic = topic
		data.Content = topic.Content()
	}

	sep := strings.Index(data.Index, indexPageSep)
	if sep >= 0 {
		data.Index = data.Index[sep+len(indexPageSep):]
		if topic != nil && topic.ID == index.ID {
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
	"html":          unescapeHTML,
	"formatTime":    formatTime,
	"stringBetween": stringBetween,
}

func unescapeHTML(s string) template.HTML {
	return template.HTML(s)
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
<title>{{if .Topic}}{{.Topic.Title}}{{else if .Query}}{{.Query}}{{else}}Search Results{{end}} - Snap Docs</title>
<meta name="viewport" content="width=device-width, initial-scale=1.0, minimum-scale=1.0, maximum-scale=1.0, user-scalable=no">
<link href="https://maxcdn.bootstrapcdn.com/bootstrap/3.3.7/css/bootstrap.min.css" rel="stylesheet" integrity="sha384-BVYiiSIFeK1dGmJRAkycuHAHRg32OmUcww7on3RYdg4Va+PmSTsz/K68vbdEjh4u" crossorigin="anonymous">
<!--<link href="https://maxcdn.bootstrapcdn.com/font-awesome/4.7.0/css/font-awesome.min.css" rel="stylesheet">-->

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
	font-size: 1.8em;
}
.page-body h2, .index h2 {
	font-size: 1.6em;
}
.page-body h3, .index h3 {
	font-size: 1.4em;
}
.page-body h4, .index h4 {
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

.search {
	margin-top: 20px;
}

input[type=search] {
	border-radius: 10px;
	border: 1px solid #ccc;
	text-indent: 10px;
	width: auto;
	max-width: 200px;
}

.page-body .search input {
	font-size: 1.2em;
	width: 100%;
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
	vertical-align: top;
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
			{{html .Logo}}
			<div class="search">
				<form method="GET" action="/search">
					<input type="search" name="q" placeholder="&#x1f50d; Search" value="{{.Query}}">
					<input type="submit" style="position: absolute; left: -9999px; width: 1px; height: 1px;" tabindex="-1"/>
				</form>
			</div>
			<div>
			{{html .Index}}
			</div>
		</div>
		<div class="content col-sm-9 col-sm-offset-3">
			<div class="page-header">
				<h1>{{if .Topic}}{{.Topic.Title}}{{else}}Search{{end}}</h1>
			</div>
			<div class="alert alert-info" role="alert">This content is <strong>experimental</strong>. Make sure to visit the <a href="https://docs.snapcraft.io/">official site</a>.</div>
			<div class="page-body">
				{{if .Topic}}
				{{html .Content}}
				{{else}}
				<div class="search">
					<form method="GET" action="/search">
						<input type="search" name="q" placeholder="&#x1f50d; Terms to search for" value="{{.Query}}">
						<input type="submit" style="position: absolute; left: -9999px; width: 1px; height: 1px;" tabindex="-1"/>
					</form>
				</div>
				{{range .Results}}
				<h1 class="result-title"><a href="{{.}}">{{.Title}}</a></h1>
				<div class="result-blurb">{{html .Blurb}}</div>
				{{else}}
				{{if .Query}}<h3>Cannot find any documents matching <code>{{.Query}}</code> right now.</h3>{{end}}
				{{end}}
				{{end}}
			</div>
			<div class="page-footer">
				<hr>
				<div class="text-muted credit">
				{{if .Topic}}
				<div>For questions and comments see <a href="{{.Topic.ForumURL}}">the forum topic</a>.</div>
				<div>Last update on {{formatTime .Topic.LastUpdate}}.</div>
				{{else if .Query}}
				<div>{{if .Results}}Cannot find what you are looking for? {{end}}Consider asking about it <a href="https://forum.snapcraft.io/">in the forum</a>.</div>
				{{end}}
				</div>
			</div>
		</div>
	</div>
</div>

</body>

</html>
`

var logoString = `
<svg
   xmlns:dc="http://purl.org/dc/elements/1.1/"
   xmlns:cc="http://creativecommons.org/ns#"
   xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"
   xmlns:svg="http://www.w3.org/2000/svg"
   xmlns="http://www.w3.org/2000/svg"
   xmlns:sodipodi="http://sodipodi.sourceforge.net/DTD/sodipodi-0.dtd"
   xmlns:inkscape="http://www.inkscape.org/namespaces/inkscape"
   viewBox="0 0 157.24001 45.309734"
   height="45.309734"
   width="157.24001"
   id="svg3750"
   version="1.1"
   sodipodi:docname="snapcraft-docs.svg"
   inkscape:version="0.92.2 (30a58d1, 2017-08-07)">
  <sodipodi:namedview
     pagecolor="#ffffff"
     bordercolor="#666666"
     borderopacity="1"
     objecttolerance="10"
     gridtolerance="10"
     guidetolerance="10"
     inkscape:pageopacity="0"
     inkscape:pageshadow="2"
     inkscape:window-width="1916"
     inkscape:window-height="2120"
     id="namedview20"
     showgrid="false"
     units="px"
     inkscape:zoom="4.1465275"
     inkscape:cx="97.913256"
     inkscape:cy="-15.92634"
     inkscape:window-x="0"
     inkscape:window-y="36"
     inkscape:window-maximized="0"
     inkscape:current-layer="g3921"
     showguides="true"
     inkscape:guide-bbox="true"
     fit-margin-top="0"
     fit-margin-left="0"
     fit-margin-right="0"
     fit-margin-bottom="0" />
  <metadata
     id="metadata3756">
    <rdf:RDF>
      <cc:Work
         rdf:about="">
        <dc:format>image/svg+xml</dc:format>
        <dc:type
           rdf:resource="http://purl.org/dc/dcmitype/StillImage" />
        <dc:title></dc:title>
      </cc:Work>
    </rdf:RDF>
  </metadata>
  <defs
     id="defs3754">
    <style
       id="style3870">.cls-1{fill:#464646;}.cls-2{fill:#82bea0;}.cls-3{fill:#fa6441;}</style>
  </defs>
  <g
     transform="translate(-25.38,-12)"
     id="g3921">
    <path
       style="fill:#464646"
       class="cls-1"
       d="m 77.61,36.63 a 6,6 0 0 0 2.69,-0.54 1.88,1.88 0 0 0 1.06,-1.82 2.63,2.63 0 0 0 -0.19,-1 2.07,2.07 0 0 0 -0.63,-0.79 5.37,5.37 0 0 0 -1.15,-0.67 L 77.64,31 q -0.85,-0.35 -1.6,-0.71 a 6.51,6.51 0 0 1 -1.34,-0.84 3.67,3.67 0 0 1 -0.93,-1.15 3.49,3.49 0 0 1 -0.35,-1.63 3.66,3.66 0 0 1 1.38,-3 5.82,5.82 0 0 1 3.8,-1.14 10.31,10.31 0 0 1 2.55,0.26 8.08,8.08 0 0 1 1.41,0.47 L 82.13,25 a 11.37,11.37 0 0 0 -1.18,-0.47 7.75,7.75 0 0 0 -2.43,-0.29 5.36,5.36 0 0 0 -1.21,0.13 3.22,3.22 0 0 0 -1,0.41 2.19,2.19 0 0 0 -0.7,0.7 1.93,1.93 0 0 0 -0.26,1 2.21,2.21 0 0 0 0.23,1.05 2.33,2.33 0 0 0 0.69,0.77 5.76,5.76 0 0 0 1.11,0.63 l 1.5,0.64 q 0.87,0.35 1.68,0.71 a 6.18,6.18 0 0 1 1.41,0.87 4,4 0 0 1 1,1.22 3.8,3.8 0 0 1 0.38,1.79 3.5,3.5 0 0 1 -1.53,3.09 7.3,7.3 0 0 1 -4.18,1 10.52,10.52 0 0 1 -3,-0.34 11.53,11.53 0 0 1 -1.4,-0.51 l 0.5,-1.72 a 3.06,3.06 0 0 0 0.38,0.19 7.92,7.92 0 0 0 0.79,0.29 8.47,8.47 0 0 0 1.18,0.28 9.58,9.58 0 0 0 1.52,0.19 z"
       id="path3876"
       inkscape:connector-curvature="0" />
    <path
       style="fill:#464646"
       class="cls-1"
       d="m 85.92,23.44 q 0.84,-0.23 2.27,-0.52 a 17.87,17.87 0 0 1 3.5,-0.29 7.28,7.28 0 0 1 2.87,0.5 4.4,4.4 0 0 1 1.84,1.41 5.77,5.77 0 0 1 1,2.2 12.78,12.78 0 0 1 0.29,2.83 V 38 h -1.92 v -7.83 a 14.31,14.31 0 0 0 -0.22,-2.71 4.53,4.53 0 0 0 -0.73,-1.81 2.83,2.83 0 0 0 -1.34,-1 6.09,6.09 0 0 0 -2.08,-0.31 15.86,15.86 0 0 0 -2.32,0.15 7.11,7.11 0 0 0 -1.27,0.26 V 38 h -1.89 z"
       id="path3878"
       inkscape:connector-curvature="0" />
    <path
       style="fill:#464646"
       class="cls-1"
       d="m 105.92,22.56 a 6.68,6.68 0 0 1 2.52,0.42 4.33,4.33 0 0 1 1.67,1.17 4.59,4.59 0 0 1 0.93,1.76 8,8 0 0 1 0.29,2.21 v 9.5 a 8.56,8.56 0 0 1 -0.84,0.19 l -1.28,0.22 q -0.73,0.12 -1.62,0.19 -0.89,0.07 -1.82,0.07 a 8.69,8.69 0 0 1 -2.2,-0.26 4.86,4.86 0 0 1 -1.75,-0.83 3.91,3.91 0 0 1 -1.16,-1.46 5,5 0 0 1 -0.42,-2.17 4.4,4.4 0 0 1 0.47,-2.1 4,4 0 0 1 1.29,-1.47 5.86,5.86 0 0 1 2,-0.83 11.56,11.56 0 0 1 2.53,-0.26 c 0.27,0 0.56,0 0.86,0 0.3,0 0.59,0.07 0.87,0.12 l 0.73,0.15 a 2.3,2.3 0 0 1 0.42,0.13 v -0.93 a 9.92,9.92 0 0 0 -0.12,-1.53 3.35,3.35 0 0 0 -0.51,-1.34 2.83,2.83 0 0 0 -1.11,-1 4.15,4.15 0 0 0 -1.88,-0.36 9.72,9.72 0 0 0 -2.48,0.23 q -0.82,0.23 -1.19,0.38 l -0.26,-1.66 a 7,7 0 0 1 1.53,-0.44 13.07,13.07 0 0 1 2.53,-0.1 z m 0.18,14 q 1.11,0 1.94,-0.07 a 13.15,13.15 0 0 0 1.41,-0.19 V 31 a 5.8,5.8 0 0 0 -1,-0.31 9,9 0 0 0 -1.92,-0.16 10.93,10.93 0 0 0 -1.46,0.1 4.23,4.23 0 0 0 -1.4,0.44 2.91,2.91 0 0 0 -1,0.92 2.64,2.64 0 0 0 -0.41,1.54 3.31,3.31 0 0 0 0.28,1.43 2.3,2.3 0 0 0 0.79,0.93 3.49,3.49 0 0 0 1.22,0.51 7.39,7.39 0 0 0 1.55,0.2 z"
       id="path3880"
       inkscape:connector-curvature="0" />
    <path
       style="fill:#464646"
       class="cls-1"
       d="m 116.51,43.36 h -1.89 V 23.44 a 17.12,17.12 0 0 1 2.16,-0.55 17.39,17.39 0 0 1 3.32,-0.26 8.1,8.1 0 0 1 3,0.54 6.53,6.53 0 0 1 2.33,1.56 7.1,7.1 0 0 1 1.57,2.46 9.45,9.45 0 0 1 0.54,3.29 10.53,10.53 0 0 1 -0.45,3.16 7,7 0 0 1 -1.33,2.48 6.09,6.09 0 0 1 -2.14,1.62 6.87,6.87 0 0 1 -2.9,0.58 7.14,7.14 0 0 1 -2.58,-0.42 6.72,6.72 0 0 1 -1.59,-0.8 z m 0,-8.1 a 6.3,6.3 0 0 0 0.66,0.44 6.2,6.2 0 0 0 0.92,0.44 7.41,7.41 0 0 0 1.14,0.33 6.09,6.09 0 0 0 1.28,0.13 5.14,5.14 0 0 0 2.34,-0.48 4.1,4.1 0 0 0 1.53,-1.31 5.56,5.56 0 0 0 0.84,-2 10.38,10.38 0 0 0 0.26,-2.37 6.43,6.43 0 0 0 -1.48,-4.51 5.13,5.13 0 0 0 -3.93,-1.59 15.61,15.61 0 0 0 -2.26,0.13 8.47,8.47 0 0 0 -1.3,0.28 z"
       id="path3882"
       inkscape:connector-curvature="0" />
    <path
       style="fill:#464646"
       class="cls-1"
       d="m 140.25,37.22 a 3.83,3.83 0 0 1 -0.76,0.36 9.82,9.82 0 0 1 -0.95,0.29 9.2,9.2 0 0 1 -1,0.2 7.64,7.64 0 0 1 -1,0.07 6.45,6.45 0 0 1 -5,-2 8.08,8.08 0 0 1 -1.78,-5.61 10.77,10.77 0 0 1 0.47,-3.29 7.19,7.19 0 0 1 1.33,-2.48 5.65,5.65 0 0 1 2.1,-1.56 6.9,6.9 0 0 1 2.78,-0.54 10.87,10.87 0 0 1 2.1,0.17 5.75,5.75 0 0 1 1.54,0.52 l -0.29,0.9 a 5.14,5.14 0 0 0 -1.46,-0.5 9.67,9.67 0 0 0 -1.89,-0.17 5,5 0 0 0 -4.17,1.81 8,8 0 0 0 -1.46,5.13 7.34,7.34 0 0 0 1.47,5 5.39,5.39 0 0 0 4.3,1.69 7.56,7.56 0 0 0 1.86,-0.26 6.46,6.46 0 0 0 1.66,-0.64 z"
       id="path3884"
       inkscape:connector-curvature="0" />
    <path
       style="fill:#464646"
       class="cls-1"
       d="m 144.07,38 h -1 V 23.78 a 9.74,9.74 0 0 1 2.2,-0.79 10.4,10.4 0 0 1 2.32,-0.26 7.7,7.7 0 0 1 2.68,0.38 l -0.2,0.87 a 6.06,6.06 0 0 0 -1.11,-0.23 11,11 0 0 0 -1.43,-0.09 8.86,8.86 0 0 0 -1.79,0.19 7.73,7.73 0 0 0 -1.68,0.54 z"
       id="path3886"
       inkscape:connector-curvature="0" />
    <path
       style="fill:#464646"
       class="cls-1"
       d="m 161.87,37.48 a 15.76,15.76 0 0 1 -2.35,0.5 18.31,18.31 0 0 1 -2.49,0.17 6.85,6.85 0 0 1 -4.31,-1.15 4.1,4.1 0 0 1 -1.46,-3.42 3.85,3.85 0 0 1 1.52,-3.31 7.67,7.67 0 0 1 4.57,-1.12 11.75,11.75 0 0 1 1.86,0.16 10.05,10.05 0 0 1 1.66,0.39 v -1.14 a 5.89,5.89 0 0 0 -1,-3.79 4.19,4.19 0 0 0 -3.37,-1.17 10,10 0 0 0 -1.91,0.19 6.22,6.22 0 0 0 -1.56,0.48 l -0.15,-0.93 a 9.26,9.26 0 0 1 3.79,-0.67 5,5 0 0 1 3.92,1.38 5.73,5.73 0 0 1 1.28,3.95 z m -1,-6.9 a 8.51,8.51 0 0 0 -1.57,-0.41 11.76,11.76 0 0 0 -1.92,-0.15 q -5.07,0 -5.07,3.55 a 3.22,3.22 0 0 0 1.18,2.75 6.15,6.15 0 0 0 3.69,0.89 17.73,17.73 0 0 0 1.85,-0.1 12,12 0 0 0 1.85,-0.33 z"
       id="path3888"
       inkscape:connector-curvature="0" />
    <path
       style="fill:#464646"
       class="cls-1"
       d="m 165.91,38 h -1 V 21.1 a 6.31,6.31 0 0 1 1.27,-4.36 5,5 0 0 1 3.82,-1.38 7.41,7.41 0 0 1 1.56,0.16 5.1,5.1 0 0 1 1.15,0.36 l -0.23,0.9 a 5.58,5.58 0 0 0 -1.21,-0.36 6.75,6.75 0 0 0 -1.27,-0.12 4,4 0 0 0 -3.09,1.09 5.5,5.5 0 0 0 -1,3.77 V 23 h 6.29 v 1 h -6.29 z"
       id="path3890"
       inkscape:connector-curvature="0" />
    <path
       style="fill:#464646"
       class="cls-1"
       d="m 182.62,37.36 a 7.22,7.22 0 0 1 -1.49,0.55 6.67,6.67 0 0 1 -1.72,0.23 A 4.48,4.48 0 0 1 176,37 q -1.11,-1.2 -1.11,-4.14 V 18.37 l 1,-0.23 V 23 h 6.09 v 0.87 h -6.09 V 33 a 7.85,7.85 0 0 0 0.23,2.08 3,3 0 0 0 0.7,1.31 2.49,2.49 0 0 0 1.12,0.67 5.38,5.38 0 0 0 1.5,0.19 6,6 0 0 0 1.68,-0.23 6.17,6.17 0 0 0 1.27,-0.5 z"
       id="path3892"
       inkscape:connector-curvature="0" />
    <polygon
       style="fill:#82bea0"
       class="cls-2"
       points="55.37,23.83 47.06,20.14 47.06,32.14 "
       id="polygon3894" />
    <polygon
       style="fill:#82bea0"
       class="cls-2"
       points="45.85,33.34 41.38,28.9 31.19,48 "
       id="polygon3896" />
    <polygon
       style="fill:#82bea0"
       class="cls-2"
       points="46.35,32.84 46.35,19.58 25.38,12 "
       id="polygon3898" />
    <polygon
       style="fill:#fa6441"
       class="cls-3"
       points="47.54,19.58 63.06,26.48 59.62,19.58 "
       id="polygon3900" />
    <g
       aria-label="documentation"
       style="font-style:normal;font-variant:normal;font-weight:normal;font-stretch:normal;font-size:17.06925011px;line-height:1.25;font-family:ubuntu;-inkscape-font-specification:ubuntu;letter-spacing:0px;word-spacing:0px;fill:#000000;fill-opacity:1;stroke:none;stroke-width:0.42673126"
       id="text873"
       transform="matrix(1.0744505,0,0,1.0744505,-13.588867,-3.1894163)">
      <path
         d="m 72.138551,49.086771 q -0.290178,-0.23897 -0.836394,-0.46087 -0.546216,-0.2219 -1.194847,-0.2219 -0.68277,0 -1.177778,0.256038 -0.477939,0.23897 -0.785186,0.68277 -0.307246,0.426732 -0.4438,1.024155 -0.136554,0.597424 -0.136554,1.280194 0,1.553302 0.768116,2.406764 0.768116,0.836394 2.04831,0.836394 0.648631,0 1.075363,-0.05121 0.4438,-0.06828 0.68277,-0.136554 z m 0,-5.974238 1.58744,-0.273108 v 12.989699 q -0.546216,0.153624 -1.399679,0.307247 -0.853462,0.153623 -1.962963,0.153623 -1.024155,0 -1.843479,-0.324316 -0.819324,-0.324315 -1.399679,-0.921739 -0.580354,-0.597424 -0.90467,-1.450886 -0.307247,-0.870532 -0.307247,-1.945895 0,-1.024155 0.256039,-1.877617 0.273108,-0.853463 0.785186,-1.467956 0.512077,-0.614493 1.246055,-0.955878 0.751047,-0.341385 1.706925,-0.341385 0.768116,0 1.348471,0.204831 0.597423,0.204831 0.887601,0.392593 z"
         style="letter-spacing:0px;stroke-width:0.42673126"
         id="path879"
         inkscape:connector-curvature="0" />
      <path
         d="m 84.306259,51.647158 q 0,1.058294 -0.307247,1.911756 -0.307246,0.853463 -0.870531,1.467956 -0.546216,0.614493 -1.314333,0.955878 -0.768116,0.324315 -1.672786,0.324315 -0.90467,0 -1.672787,-0.324315 -0.768116,-0.341385 -1.331401,-0.955878 -0.546216,-0.614493 -0.853463,-1.467956 -0.307246,-0.853462 -0.307246,-1.911756 0,-1.041224 0.307246,-1.894687 0.307247,-0.870531 0.853463,-1.485024 0.563285,-0.614493 1.331401,-0.938809 0.768117,-0.341385 1.672787,-0.341385 0.90467,0 1.672786,0.341385 0.768117,0.324316 1.314333,0.938809 0.563285,0.614493 0.870531,1.485024 0.307247,0.853463 0.307247,1.894687 z m -1.655717,0 q 0,-1.502094 -0.68277,-2.372626 -0.665701,-0.887601 -1.82641,-0.887601 -1.160709,0 -1.843479,0.887601 -0.665701,0.870532 -0.665701,2.372626 0,1.502094 0.665701,2.389695 0.68277,0.870532 1.843479,0.870532 1.160709,0 1.82641,-0.870532 0.68277,-0.887601 0.68277,-2.389695 z"
         style="letter-spacing:0px;stroke-width:0.42673126"
         id="path881"
         inkscape:connector-curvature="0" />
      <path
         d="m 90.397314,56.289994 q -1.075362,0 -1.894686,-0.341385 -0.802255,-0.341385 -1.36554,-0.955878 -0.546216,-0.614493 -0.819324,-1.450886 -0.273108,-0.853463 -0.273108,-1.877618 0,-1.024155 0.290177,-1.877617 0.307246,-0.853463 0.853462,-1.467956 0.546216,-0.631562 1.331402,-0.972947 0.802255,-0.358454 1.775202,-0.358454 0.597424,0 1.194847,0.102415 0.597424,0.102416 1.14364,0.324316 l -0.358454,1.348471 q -0.358454,-0.170693 -0.836393,-0.273108 -0.46087,-0.102416 -0.990017,-0.102416 -1.331401,0 -2.04831,0.836394 -0.699839,0.836393 -0.699839,2.440902 0,0.716909 0.153623,1.314333 0.170693,0.597423 0.512078,1.024155 0.358454,0.426731 0.90467,0.6657 0.546216,0.221901 1.331401,0.221901 0.631563,0 1.14364,-0.119485 0.512078,-0.119485 0.802255,-0.256039 l 0.2219,1.331402 q -0.136554,0.08535 -0.392593,0.170692 -0.256038,0.06828 -0.580354,0.119485 -0.324316,0.06828 -0.699839,0.102415 -0.358455,0.05121 -0.69984,0.05121 z"
         style="letter-spacing:0px;stroke-width:0.42673126"
         id="path883"
         inkscape:connector-curvature="0" />
      <path
         d="m 101.54087,55.829124 q -0.54622,0.136554 -1.45089,0.290178 -0.8876,0.153623 -2.065378,0.153623 -1.024155,0 -1.723994,-0.290177 -0.69984,-0.307247 -1.126571,-0.853463 -0.426731,-0.546216 -0.614493,-1.280194 -0.187762,-0.751047 -0.187762,-1.655717 v -4.984221 h 1.587441 v 4.642836 q 0,1.621579 0.512077,2.321418 0.512078,0.699839 1.723994,0.699839 0.256039,0 0.529147,-0.01707 0.273108,-0.01707 0.512078,-0.03414 0.238969,-0.03414 0.426731,-0.05121 0.204831,-0.03414 0.290177,-0.06828 v -7.493401 h 1.587443 z"
         style="letter-spacing:0px;stroke-width:0.42673126"
         id="path885"
         inkscape:connector-curvature="0" />
      <path
         d="m 104.27568,47.465192 q 0.54622,-0.136554 1.43382,-0.290177 0.90467,-0.153624 2.08245,-0.153624 0.85346,0 1.43381,0.23897 0.58036,0.2219 0.97295,0.665701 0.11949,-0.08535 0.37552,-0.23897 0.25604,-0.153623 0.63157,-0.290177 0.37552,-0.153623 0.83639,-0.256039 0.46087,-0.119485 0.99002,-0.119485 1.02415,0 1.67278,0.307247 0.64863,0.290177 1.00709,0.836393 0.37552,0.546216 0.49501,1.297263 0.13655,0.751047 0.13655,1.638648 v 4.984221 h -1.58744 v -4.642836 q 0,-0.785185 -0.0854,-1.348471 -0.0683,-0.563285 -0.29017,-0.938808 -0.20483,-0.375524 -0.58036,-0.546216 -0.35845,-0.187762 -0.93881,-0.187762 -0.80225,0 -1.3314,0.2219 -0.51208,0.204831 -0.69984,0.375524 0.13656,0.4438 0.20483,0.972947 0.0683,0.529147 0.0683,1.109501 v 4.984221 h -1.58744 v -4.642836 q 0,-0.785185 -0.0854,-1.348471 -0.0853,-0.563285 -0.30724,-0.938808 -0.20483,-0.375524 -0.58036,-0.546216 -0.35845,-0.187762 -0.92174,-0.187762 -0.23897,0 -0.51207,0.01707 -0.27311,0.01707 -0.52915,0.05121 -0.23897,0.01707 -0.4438,0.05121 -0.20483,0.03414 -0.27311,0.05121 v 7.493401 h -1.58744 z"
         style="letter-spacing:0px;stroke-width:0.42673126"
         id="path887"
         inkscape:connector-curvature="0" />
      <path
         d="m 118.4829,51.664227 q 0,-1.177778 0.34138,-2.04831 0.34139,-0.887601 0.90467,-1.467955 0.56329,-0.580355 1.29727,-0.870532 0.73397,-0.290177 1.50209,-0.290177 1.79227,0 2.74815,1.12657 0.95588,1.109502 0.95588,3.396781 0,0.102416 0,0.273108 0,0.153623 -0.0171,0.290177 h -6.07666 q 0.10242,1.38261 0.80226,2.099518 0.69984,0.716909 2.18486,0.716909 0.8364,0 1.39968,-0.136554 0.58036,-0.153624 0.87053,-0.290178 l 0.2219,1.331402 q -0.29017,0.153623 -1.02415,0.324316 -0.71691,0.170692 -1.63865,0.170692 -1.16071,0 -2.01417,-0.341385 -0.83639,-0.358454 -1.38261,-0.972947 -0.54622,-0.614493 -0.81932,-1.450886 -0.25604,-0.853463 -0.25604,-1.860549 z m 6.09372,-0.870531 q 0.0171,-1.075363 -0.54622,-1.758133 -0.54621,-0.699839 -1.51916,-0.699839 -0.54622,0 -0.97295,0.2219 -0.40966,0.204831 -0.69984,0.546216 -0.29017,0.341385 -0.46087,0.785185 -0.15362,0.443801 -0.20483,0.904671 z"
         style="letter-spacing:0px;stroke-width:0.42673126"
         id="path889"
         inkscape:connector-curvature="0" />
      <path
         d="m 128.51268,47.465192 q 0.54622,-0.136554 1.45089,-0.290177 0.90467,-0.153624 2.08245,-0.153624 1.05829,0 1.75813,0.307247 0.69984,0.290177 1.1095,0.836393 0.42673,0.529147 0.59742,1.280194 0.1707,0.751047 0.1707,1.655717 v 4.984221 h -1.58744 v -4.642836 q 0,-0.819324 -0.11949,-1.399678 -0.10241,-0.580355 -0.35845,-0.938809 -0.25604,-0.358454 -0.68277,-0.512078 -0.42673,-0.170692 -1.0583,-0.170692 -0.25604,0 -0.52914,0.01707 -0.27311,0.01707 -0.52915,0.05121 -0.23897,0.01707 -0.4438,0.05121 -0.18776,0.03414 -0.27311,0.05121 v 7.493401 h -1.58744 z"
         style="letter-spacing:0px;stroke-width:0.42673126"
         id="path891"
         inkscape:connector-curvature="0" />
      <path
         d="m 139.79919,47.209153 h 3.36264 v 1.331402 h -3.36264 v 4.09662 q 0,0.6657 0.10241,1.109501 0.10242,0.426731 0.30725,0.68277 0.20483,0.238969 0.51208,0.341385 0.30724,0.102415 0.71691,0.102415 0.7169,0 1.14364,-0.153623 0.4438,-0.170692 0.61449,-0.238969 l 0.30725,1.314332 q -0.23897,0.119485 -0.8364,0.290177 -0.59742,0.187762 -1.36554,0.187762 -0.90467,0 -1.50209,-0.2219 -0.58036,-0.23897 -0.93881,-0.69984 -0.35845,-0.460869 -0.51208,-1.12657 -0.13655,-0.68277 -0.13655,-1.570371 v -7.920132 l 1.58744,-0.273108 z"
         style="letter-spacing:0px;stroke-width:0.42673126"
         id="path893"
         inkscape:connector-curvature="0" />
      <path
         d="m 148.18606,54.941523 q 0.56328,0 0.99001,-0.01707 0.4438,-0.03414 0.73398,-0.102415 v -2.645734 q -0.17069,-0.08535 -0.56328,-0.136554 -0.37553,-0.06828 -0.92174,-0.06828 -0.35846,0 -0.76812,0.05121 -0.39259,0.05121 -0.73398,0.2219 -0.32431,0.153623 -0.54621,0.4438 -0.2219,0.273108 -0.2219,0.733978 0,0.853463 0.54621,1.194848 0.54622,0.324315 1.48503,0.324315 z m -0.13656,-7.95427 q 0.95588,0 1.60451,0.256039 0.6657,0.238969 1.0583,0.699839 0.40966,0.4438 0.58035,1.075363 0.17069,0.614493 0.17069,1.36554 v 5.547506 q -0.20483,0.03414 -0.58035,0.102415 -0.35845,0.05121 -0.81932,0.102416 -0.46087,0.05121 -1.00709,0.08535 -0.52915,0.05121 -1.05829,0.05121 -0.75105,0 -1.38261,-0.153623 -0.63157,-0.153624 -1.09244,-0.477939 -0.46086,-0.341385 -0.7169,-0.887601 -0.25604,-0.546216 -0.25604,-1.314333 0,-0.733977 0.29017,-1.263124 0.30725,-0.529147 0.81933,-0.853463 0.51208,-0.324315 1.19485,-0.477939 0.68277,-0.153623 1.43381,-0.153623 0.23897,0 0.49501,0.03414 0.25604,0.01707 0.47794,0.06828 0.23897,0.03414 0.40966,0.06828 0.1707,0.03414 0.23897,0.05121 v -0.4438 q 0,-0.392593 -0.0853,-0.768116 -0.0854,-0.392593 -0.30725,-0.68277 -0.2219,-0.307247 -0.61449,-0.477939 -0.37553,-0.187762 -0.99002,-0.187762 -0.78519,0 -1.38261,0.119485 -0.58035,0.102415 -0.87053,0.2219 l -0.18776,-1.314332 q 0.30724,-0.136554 1.02415,-0.256039 0.71691,-0.136554 1.5533,-0.136554 z"
         style="letter-spacing:0px;stroke-width:0.42673126"
         id="path895"
         inkscape:connector-curvature="0" />
      <path
         d="m 155.56825,47.209153 h 3.36264 v 1.331402 h -3.36264 v 4.09662 q 0,0.6657 0.10241,1.109501 0.10242,0.426731 0.30725,0.68277 0.20483,0.238969 0.51208,0.341385 0.30724,0.102415 0.7169,0.102415 0.71691,0 1.14364,-0.153623 0.4438,-0.170692 0.6145,-0.238969 l 0.30724,1.314332 q -0.23897,0.119485 -0.83639,0.290177 -0.59742,0.187762 -1.36554,0.187762 -0.90467,0 -1.50209,-0.2219 -0.58036,-0.23897 -0.93881,-0.69984 -0.35846,-0.460869 -0.51208,-1.12657 -0.13655,-0.68277 -0.13655,-1.570371 v -7.920132 l 1.58744,-0.273108 z"
         style="letter-spacing:0px;stroke-width:0.42673126"
         id="path897"
         inkscape:connector-curvature="0" />
      <path
         d="m 162.53837,56.085163 h -1.58744 v -8.87601 h 1.58744 z m -0.80226,-10.480519 q -0.42673,0 -0.73397,-0.273108 -0.29018,-0.290178 -0.29018,-0.768117 0,-0.477939 0.29018,-0.751047 0.30724,-0.290177 0.73397,-0.290177 0.42673,0 0.71691,0.290177 0.30725,0.273108 0.30725,0.751047 0,0.477939 -0.30725,0.768117 -0.29018,0.273108 -0.71691,0.273108 z"
         style="letter-spacing:0px;stroke-width:0.42673126"
         id="path899"
         inkscape:connector-curvature="0" />
      <path
         d="m 173.1197,51.647158 q 0,1.058294 -0.30725,1.911756 -0.30724,0.853463 -0.87053,1.467956 -0.54622,0.614493 -1.31433,0.955878 -0.76812,0.324315 -1.67279,0.324315 -0.90467,0 -1.67279,-0.324315 -0.76811,-0.341385 -1.3314,-0.955878 -0.54621,-0.614493 -0.85346,-1.467956 -0.30725,-0.853462 -0.30725,-1.911756 0,-1.041224 0.30725,-1.894687 0.30725,-0.870531 0.85346,-1.485024 0.56329,-0.614493 1.3314,-0.938809 0.76812,-0.341385 1.67279,-0.341385 0.90467,0 1.67279,0.341385 0.76811,0.324316 1.31433,0.938809 0.56329,0.614493 0.87053,1.485024 0.30725,0.853463 0.30725,1.894687 z m -1.65572,0 q 0,-1.502094 -0.68277,-2.372626 -0.6657,-0.887601 -1.82641,-0.887601 -1.16071,0 -1.84348,0.887601 -0.6657,0.870532 -0.6657,2.372626 0,1.502094 0.6657,2.389695 0.68277,0.870532 1.84348,0.870532 1.16071,0 1.82641,-0.870532 0.68277,-0.887601 0.68277,-2.389695 z"
         style="letter-spacing:0px;stroke-width:0.42673126"
         id="path901"
         inkscape:connector-curvature="0" />
      <path
         d="m 175.3531,47.465192 q 0.54622,-0.136554 1.45089,-0.290177 0.90467,-0.153624 2.08245,-0.153624 1.05829,0 1.75813,0.307247 0.69984,0.290177 1.1095,0.836393 0.42673,0.529147 0.59743,1.280194 0.17069,0.751047 0.17069,1.655717 v 4.984221 h -1.58744 v -4.642836 q 0,-0.819324 -0.11949,-1.399678 -0.10241,-0.580355 -0.35845,-0.938809 -0.25604,-0.358454 -0.68277,-0.512078 -0.42673,-0.170692 -1.05829,-0.170692 -0.25604,0 -0.52915,0.01707 -0.27311,0.01707 -0.52915,0.05121 -0.23897,0.01707 -0.4438,0.05121 -0.18776,0.03414 -0.27311,0.05121 v 7.493401 h -1.58744 z"
         style="letter-spacing:0px;stroke-width:0.42673126"
         id="path903"
         inkscape:connector-curvature="0" />
    </g>
  </g>
</svg>

`
