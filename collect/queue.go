package collect

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"bosun.org/metadata"
	"bosun.org/opentsdb"
	"bosun.org/slog"
	"github.com/GROpenSourceDev/go-ntlm-auth/ntlm"
	"net/http/httptest"
)

func queuer() {
	for dp := range tchan {
		if err := dp.Clean(); err != nil {
			atomic.AddInt64(&discarded, 1)
			continue // if anything gets this far that can't be made valid, just drop it silently.
		}
		qlock.Lock()
		for {
			if len(queue) > MaxQueueLen {
				atomic.AddInt64(&dropped, 1)
				break
			}
			queue = append(queue, dp)
			select {
			case dp = <-tchan:
				if err := dp.Clean(); err != nil {
					atomic.AddInt64(&discarded, 1)
					break // if anything gets this far that can't be made valid, just drop it silently.
				}
				continue
			default:
			}
			break
		}
		qlock.Unlock()
	}
}

// Locks the queue and sends all datapoints. Intended to be used as scollector exits.
func Flush() {
	flushData()
	metadata.FlushMetadata()
	qlock.Lock()
	for len(queue) > 0 {
		i := len(queue)
		if i > BatchSize {
			i = BatchSize
		}
		sending := queue[:i]
		queue = queue[i:]
		if Debug {
			slog.Infof("sending: %d, remaining: %d", i, len(queue))
		}
		sendBatch(sending)
	}
	qlock.Unlock()
}

func send() {
	for {
		qlock.Lock()
		if i := len(queue); i > 0 {
			if i > BatchSize {
				i = BatchSize
			}
			sending := queue[:i]
			queue = queue[i:]
			if Debug {
				slog.Infof("sending: %d, remaining: %d", i, len(queue))
			}
			qlock.Unlock()
			if DisableDefaultCollectors == false {
				Sample("collect.post.batchsize", Tags, float64(len(sending)))
			}
			sendBatch(sending)
		} else {
			qlock.Unlock()
			time.Sleep(time.Second)
		}
	}
}

func sendBatch(batch []*opentsdb.DataPoint) {
	if Print {
		for _, d := range batch {
			j, err := d.MarshalJSON()
			if err != nil {
				slog.Error(err)
			}
			slog.Info(string(j))
		}
		recordSent(len(batch))
		return
	}
	now := time.Now()
	resp, err := SendDataPoints(batch, tsdbURL)
	if err == nil {
		slog.Info("Called SDP, err is nil")
		defer resp.Body.Close()
	} else {
		slog.Info("Called SDP, err is not nil")
	}
	d := time.Since(now).Nanoseconds() / 1e6
	Sample("collect.post.duration", Tags, float64(d))
	Add("collect.post.total_duration", Tags, d)
	Add("collect.post.count", Tags, 1)
	// Some problem with connecting to the server; retry later.
	if err != nil || resp.StatusCode != http.StatusNoContent {
		slog.Info("Either nil or statsnocontent")
		if err != nil {
			slog.Info("Error nil")
			Add("collect.post.error", Tags, 1)
			slog.Error(err)
		} else if resp.StatusCode != http.StatusNoContent {
			slog.Info("Status code is not no content " + string(resp.StatusCode))
			Add("collect.post.bad_status", Tags, 1)
			slog.Errorln(resp.Status)
			body, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				slog.Error(err)
			}
			if len(body) > 0 {
				slog.Error(string(body))
			}
		}
		restored := 0
		for _, msg := range batch {
			restored++
			tchan <- msg
		}
		d := time.Second * 5
		Add("collect.post.restore", Tags, int64(restored))
		slog.Infof("restored %d, sleeping %s", restored, d)
		time.Sleep(d)
		return
	}
	// Drain up to 512 bytes so the Transport can reuse the connection when it is closed
	io.CopyN(ioutil.Discard, resp.Body, 512)
	recordSent(len(batch))
}

func recordSent(num int) {
	if Debug {
		slog.Infoln("sent", num)
	}
	slock.Lock()
	sent += int64(num)
	slock.Unlock()
}

var bufferPool = sync.Pool{
	New: func() interface{} { return &bytes.Buffer{} },
}

func SendDataPoints(dps []*opentsdb.DataPoint, tsdb string) (*http.Response, error) {
	req, err := buildHTTPRequest(dps, tsdb)
	if err != nil {
		return nil, err
	}
	slog.Info("Q url " + tsdb)
	if DirectHandler != nil {
		slog.Info("Q Direct handler is set " + req.URL.String())
		rec := httptest.NewRecorder()
		//TODO this proxy does not set the Auth header.
		DirectHandler.ServeHTTP(rec, req)
		slog.Info("Q Direct Handler called " + req.URL.String())
		return rec.Result(), nil
	} else {
		slog.Info("Q Direct handler is null")
	}
	client := DefaultClient

	if UseNtlm {
		resp, err := ntlm.DoNTLMRequest(client, req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == 401 {
			slog.Errorf("Scollector unauthorized to post data points to tsdb. Terminating.")
			os.Exit(1)
		}
		return resp, err
	}
	slog.Info("Calling client.DO")
	resp, err := client.Do(req)
	return resp, err
}

func buildHTTPRequest(dps []*opentsdb.DataPoint, tsdb string) (*http.Request, error) {
	buf := bufferPool.Get().(*bytes.Buffer)
	defer bufferPool.Put(buf)
	buf.Reset()
	g := gzip.NewWriter(buf)
	if err := json.NewEncoder(g).Encode(dps); err != nil {
		return nil, err
	}
	if err := g.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", tsdb, buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	if AuthToken != "" {
		req.Header.Set("X-Access-Token", AuthToken)
	}
	Add("collect.post.total_bytes", Tags, int64(buf.Len()))
	return req, nil
}
