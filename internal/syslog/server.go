package syslog

import (
	"code.cloudfoundry.org/go-loggregator/metrics"
	"code.cloudfoundry.org/go-loggregator/rpc/loggregator_v2"
	"code.cloudfoundry.org/log-cache/pkg/rpc/logcache_v1"
	"code.cloudfoundry.org/tlsconfig"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/influxdata/go-syslog"
	"github.com/influxdata/go-syslog/octetcounting"
	"log"
	"time"

	"golang.org/x/net/context"
	"net"
	"strconv"
	"strings"
	"sync"
)

type Server struct {
	sync.Mutex
	port           int
	l              net.Listener
	loggr          *log.Logger
	ingress        metrics.Counter
	invalidIngress metrics.Counter
	logCache       logcache_v1.IngressClient
	syslogCert     string
	syslogKey      string
	idleTimeout    time.Duration
}

type MetricsRegistry interface {
	NewCounter(name string, opts ...metrics.MetricOption) metrics.Counter
}

type ServerOption func(s *Server)

func NewServer(
	loggr *log.Logger,
	logCache logcache_v1.IngressClient,
	metrics MetricsRegistry,
	cert string,
	key string,
	opts ...ServerOption,
) *Server {
	s := &Server{
		logCache:    logCache,
		loggr:       loggr,
		syslogCert:  cert,
		syslogKey:   key,
		idleTimeout: 2 * time.Minute,
	}

	for _, o := range opts {
		o(s)
	}

	s.ingress = metrics.NewCounter("ingress")
	s.invalidIngress = metrics.NewCounter("invalid_ingress")

	return s
}

func WithServerPort(p int) ServerOption {
	return func(s *Server) {
		s.port = p
	}
}

func WithIdleTimeout(d time.Duration) ServerOption {
	return func(s *Server) {
		s.idleTimeout = d
	}
}

func (s *Server) Start() {
	tlsConfig := s.buildTLSConfig()
	l, err := tls.Listen("tcp", fmt.Sprintf(":%d", s.port), tlsConfig)
	if err != nil {
		s.loggr.Fatalf("unable to start syslog server: %s", err)
	}
	defer s.Stop()

	s.Lock()
	s.l = l
	s.Unlock()

	for {
		c, err := l.Accept()
		if err != nil {
			s.loggr.Printf("syslog server no longer accepting connections: %s", err)
			return
		}
		go s.handleConnection(c)
	}
}

func (s *Server) handleConnection(conn net.Conn) {
	defer conn.Close()
	s.setReadDeadline(conn)

	p := octetcounting.NewParser()
	p.WithListener(s.parseListenerForConnection(conn))
	p.Parse(conn)
}

func (s *Server) parseListenerForConnection(conn net.Conn) syslog.ParserListener {
	return func(res *syslog.Result) {
		s.parseListener(res)
		s.setReadDeadline(conn)
	}
}

func (s *Server) setReadDeadline(conn net.Conn) {
	err := conn.SetReadDeadline(time.Now().Add(s.idleTimeout))
	if err != nil {
		s.loggr.Printf("syslog server could not set deadline on connection: %s", err)
	}
}

func (s *Server) parseListener(res *syslog.Result) {
	msg := res.Message
	if res.Error != nil {
		s.invalidIngress.Add(1)
		return
	}

	env, err := s.convertToEnvelope(msg)
	if err != nil {
		s.invalidIngress.Add(1)
		return
	}

	_, err = s.logCache.Send(
		context.Background(),
		&logcache_v1.SendRequest{
			Envelopes: &loggregator_v2.EnvelopeBatch{
				Batch: []*loggregator_v2.Envelope{env},
			},
		},
	)
	if err != nil {
		s.loggr.Println("syslog server dropped messages to log cache")
		return
	}

	s.ingress.Add(1)
}

func (s *Server) convertToEnvelope(msg syslog.Message) (*loggregator_v2.Envelope, error) {
	sourceType, instanceId := s.sourceTypeInstIdFromPID(*msg.ProcID())
	env := &loggregator_v2.Envelope{
		SourceId:   *msg.Appname(),
		Timestamp:  msg.Timestamp().UnixNano(),
		InstanceId: instanceId,
	}

	if msg.StructuredData() != nil {
		for envType, payload := range *msg.StructuredData() {
			switch {
			case strings.HasPrefix(envType, "counter"):
				return convertCounter(env, payload)
			case strings.HasPrefix(envType, "gauge"):
				return convertGauge(env, payload)
			case strings.HasPrefix(envType, "event"):
				return convertEvent(env, payload)
			case strings.HasPrefix(envType, "timer"):
				return convertTimer(env, payload)
			default:
				return nil, fmt.Errorf("unknown envelope type for structured data: %#v", msg.StructuredData())
			}
		}
	}

	env.Message = &loggregator_v2.Envelope_Log{
		Log: &loggregator_v2.Log{
			Payload: []byte(strings.TrimSpace(*msg.Message())),
			Type:    s.typeFromPriority(int(*msg.Priority())),
		},
	}
	env.Tags = map[string]string{"source_type": sourceType}
	return env, nil
}

func convertTimer(env *loggregator_v2.Envelope, msg map[string]string) (*loggregator_v2.Envelope, error) {
	start, err := strconv.ParseInt(msg["start"], 10, 64)
	if err != nil {
		return nil, err
	}

	stop, err := strconv.ParseInt(msg["stop"], 10, 64)
	if err != nil {
		return nil, err
	}

	env.Message = &loggregator_v2.Envelope_Timer{
		Timer: &loggregator_v2.Timer{
			Name: msg["name"],
			Start: start,
			Stop:  stop,
		},
	}

	return env, nil
}

func convertEvent(env *loggregator_v2.Envelope, msg map[string]string) (*loggregator_v2.Envelope, error) {
	env.Message = &loggregator_v2.Envelope_Event{
		Event: &loggregator_v2.Event{
			Title: msg["title"],
			Body: msg["body"],
		},
	}

	return env, nil
}

func convertCounter(env *loggregator_v2.Envelope, msg map[string]string) (*loggregator_v2.Envelope, error) {
	delta, err := strconv.ParseUint(msg["delta"], 10, 64)
	if err != nil {
		return nil, err
	}
	total, err := strconv.ParseUint(msg["total"], 10, 64)
	if err != nil {
		return nil, err
	}
	env.Message = &loggregator_v2.Envelope_Counter{
		Counter: &loggregator_v2.Counter{
			Name:  msg["name"],
			Delta: delta,
			Total: total,
		},
	}
	return env, nil
}

func convertGauge(env *loggregator_v2.Envelope, msg map[string]string) (*loggregator_v2.Envelope, error) {
	unit, ok := msg["unit"]
	if !ok {
		return nil, errors.New("expected unit not found in gauge")
	}
	value, err := strconv.ParseFloat(msg["value"], 64)
	if err != nil {
		return nil, err
	}
	env.Message = &loggregator_v2.Envelope_Gauge{
		Gauge: &loggregator_v2.Gauge{
			Metrics: map[string]*loggregator_v2.GaugeValue{
				msg["name"]: {
					Unit:  unit,
					Value: value,
				},
			},
		},
	}
	return env, nil
}

func (s *Server) typeFromPriority(priority int) loggregator_v2.Log_Type {
	if priority == 11 {
		return loggregator_v2.Log_ERR
	}

	return loggregator_v2.Log_OUT
}

func (s *Server) sourceTypeInstIdFromPID(pid string) (sourceType, instanceId string) {
	pid = strings.Trim(pid, "[]")

	pidToks := strings.Split(pid, "/")
	sourceType = pidToks[0]

	instanceId = pidToks[len(pidToks)-1]

	return
}

func (s *Server) Addr() string {
	s.Lock()
	defer s.Unlock()

	if s.l != nil && s.l.Addr() != nil {
		return s.l.Addr().String()
	}
	return ""
}

func (s *Server) Stop() {
	s.Lock()
	defer s.Unlock()

	if s.l != nil {
		s.l.Close()
		s.l = nil
	}
}

func (s *Server) buildTLSConfig() *tls.Config {
	tlsConfig, err := tlsconfig.Build(
		tlsconfig.WithInternalServiceDefaults(),
		tlsconfig.WithIdentityFromFile(s.syslogCert, s.syslogKey),
	).Server()

	if err != nil {
		log.Fatal(err)
	}
	return tlsConfig
}
