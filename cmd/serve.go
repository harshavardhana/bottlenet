package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/minio/bottlenet/pkg/perf"
)

func newClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 10 * time.Second,
				DualStack: true,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

func serveBottlenet(ctx context.Context, mux *http.ServeMux) error {
	defaultMux := mux
	if mux == nil {
		defaultMux = http.NewServeMux()
	}
	defaultMux.HandleFunc("/perf", listenPerf(ctx))
	defaultMux.HandleFunc("/dispatch", listenDispatch(ctx))

	server := http.Server{
		Addr:    fmt.Sprintf(":%d", bottlenetPort),
		Handler: defaultMux,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}
	done := make(chan error, 1)
	go func() {
		errChan := make(chan error, 1)
		go func() {
			errChan <- server.ListenAndServe()
		}()
		for {
			select {
			case err := <-errChan:
				fmt.Println(err)
				done <- server.Shutdown(ctx)
			case <-ctx.Done():
				done <- server.Shutdown(ctx)
			}
		}
	}()
	return <-done
}

func doDispatch(ctx context.Context, addr string, remotes []*node) error {
	client := newClient()

	jsonData, err := json.Marshal(remotes)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://%s:7007/%s", addr, "dispatch"), bytes.NewReader(jsonData))
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	//	if resp.StatusCode != http.StatusOK {
	//	return fmt.Errorf("failed to dispatch bottlenet to %s", addr)
	//}

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	//fmt.Println(string(respBody))
	
	return json.Unmarshal(respBody, &remotes)
}

func listenDispatch(ctx context.Context) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			return
		}

		p := &[]*node{}
		if err := json.Unmarshal(body, p); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			return
		}

		for _, px := range *p {
			if err := doPerf(ctx, px); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(err.Error()))
				return
			}
		}
		respBody, err := json.Marshal(p)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(respBody)
	}
}

func doPerf(ctx context.Context, p *node) error {
	info, err := flood(ctx, p.Addr)
	if err != nil {
		return err
	}
	if p.Perf == nil {
		p.Perf = map[string]perf.Perf{}
	}
	p.Perf[p.Addr] = info
	return nil
}

func listenPerf(ctx context.Context) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		// Use this trailer to send additional headers after sending body
		w.Header().Set("Trailer", "FinalStatus")

		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)

		copyContext := func(ctx context.Context, dst io.Writer, src io.Reader) (written int64, err error) {
			size := 32 * 1024
			if l, ok := src.(*io.LimitedReader); ok && int64(size) > l.N {
				if l.N < 1 {
					size = 1
				} else {
					size = int(l.N)
				}
			}
			buf := make([]byte, size)

		loop:
			for {
				select {
				case <-ctx.Done():
					return written, ctx.Err()
				default:
					// If the reader has a WriteTo method, use it to do the copy.
					// Avoids an allocation and a copy.
					if wt, ok := src.(io.WriterTo); ok {
						return wt.WriteTo(dst)
					}

					// Similarly, if the writer has a ReadFrom method, use it to do the copy.
					if rt, ok := dst.(io.ReaderFrom); ok {
						return rt.ReadFrom(src)
					}
					nr, er := src.Read(buf)
					if nr > 0 {
						nw, ew := dst.Write(buf[0:nr])
						if nw > 0 {
							written += int64(nw)
						}
						if ew != nil {
							err = ew
							break loop
						}
						if nr != nw {
							err = io.ErrShortWrite
							break loop
						}
					}
					if er != nil {
						if er != io.EOF {
							err = er
						}
						break loop
					}
				}
			}
			return written, err
		}

		n, err := copyContext(ctx, ioutil.Discard, r.Body)
		if err == io.ErrUnexpectedEOF {
			w.Header().Set("FinalStatus", err.Error())
			return
		}
		if err != nil && err != io.EOF {
			w.Header().Set("FinalStatus", err.Error())
			return
		}
		if n != r.ContentLength {
			err := fmt.Errorf("bottlenet: short read: expected %d found %d", r.ContentLength, n)
			w.Header().Set("FinalStatus", err.Error())
			return
		}
		w.Header().Set("FinalStatus", "Success")
		w.(http.Flusher).Flush()
	}
}

func doFlood(ctx context.Context, remote string, dataSize int64, threadCount uint) (info perf.Perf, err error) {
	latencies := []float64{}
	throughputs := []float64{}

	buf := make([]byte, dataSize)

	buflimiter := make(chan struct{}, threadCount)
	errChan := make(chan error, threadCount)

	totalTransferred := int64(0)
	transferChan := make(chan int64, threadCount)

	client := newClient()

	go func() {
		for v := range transferChan {
			atomic.AddInt64(&totalTransferred, v)
		}
	}()

	// ensure enough samples to obtain normal distribution
	maxSamples := int(10 * threadCount)

	innerCtx, cancel := context.WithCancel(ctx)

	slowSamples := int32(0)
	maxSlowSamples := int32(maxSamples / 20)
	slowSample := func() {
		if slowSamples > maxSlowSamples { // 5% of total
			return
		}
		if atomic.AddInt32(&slowSamples, 1) >= maxSlowSamples {
			errChan <- networkOverloaded
			cancel()
		}
	}

	wg := sync.WaitGroup{}
	finish := func() {
		<-buflimiter
		wg.Done()
	}

loop:
	for i := 0; i < maxSamples; i++ {
		select {
		case <-ctx.Done():
			return info, ctx.Err()
		case err = <-errChan:
			break loop
		case buflimiter <- struct{}{}:
			wg.Add(1)

			if innerCtx.Err() != nil {
				finish()
				continue
			}

			go func(i int) {
				bufReader := bytes.NewReader(buf)
				bufReadCloser := ioutil.NopCloser(&progressReader{
					r:            bufReader,
					progressChan: transferChan,
				})
				start := time.Now()
				before := atomic.LoadInt64(&totalTransferred)

				ctx, cancel := context.WithTimeout(innerCtx, 10*time.Second)
				defer cancel()

				req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://%s:7007/%s", remote, "perf"), bufReadCloser)
				if err != nil {
					errChan <- err
					finish()
					return
				}
				req.ContentLength = dataSize
				resp, err := client.Do(req) //peerRESTMethodNetOBDInfo, nil, bufReadCloser, dataSize)
				if err != nil {
					if err == context.DeadlineExceeded {
						slowSample()
						finish()
						return
					}
					finish()
					errChan <- err
					return
				}

				defer resp.Body.Close()
				io.Copy(ioutil.Discard, resp.Body)

				after := atomic.LoadInt64(&totalTransferred)
				finish()
				end := time.Now()

				latency := float64(end.Sub(start).Seconds())

				if latency > maxLatencyForSizeThreads(dataSize, threadCount) {
					slowSample()
				}

				/* Throughput = (total data transferred across all threads / time taken) */
				throughput := float64(float64((after - before)) / latency)

				latencies = append(latencies, latency)
				throughputs = append(throughputs, throughput)
			}(i)
		}
	}
	wg.Wait()

	if err != nil {
		return info, err
	}

	return perf.ComputePerf(latencies, throughputs)
}

func maxLatencyForSizeThreads(size int64, threadCount uint) float64 {
	Gbit100 := 12.5 * float64(humanize.GiByte)
	Gbit40 := 5.00 * float64(humanize.GiByte)
	Gbit25 := 3.25 * float64(humanize.GiByte)
	Gbit10 := 1.25 * float64(humanize.GiByte)
	// Gbit1 := 0.25 * float64(humanize.GiByte)

	// Given the current defaults, each combination of size/thread
	// is supposed to fully saturate the intended pipe when all threads are active
	// i.e. if the test is performed in a perfectly controlled environment, i.e. without
	// CPU scheduling latencies and/or network jitters, then all threads working
	// simultaneously should result in each of them completing in 1s
	//
	// In reality, I've assumed a normal distribution of latency with expected mean of 1s and min of 0s
	// Then, 95% of threads should complete within 2 seconds (2 std. deviations from the mean). The 2s comes
	// from fitting the normal curve such that the mean is 1.
	//
	// i.e. we expect that no more than 5% of threads to take longer than 2s to push the data.
	//
	// throughput  |  max latency
	//   100 Gbit  |  2s
	//    40 Gbit  |  2s
	//    25 Gbit  |  2s
	//    10 Gbit  |  2s
	//     1 Gbit  |  inf

	throughput := float64(int64(size) * int64(threadCount))
	if throughput >= Gbit100 {
		return 2.0
	} else if throughput >= Gbit40 {
		return 2.0
	} else if throughput >= Gbit25 {
		return 2.0
	} else if throughput >= Gbit10 {
		return 2.0
	}
	return math.MaxFloat64
}

func flood(ctx context.Context, remote string) (info perf.Perf, err error) {

	// 100 Gbit ->  256 MiB  *  50 threads
	// 40 Gbit  ->  256 MiB  *  20 threads
	// 25 Gbit  ->  128 MiB  *  25 threads
	// 10 Gbit  ->  128 MiB  *  10 threads
	// 1 Gbit   ->  64  MiB  *  2  threads

	type step struct {
		size    int64
		threads uint
	}
	steps := []step{
		{ // 100 Gbit
			size:    256 * humanize.MiByte,
			threads: 50,
		},
		{ // 40 Gbit
			size:    256 * humanize.MiByte,
			threads: 20,
		},
		{ // 25 Gbit
			size:    128 * humanize.MiByte,
			threads: 25,
		},
		{ // 10 Gbit
			size:    128 * humanize.MiByte,
			threads: 10,
		},
		{ // 1 Gbit
			size:    64 * humanize.MiByte,
			threads: 2,
		},
	}

	for i := range steps {
		size := steps[i].size
		threads := steps[i].threads

		if info, err = doFlood(ctx, remote, size, threads); err != nil {
			if err == networkOverloaded {
				continue
			}
			if err == context.DeadlineExceeded {
				continue
			}
			if err == context.Canceled {
				continue
			}
		}
		return info, err
	}
	return info, err
}