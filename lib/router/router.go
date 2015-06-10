package router

import (
	"bytes"
	"errors"
	"fmt"
	"hash/adler32"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"

	"code.google.com/p/go-uuid/uuid"
	"github.com/pachyderm/pfs/lib/etcache"
)

var NoHosts = errors.New("No hosts found")

func HashResource(resource string) uint64 {
	return uint64(adler32.Checksum([]byte(resource)))
}

// Parse a string descriving a shard, the string looks like: "0-4"
func ParseShard(shardDesc string) (uint64, uint64, error) {
	s_m := strings.Split(shardDesc, "-")
	shard, err := strconv.ParseUint(s_m[0], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	modulos, err := strconv.ParseUint(s_m[1], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	return shard, modulos, nil
}

// Match returns true of a resource hashes to the given shard.
func Match(resource, shardDesc string) (bool, error) {
	shard, modulos, err := ParseShard(shardDesc)
	if err != nil {
		return false, err
	}
	return (HashResource(resource) % modulos) == shard, nil
}

func hashRequest(r *http.Request) uint64 {
	return HashResource(r.URL.Path)
}

func Route(r *http.Request, etcdKey string, modulos uint64) (io.ReadCloser, error) {
	bucket := hashRequest(r) % modulos
	shard := fmt.Sprint(bucket, "-", fmt.Sprint(modulos))

	_master, err := etcache.Get(path.Join(etcdKey, shard), false, false)
	if err != nil {
		return nil, err
	}
	master := _master.Node.Value

	httpClient := &http.Client{}
	// `Do` will complain if r.RequestURI is set so we unset it
	r.RequestURI = ""
	r.URL.Scheme = "http"
	r.URL.Host = strings.TrimPrefix(master, "http://")
	resp, err := httpClient.Do(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Failed request (%s) to %s.", resp.Status, r.URL.String())
	}
	return resp.Body, nil
}

func RouteHttp(w http.ResponseWriter, r *http.Request, etcdKey string, modulos uint64) {
	reader, err := Route(r, etcdKey, modulos)
	if err != nil {
		http.Error(w, err.Error(), 500)
		log.Print(err)
		return
	}
	_, err = io.Copy(w, reader)
	if err != nil {
		http.Error(w, err.Error(), 500)
		log.Print(err)
		return
	}
}

type multiReadCloser struct {
	readers []io.ReadCloser
}

func (mr *multiReadCloser) Read(p []byte) (n int, err error) {
	for len(mr.readers) > 0 {
		n, err = mr.readers[0].Read(p)
		if n > 0 || err != io.EOF {
			if err == io.EOF {
				// Don't return EOF yet. There may be more bytes
				// in the remaining readers.
				err = nil
			}
			return
		}
		err = mr.readers[0].Close()
		if err != nil {
			return
		}
		mr.readers = mr.readers[1:]
	}
	return 0, io.EOF
}

func (mr *multiReadCloser) Close() error {
	for len(mr.readers) > 0 {
		err := mr.readers[0].Close()
		if err != nil {
			return err
		}
		mr.readers = mr.readers[1:]
	}
	return nil
}

// MultiReadCloser returns a ReaderCloser that's the logical concatenation of
// the provided input readers. They're read sequentially.  Once all inputs have
// returned EOF, Read will return EOF. If any of the readers return a non-nil,
// non-EOF error, Read will return that error.  MultiReadCloser closes all of
// the input readers when it is closed. It also closes readers when they finish.
func MultiReadCloser(readers ...io.ReadCloser) io.ReadCloser {
	r := make([]io.ReadCloser, len(readers))
	copy(r, readers)
	return &multiReadCloser{r}
}

// Multicast enables the Ogre Magi to rapidly cast his spells, giving them
// greater potency.
// Multicast sends a request to every host it finds under a key and returns a
// ReadCloser for each one.
func Multicast(r *http.Request, etcdKey string) ([]*http.Response, error) {
	_endpoints, err := etcache.Get(etcdKey, false, true)
	if err != nil {
		return nil, err
	}
	endpoints := _endpoints.Node.Nodes
	if len(endpoints) == 0 {
		return nil, NoHosts
	}

	// If the request has a body we need to store it in memory because it needs
	// to be sent to multiple endpoints and Reader (the type of r.Body) is
	// single use.
	var body []byte
	if r.ContentLength != 0 {
		body, err = ioutil.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
	}

	var resps []*http.Response
	errors := make(chan error, len(endpoints))
	var lock sync.Mutex
	var wg sync.WaitGroup
	wg.Add(len(endpoints))
	for _, node := range endpoints {
		//go func(node *etcd.Node) {
		wg.Done()
		httpClient := &http.Client{}
		// `Do` will complain if r.RequestURI is set so we unset it
		r.RequestURI = ""
		r.URL.Scheme = "http"
		r.URL.Host = strings.TrimPrefix(node.Value, "http://")
		log.Print(r.URL.String())
		if err != nil {
			//errors <- err
			return nil, err
		}

		if r.ContentLength != 0 {
			r.Body = ioutil.NopCloser(bytes.NewReader(body))
		}

		resp, err := httpClient.Do(r)
		if err != nil {
			//errors <- err
			return nil, err
		}
		if resp.StatusCode != 200 {
			//errors <- fmt.Errorf("Failed request (%s) to %s.", resp.Status, r.URL.String())
			return nil, err
		}
		lock.Lock()
		resps = append(resps, resp)
		lock.Unlock()
		//}(node)
	}
	wg.Wait()
	close(errors)

	for err := range errors {
		if err != nil {
			return nil, err
		}
	}

	return resps, nil
}

type ForwardingPolicy int

const (
	// ReturnOne makes MulticastHttp return only the first response
	ReturnOne ForwardingPolicy = iota
	// ReturnAll makes MulticastHttp return all the responses
	ReturnAll ForwardingPolicy = iota
)

// MulticastHttp sends r to every host it finds under etcdKey, then prints the
// response to w based on
func MulticastHttp(w http.ResponseWriter, r *http.Request, etcdKey string, f ForwardingPolicy) {
	resps, err := Multicast(r, etcdKey)
	if err != nil {
		http.Error(w, err.Error(), 500)
		log.Print(err)
		return
	}
	var readers []io.ReadCloser
	for _, resp := range resps {
		readers = append(readers, resp.Body)
	}
	var reader io.ReadCloser
	switch f {
	case ReturnOne:
		reader = readers[0]
	case ReturnAll:
		reader = MultiReadCloser(readers...)
	default:
		http.Error(w, "Internal error.", 500)
		log.Print("Invalid ForwardingPolicy (programmer error)")
		return
	}
	defer reader.Close() //this line will close all of the readers

	_, err = io.Copy(w, reader)
	if err != nil {
		http.Error(w, err.Error(), 500)
		log.Print(err)
		return
	}
}

type Router struct {
	modulos uint64
}

func NewRouter(modulos uint64) *Router {
	return &Router{
		modulos: modulos,
	}
}

func RouterFromArgs() (*Router, error) {
	modulos, err := strconv.ParseUint(os.Args[1], 10, 32)

	if err != nil {
		return nil, err
	}
	return NewRouter(modulos), nil
}

func (ro *Router) RouterMux() *http.ServeMux {
	mux := http.NewServeMux()

	fileHandler := func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "*") {
			MulticastHttp(w, r, "/pfs/master", ReturnAll)
		} else {
			RouteHttp(w, r, "/pfs/master", ro.modulos)
		}
	}
	commitHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			values := r.URL.Query()
			values.Add("commit", uuid.New())
			r.URL.RawQuery = values.Encode()
		}
		MulticastHttp(w, r, "/pfs/master", ReturnOne)
	}
	branchHandler := func(w http.ResponseWriter, r *http.Request) {
		MulticastHttp(w, r, "/pfs/master", ReturnOne)
	}
	jobHandler := func(w http.ResponseWriter, r *http.Request) {
		MulticastHttp(w, r, "/pfs/master", ReturnOne)
	}
	pipelineHandler := func(w http.ResponseWriter, r *http.Request) {
		MulticastHttp(w, r, "/pfs/master", ReturnOne)
	}

	mux.HandleFunc("/file/", fileHandler)
	mux.HandleFunc("/commit", commitHandler)
	mux.HandleFunc("/branch", branchHandler)
	mux.HandleFunc("/job/", jobHandler)
	mux.HandleFunc("/pipeline/", pipelineHandler)
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "pong\n") })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Welcome to pfs!\n")
	})

	return mux
}

func (r *Router) RunServer() {
	http.ListenAndServe(":80", r.RouterMux())
}