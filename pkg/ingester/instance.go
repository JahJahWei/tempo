package ingester

import (
	"bytes"
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/weaveworks/common/httpgrpc"

	"github.com/joe-elliott/frigg/pkg/friggpb"
	"github.com/joe-elliott/frigg/pkg/ingester/wal"
	"github.com/joe-elliott/frigg/pkg/storage"
	"github.com/joe-elliott/frigg/pkg/util"
)

type traceFingerprint uint64

const queryBatchSize = 128

// Errors returned on Query.
var (
	ErrTraceMissing = errors.New("Trace missing")
)

var (
	tracesCreatedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "frigg",
		Name:      "ingester_traces_created_total",
		Help:      "The total number of traces created per tenant.",
	}, []string{"tenant"})
)

type instance struct {
	tracesMtx sync.Mutex
	traces    map[traceFingerprint]*trace

	blockTracesMtx sync.RWMutex
	traceRecords   []*storage.TraceRecord
	walBlock       wal.WALBlock
	lastBlockCut   time.Time

	instanceID         string
	tracesCreatedTotal prometheus.Counter
	limiter            *Limiter
	wal                wal.WAL
}

func newInstance(instanceID string, limiter *Limiter, wal wal.WAL) *instance {
	i := &instance{
		traces:       map[traceFingerprint]*trace{},
		lastBlockCut: time.Now(),

		instanceID:         instanceID,
		tracesCreatedTotal: tracesCreatedTotal.WithLabelValues(instanceID),
		limiter:            limiter,
		wal:                wal,
	}
	i.ResetBlock()
	return i
}

func (i *instance) Push(ctx context.Context, req *friggpb.PushRequest) error {
	i.tracesMtx.Lock()
	defer i.tracesMtx.Unlock()

	trace, err := i.getOrCreateTrace(req)
	if err != nil {
		return err
	}

	if err := trace.Push(ctx, req); err != nil {
		return err
	}

	return nil
}

// Moves any complete traces out of the map to complete traces
func (i *instance) CutCompleteTraces(cutoff time.Duration, immediate bool) error {
	i.tracesMtx.Lock()
	defer i.tracesMtx.Unlock()

	i.blockTracesMtx.Lock()
	defer i.blockTracesMtx.Unlock()

	now := time.Now()
	for key, trace := range i.traces {
		if now.Add(cutoff).After(trace.lastAppend) || immediate {
			start, length, err := i.walBlock.Write(trace.trace)
			if err != nil {
				return err
			}

			// insert sorted
			idx := sort.Search(len(i.traceRecords), func(idx int) bool {
				return bytes.Compare(i.traceRecords[idx].TraceID, trace.traceID) == -1
			})
			i.traceRecords = append(i.traceRecords, nil)
			copy(i.traceRecords[idx+1:], i.traceRecords[idx:])
			i.traceRecords[idx] = &storage.TraceRecord{
				TraceID: trace.traceID,
				Start:   start,
				Length:  length,
			}

			delete(i.traces, key)
		}
	}

	return nil
}

func (i *instance) IsBlockReady(maxTracesPerBlock int, maxBlockLifetime time.Duration) bool {
	i.blockTracesMtx.RLock()
	defer i.blockTracesMtx.RUnlock()

	now := time.Now()
	return len(i.traceRecords) >= maxTracesPerBlock || i.lastBlockCut.Add(maxBlockLifetime).Before(now)
}

// GetBlock() returns complete traces.  It is up to the caller to do something sensible at this point
func (i *instance) GetBlock() ([]*storage.TraceRecord, wal.WALBlock) {
	i.blockTracesMtx.Lock()
	defer i.blockTracesMtx.Unlock()

	return i.traceRecords, i.walBlock
}

func (i *instance) ResetBlock() error {
	i.blockTracesMtx.Lock()
	defer i.blockTracesMtx.Unlock()

	i.traceRecords = make([]*storage.TraceRecord, 0) //todo : init this with some value?  max traces per block?

	if i.walBlock != nil {
		i.walBlock.Clear()
	}

	var err error
	i.walBlock, err = i.wal.NewBlock(uuid.New(), i.instanceID)
	return err
}

func (i *instance) getOrCreateTrace(req *friggpb.PushRequest) (*trace, error) {
	traceID := req.Spans[0].TraceID // two assumptions here should hold.  distributor separates spans by traceid.  0 length span slices should be filtered before here
	fp := traceFingerprint(util.Fingerprint(traceID))

	trace, ok := i.traces[fp]
	if ok {
		return trace, nil
	}

	err := i.limiter.AssertMaxTracesPerUser(i.instanceID, len(i.traces))
	if err != nil {
		return nil, httpgrpc.Errorf(http.StatusTooManyRequests, err.Error())
	}

	trace = newTrace(fp, traceID)
	i.traces[fp] = trace
	i.tracesCreatedTotal.Inc()

	return trace, nil
}

func isDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}
