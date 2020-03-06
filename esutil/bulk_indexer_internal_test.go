// Licensed to Elasticsearch B.V. under one or more agreements.
// Elasticsearch B.V. licenses this file to you under the Apache 2.0 License.
// See the LICENSE file in the project root for more information.

// +build !integration

package esutil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esapi"
	"github.com/elastic/go-elasticsearch/v8/estransport"
)

var defaultRoundTripFunc = func(*http.Request) (*http.Response, error) {
	return &http.Response{Body: ioutil.NopCloser(strings.NewReader(`{}`))}, nil
}

type mockTransport struct {
	RoundTripFunc func(*http.Request) (*http.Response, error)
}

func (t *mockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.RoundTripFunc == nil {
		return defaultRoundTripFunc(req)
	}
	return t.RoundTripFunc(req)
}

func TestBulkIndexer(t *testing.T) {
	t.Run("Basic", func(t *testing.T) {
		var (
			wg sync.WaitGroup

			countReqs int
			testfile  string
			numItems  = 6
		)

		es, _ := elasticsearch.NewClient(elasticsearch.Config{Transport: &mockTransport{
			RoundTripFunc: func(*http.Request) (*http.Response, error) {
				countReqs++
				switch countReqs {
				case 1:
					testfile = "testdata/bulk_response_1a.json"
				case 2:
					testfile = "testdata/bulk_response_1b.json"
				case 3:
					testfile = "testdata/bulk_response_1c.json"
				}
				bodyContent, _ := ioutil.ReadFile(testfile)
				return &http.Response{Body: ioutil.NopCloser(bytes.NewBuffer(bodyContent))}, nil
			},
		}})

		cfg := BulkIndexerConfig{
			NumWorkers:    1,
			FlushBytes:    50,
			FlushInterval: time.Hour, // Disable auto-flushing, because response doesn't match number of items
			Client:        es}
		if os.Getenv("DEBUG") != "" {
			cfg.DebugLogger = log.New(os.Stdout, "", 0)
		}

		bi, _ := NewBulkIndexer(cfg)

		for i := 1; i <= numItems; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				err := bi.Add(context.Background(), BulkIndexerItem{
					Action:     "foo",
					DocumentID: strconv.Itoa(i),
					Body:       strings.NewReader(fmt.Sprintf(`{"title":"foo-%d"}`, i)),
				})
				if err != nil {
					t.Fatalf("Unexpected error: %s", err)
				}
			}(i)
		}
		wg.Wait()

		if err := bi.Close(context.Background()); err != nil {
			t.Errorf("Unexpected error: %s", err)
		}

		stats := bi.Stats()

		// added = numitems
		if stats.NumAdded != uint(numItems) {
			t.Errorf("Unexpected NumAdded: want=%d, got=%d", numItems, stats.NumAdded)
		}

		// flushed = numitems - 1x conflict + 1x not_found
		if stats.NumFlushed != uint(numItems-2) {
			t.Errorf("Unexpected NumFlushed: want=%d, got=%d", numItems-2, stats.NumFlushed)
		}

		// failed = 1x conflict + 1x not_found
		if stats.NumFailed != 2 {
			t.Errorf("Unexpected NumFailed: want=%d, got=%d", 2, stats.NumFailed)
		}

		// indexed = 1x
		if stats.NumIndexed != 1 {
			t.Errorf("Unexpected NumIndexed: want=%d, got=%d", 1, stats.NumIndexed)
		}

		// created = 1x
		if stats.NumCreated != 1 {
			t.Errorf("Unexpected NumCreated: want=%d, got=%d", 1, stats.NumCreated)
		}

		// deleted = 1x
		if stats.NumDeleted != 1 {
			t.Errorf("Unexpected NumDeleted: want=%d, got=%d", 1, stats.NumDeleted)
		}

		if stats.NumUpdated != 1 {
			t.Errorf("Unexpected NumUpdated: want=%d, got=%d", 1, stats.NumUpdated)
		}

		// 3 items * 40 bytes, 2 workers, 1 request per worker
		if stats.NumRequests != 3 {
			t.Errorf("Unexpected NumRequests: want=%d, got=%d", 3, stats.NumRequests)
		}
	})

	t.Run("Add() Timeout", func(t *testing.T) {
		es, _ := elasticsearch.NewClient(elasticsearch.Config{Transport: &mockTransport{}})
		bi, _ := NewBulkIndexer(BulkIndexerConfig{NumWorkers: 1, Client: es})
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()

		var errs []error
		for i := 0; i < 10; i++ {
			errs = append(errs, bi.Add(ctx, BulkIndexerItem{Action: "delete", DocumentID: "timeout"}))
		}
		if err := bi.Close(context.Background()); err != nil {
			t.Errorf("Unexpected error: %s", err)
		}

		var gotError bool
		for _, err := range errs {
			if err != nil && err.Error() == "context deadline exceeded" {
				gotError = true
			}
		}
		if !gotError {
			t.Errorf("Expected timeout error, but none in: %s", errs)
		}
	})

	t.Run("Indexer Callback", func(t *testing.T) {
		esCfg := elasticsearch.Config{
			Transport: &mockTransport{
				RoundTripFunc: func(*http.Request) (*http.Response, error) {
					return nil, fmt.Errorf("Mock transport error")
				},
			},
		}
		if os.Getenv("DEBUG") != "" {
			esCfg.Logger = &estransport.ColorLogger{
				Output:             os.Stdout,
				EnableRequestBody:  true,
				EnableResponseBody: true,
			}
		}

		es, _ := elasticsearch.NewClient(esCfg)

		var indexerError error
		biCfg := BulkIndexerConfig{
			NumWorkers: 1,
			Client:     es,
			OnError:    func(err error) { indexerError = err },
		}
		if os.Getenv("DEBUG") != "" {
			biCfg.DebugLogger = log.New(os.Stdout, "", 0)
		}

		bi, _ := NewBulkIndexer(biCfg)

		if err := bi.Add(context.Background(), BulkIndexerItem{
			Action: "foo",
		}); err != nil {
			t.Fatalf("Unexpected error: %s", err)
		}

		bi.Close(context.Background())

		if indexerError == nil {
			t.Errorf("Expected indexerError to not be nil")
		}
	})

	t.Run("Item Callbacks", func(t *testing.T) {
		var (
			countSuccessful uint64
			countFailed     uint64
			failedIDs       []string

			numItems       = 4
			numFailed      = 2
			bodyContent, _ = ioutil.ReadFile("testdata/bulk_response_2.json")
		)

		es, _ := elasticsearch.NewClient(elasticsearch.Config{Transport: &mockTransport{
			RoundTripFunc: func(*http.Request) (*http.Response, error) {
				return &http.Response{Body: ioutil.NopCloser(bytes.NewBuffer(bodyContent))}, nil
			},
		}})

		cfg := BulkIndexerConfig{NumWorkers: 1, Client: es}
		if os.Getenv("DEBUG") != "" {
			cfg.DebugLogger = log.New(os.Stdout, "", 0)
		}

		bi, _ := NewBulkIndexer(cfg)

		successFunc := func(item BulkIndexerItem, res BulkIndexerResponseItem) {
			atomic.AddUint64(&countSuccessful, 1)
		}
		failureFunc := func(item BulkIndexerItem, res BulkIndexerResponseItem, err error) {
			atomic.AddUint64(&countFailed, 1)
			failedIDs = append(failedIDs, item.DocumentID)
		}

		if err := bi.Add(context.Background(), BulkIndexerItem{
			Action:     "index",
			DocumentID: "1",
			Body:       strings.NewReader(`{"title":"foo"}`),
			OnSuccess:  successFunc,
			OnFailure:  failureFunc,
		}); err != nil {
			t.Fatalf("Unexpected error: %s", err)
		}

		if err := bi.Add(context.Background(), BulkIndexerItem{
			Action:     "create",
			DocumentID: "1",
			Body:       strings.NewReader(`{"title":"bar"}`),
			OnSuccess:  successFunc,
			OnFailure:  failureFunc,
		}); err != nil {
			t.Fatalf("Unexpected error: %s", err)
		}

		if err := bi.Add(context.Background(), BulkIndexerItem{
			Action:     "delete",
			DocumentID: "2",
			OnSuccess:  successFunc,
			OnFailure:  failureFunc,
		}); err != nil {
			t.Fatalf("Unexpected error: %s", err)
		}

		if err := bi.Add(context.Background(), BulkIndexerItem{
			Action:     "update",
			DocumentID: "3",
			Body:       strings.NewReader(`{"doc":{"title":"qux"}}`),
			OnSuccess:  successFunc,
			OnFailure:  failureFunc,
		}); err != nil {
			t.Fatalf("Unexpected error: %s", err)
		}

		if err := bi.Close(context.Background()); err != nil {
			t.Errorf("Unexpected error: %s", err)
		}

		stats := bi.Stats()

		if stats.NumAdded != uint(numItems) {
			t.Errorf("Unexpected NumAdded: %d", stats.NumAdded)
		}

		// Two failures are expected:
		//
		// * Operation #2: document can't be created, because a document with the same ID already exists.
		// * Operation #3: document can't be deleted, because it doesn't exist.

		if stats.NumFailed != uint(numFailed) {
			t.Errorf("Unexpected NumFailed: %d", stats.NumFailed)
		}

		if stats.NumFlushed != 2 {
			t.Errorf("Unexpected NumFailed: %d", stats.NumFailed)
		}

		if stats.NumIndexed != 1 {
			t.Errorf("Unexpected NumIndexed: %d", stats.NumIndexed)
		}

		if stats.NumUpdated != 1 {
			t.Errorf("Unexpected NumUpdated: %d", stats.NumUpdated)
		}

		if countSuccessful != uint64(numItems-numFailed) {
			t.Errorf("Unexpected countSuccessful: %d", countSuccessful)
		}

		if countFailed != uint64(numFailed) {
			t.Errorf("Unexpected countFailed: %d", countFailed)
		}

		if !reflect.DeepEqual(failedIDs, []string{"1", "2"}) {
			t.Errorf("Unexpected failedIDs: %#v", failedIDs)
		}
	})

	t.Run("Automatic flush", func(t *testing.T) {
		es, _ := elasticsearch.NewClient(elasticsearch.Config{Transport: &mockTransport{
			RoundTripFunc: func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Body:       ioutil.NopCloser(strings.NewReader(`{"items":[{"index": {}}]}`))}, nil
			},
		}})

		cfg := BulkIndexerConfig{
			NumWorkers:    1,
			Client:        es,
			FlushInterval: 100 * time.Millisecond, // Decrease the flush timeout
		}
		if os.Getenv("DEBUG") != "" {
			cfg.DebugLogger = log.New(os.Stdout, "", 0)
		}

		bi, _ := NewBulkIndexer(cfg)

		bi.Add(context.Background(),
			BulkIndexerItem{Action: "index", Body: strings.NewReader(`{"title":"foo"}`)})

		// Allow some time for auto-flush to kick in
		time.Sleep(150 * time.Millisecond)

		stats := bi.Stats()
		expected := uint(1)

		if stats.NumAdded != expected {
			t.Errorf("Unexpected NumAdded: want=%d, got=%d", expected, stats.NumAdded)
		}

		if stats.NumFailed != 0 {
			t.Errorf("Unexpected NumFailed: want=%d, got=%d", 0, stats.NumFlushed)
		}

		if stats.NumFlushed != expected {
			t.Errorf("Unexpected NumFlushed: want=%d, got=%d", expected, stats.NumFlushed)
		}

		if stats.NumIndexed != expected {
			t.Errorf("Unexpected NumIndexed: want=%d, got=%d", expected, stats.NumIndexed)
		}

		// Wait some time before closing the indexer to clear the timer
		time.Sleep(200 * time.Millisecond)
		bi.Close(context.Background())
	})

	t.Run("TooManyRequests", func(t *testing.T) {
		var (
			wg sync.WaitGroup

			countReqs int
			numItems  = 2
		)

		esCfg := elasticsearch.Config{
			Transport: &mockTransport{
				RoundTripFunc: func(*http.Request) (*http.Response, error) {
					countReqs++
					if countReqs <= 4 {
						return &http.Response{
							StatusCode: http.StatusTooManyRequests,
							Status:     "429 TooManyRequests",
							Body:       ioutil.NopCloser(strings.NewReader(`{"took":1}`))}, nil
					}
					bodyContent, _ := ioutil.ReadFile("testdata/bulk_response_1c.json")
					return &http.Response{
						StatusCode: http.StatusOK,
						Status:     "200 OK",
						Body:       ioutil.NopCloser(bytes.NewBuffer(bodyContent)),
					}, nil
				},
			},

			MaxRetries:    5,
			RetryOnStatus: []int{502, 503, 504, 429},
			RetryBackoff: func(i int) time.Duration {
				if os.Getenv("DEBUG") != "" {
					fmt.Printf("*** Retry #%d\n", i)
				}
				return time.Duration(i) * 100 * time.Millisecond
			},
		}
		if os.Getenv("DEBUG") != "" {
			esCfg.Logger = &estransport.ColorLogger{Output: os.Stdout}
		}
		es, _ := elasticsearch.NewClient(esCfg)

		biCfg := BulkIndexerConfig{NumWorkers: 1, FlushBytes: 50, Client: es}
		if os.Getenv("DEBUG") != "" {
			biCfg.DebugLogger = log.New(os.Stdout, "", 0)
		}

		bi, _ := NewBulkIndexer(biCfg)

		for i := 1; i <= numItems; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				err := bi.Add(context.Background(), BulkIndexerItem{
					Action: "foo",
					Body:   strings.NewReader(`{"title":"foo"}`),
				})
				if err != nil {
					t.Fatalf("Unexpected error: %s", err)
				}
			}(i)
		}
		wg.Wait()

		if err := bi.Close(context.Background()); err != nil {
			t.Errorf("Unexpected error: %s", err)
		}

		stats := bi.Stats()

		if stats.NumAdded != uint(numItems) {
			t.Errorf("Unexpected NumAdded: want=%d, got=%d", numItems, stats.NumAdded)
		}

		if stats.NumFlushed != uint(numItems) {
			t.Errorf("Unexpected NumFlushed: want=%d, got=%d", numItems, stats.NumFlushed)
		}

		if stats.NumFailed != 0 {
			t.Errorf("Unexpected NumFailed: want=%d, got=%d", 0, stats.NumFailed)
		}

		// Stats don't include the retries in client
		if stats.NumRequests != 1 {
			t.Errorf("Unexpected NumRequests: want=%d, got=%d", 3, stats.NumRequests)
		}
	})

	t.Run("Custom JSON Decoder", func(t *testing.T) {
		es, _ := elasticsearch.NewClient(elasticsearch.Config{Transport: &mockTransport{}})
		bi, _ := NewBulkIndexer(BulkIndexerConfig{Client: es, Decoder: customJSONDecoder{}})

		err := bi.Add(context.Background(), BulkIndexerItem{
			Action:     "index",
			DocumentID: "1",
			Body:       strings.NewReader(`{"title":"foo"}`),
		})
		if err != nil {
			t.Fatalf("Unexpected error: %s", err)
		}

		if err := bi.Close(context.Background()); err != nil {
			t.Errorf("Unexpected error: %s", err)
		}

		stats := bi.Stats()

		if stats.NumAdded != uint(1) {
			t.Errorf("Unexpected NumAdded: %d", stats.NumAdded)
		}
	})

	t.Run("Custom Flusher", func(t *testing.T) {
		es, _ := elasticsearch.NewClient(elasticsearch.Config{Transport: &mockTransport{}})
		bi, _ := NewBulkIndexer(BulkIndexerConfig{
			Client:  es,
			Index:   "foo",
			Flusher: &customFlusher{},
		})

		err := bi.Add(context.Background(), BulkIndexerItem{
			Action: "index",
			Body:   strings.NewReader(`{"title":"foo"}`),
		})
		if err != nil {
			t.Fatalf("Unexpected error: %s", err)
		}

		if err := bi.Close(context.Background()); err != nil {
			t.Errorf("Unexpected error: %s", err)
		}

		stats := bi.Stats()

		if stats.NumAdded != uint(1) {
			t.Errorf("Unexpected NumAdded: %d", stats.NumAdded)
		}
	})
}

type customJSONDecoder struct{}

func (d customJSONDecoder) UnmarshalFromReader(r io.Reader, blk *BulkIndexerResponse) error {
	return json.NewDecoder(r).Decode(blk)
}

type customFlusher struct{ Client *elasticsearch.Client }

func (f *customFlusher) Flush(ctx context.Context, req esapi.BulkRequest) (*esapi.Response, error) {
	body, _ := ioutil.ReadAll(req.Body)
	fmt.Printf(">>> Custom Flusher: %q: %s\n",
		req.Index, strings.Replace(string(body), "\n", "", -1))
	return &esapi.Response{
		StatusCode: 200,
		Body:       ioutil.NopCloser(strings.NewReader(`{}`)),
	}, nil
}