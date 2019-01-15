package nozzle

import (
	"io/ioutil"
	"log"
	"runtime"
	"time"

	diodes "code.cloudfoundry.org/go-diodes"
	"code.cloudfoundry.org/go-loggregator"
	"code.cloudfoundry.org/go-loggregator/rpc/loggregator_v2"
	"code.cloudfoundry.org/log-cache/internal/metrics"
	"code.cloudfoundry.org/log-cache/pkg/rpc/logcache_v1"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

// Nozzle reads envelopes and writes them to LogCache.
type Nozzle struct {
	log          *log.Logger
	s            StreamConnector
	metrics      metrics.Initializer
	shardId      string
	selectors    []string
	streamBuffer *diodes.OneToOne

	// LogCache
	addr string
	opts []grpc.DialOption
}

const (
	BATCH_FLUSH_INTERVAL = 500 * time.Millisecond
	BATCH_CHANNEL_SIZE   = 512
)

// StreamConnector reads envelopes from the the logs provider.
type StreamConnector interface {
	// Stream creates a EnvelopeStream for the given request.
	Stream(ctx context.Context, req *loggregator_v2.EgressBatchRequest) loggregator.EnvelopeStream
}

// NewNozzle creates a new Nozzle.
func NewNozzle(c StreamConnector, logCacheAddr string, shardId string, opts ...NozzleOption) *Nozzle {
	n := &Nozzle{
		s:         c,
		addr:      logCacheAddr,
		opts:      []grpc.DialOption{grpc.WithInsecure()},
		log:       log.New(ioutil.Discard, "", 0),
		metrics:   metrics.NullMetrics{},
		shardId:   shardId,
		selectors: []string{},
	}

	for _, o := range opts {
		o(n)
	}

	n.streamBuffer = diodes.NewOneToOne(100000, diodes.AlertFunc(func(missed int) {
		n.log.Printf("Stream buffer dropped %d points", missed)
	}))

	return n
}

// NozzleOption configures a Nozzle.
type NozzleOption func(*Nozzle)

// WithNozzleLogger returns a NozzleOption that configures a nozzle's logger.
// It defaults to silent logging.
func WithNozzleLogger(l *log.Logger) NozzleOption {
	return func(n *Nozzle) {
		n.log = l
	}
}

// WithNozzleMetrics returns a NozzleOption that configures the metrics for the
// Nozzle. It will add metrics to the given map.
func WithNozzleMetrics(metrics metrics.Initializer) NozzleOption {
	return func(n *Nozzle) {
		n.metrics = metrics
	}
}

// WithNozzleDialOpts returns a NozzleOption that configures the dial options
// for dialing the LogCache. It defaults to grpc.WithInsecure().
func WithNozzleDialOpts(opts ...grpc.DialOption) NozzleOption {
	return func(n *Nozzle) {
		n.opts = opts
	}
}

func WithNozzleSelectors(selectors ...string) NozzleOption {
	return func(n *Nozzle) {
		n.selectors = selectors
	}
}

// Start starts reading envelopes from the logs provider and writes them to
// LogCache. It blocks indefinitely.
func (n *Nozzle) Start() {
	rx := n.s.Stream(context.Background(), n.buildBatchReq())

	conn, err := grpc.Dial(n.addr, n.opts...)
	if err != nil {
		log.Fatalf("failed to dial %s: %s", n.addr, err)
	}
	client := logcache_v1.NewIngressClient(conn)

	ingressInc := n.metrics.NewCounter("Ingress")
	egressInc := n.metrics.NewCounter("Egress")
	errInc := n.metrics.NewCounter("Err")

	go n.envelopeReader(rx, ingressInc)

	ch := make(chan []*loggregator_v2.Envelope, BATCH_CHANNEL_SIZE)

	log.Printf("Starting %d nozzle workers...", runtime.NumCPU())
	for i := 0; i < 2*runtime.NumCPU(); i++ {
		go n.envelopeWriter(ch, client, errInc, egressInc)
	}

	// The batcher will block indefinitely.
	n.envelopeBatcher(ch)

	for {
		batch := rx()
		ingressInc(uint64(len(batch)))

		ctx, _ := context.WithTimeout(context.Background(), 3*time.Second)
		_, err := client.Send(ctx, &logcache_v1.SendRequest{
			Envelopes: &loggregator_v2.EnvelopeBatch{
				Batch: batch,
			},
		})

		if err != nil {
			log.Printf("failed to write envelopes: %s", err)
			errInc(1)
			continue
		}

		egressInc(uint64(len(batch)))
	}
}

func (n *Nozzle) envelopeBatcher(ch chan []*loggregator_v2.Envelope) {
	poller := diodes.NewPoller(n.streamBuffer)
	envelopes := make([]*loggregator_v2.Envelope, 0)
	t := time.NewTimer(BATCH_FLUSH_INTERVAL)
	for {
		data, found := poller.TryNext()

		if found {
			// this bizarre typecasting is an implementation detail of diode
			envelope := *(*loggregator_v2.Envelope)(data)
			envelopes = append(envelopes, &envelope)
		}

		select {
		case <-t.C:
			if len(envelopes) > 0 {
				select {
				case ch <- envelopes:
					envelopes = make([]*loggregator_v2.Envelope, 0)
				default:
					envelopes = envelopes[:0]
				}
			}
			t.Reset(BATCH_FLUSH_INTERVAL)
		default:
			if len(envelopes) >= BATCH_CHANNEL_SIZE {
				select {
				case ch <- envelopes:
					envelopes = make([]*loggregator_v2.Envelope, 0)
				default:
					envelopes = envelopes[:0]
				}
				t.Reset(BATCH_FLUSH_INTERVAL)
			}
		}
	}
}

func (n *Nozzle) envelopeWriter(ch chan []*loggregator_v2.Envelope, client logcache_v1.IngressClient, errInc, egressInc func(uint64)) {
	for {
		envelopes := <-ch

		ctx, _ := context.WithTimeout(context.Background(), 3*time.Second)
		_, err := client.Send(ctx, &logcache_v1.SendRequest{
			Envelopes: &loggregator_v2.EnvelopeBatch{
				Batch: envelopes,
			},
		})

		if err != nil {
			errInc(1)
			continue
		}

		egressInc(uint64(len(envelopes)))
	}
}

func (n *Nozzle) envelopeReader(rx loggregator.EnvelopeStream, ingressInc func(uint64)) {
	for {
		envelopeBatch := rx()
		for _, envelope := range envelopeBatch {
			n.streamBuffer.Set(diodes.GenericDataType(envelope))
			ingressInc(1)
		}
	}
}

var selectorTypes = map[string]*loggregator_v2.Selector{
	"log": {
		Message: &loggregator_v2.Selector_Log{
			Log: &loggregator_v2.LogSelector{},
		},
	},
	"gauge": {
		Message: &loggregator_v2.Selector_Gauge{
			Gauge: &loggregator_v2.GaugeSelector{},
		},
	},
	"counter": {
		Message: &loggregator_v2.Selector_Counter{
			Counter: &loggregator_v2.CounterSelector{},
		},
	},
	"timer": {
		Message: &loggregator_v2.Selector_Timer{
			Timer: &loggregator_v2.TimerSelector{},
		},
	},
	"event": {
		Message: &loggregator_v2.Selector_Event{
			Event: &loggregator_v2.EventSelector{},
		},
	},
}

func (n *Nozzle) buildBatchReq() *loggregator_v2.EgressBatchRequest {
	var selectors []*loggregator_v2.Selector

	for _, selectorType := range n.selectors {
		selectors = append(selectors, selectorTypes[selectorType])
	}

	return &loggregator_v2.EgressBatchRequest{
		ShardId:          n.shardId,
		UsePreferredTags: true,
		Selectors:        selectors,
	}
}
