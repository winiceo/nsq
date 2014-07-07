// This is an NSQ client that reads the specified topic/channel
// and re-publishes the messages to destination nsqd via TCP

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bitly/go-hostpool"
	"github.com/bitly/go-nsq"
	"github.com/bitly/go-simplejson"
	"github.com/bitly/nsq/util"
)

const (
	ModeRoundRobin = iota
	ModeHostPool
)

var (
	showVersion = flag.Bool("version", false, "print version string")

	topic       = flag.String("topic", "", "nsq topic")
	channel     = flag.String("channel", "nsq_to_nsq", "nsq channel")
	destTopic   = flag.String("destination-topic", "", "destination nsq topic")
	maxInFlight = flag.Int("max-in-flight", 200, "max number of messages to allow in flight")

	statusEvery = flag.Int("status-every", 250, "the # of requests between logging status (per destination), 0 disables")
	mode        = flag.String("mode", "round-robin", "the upstream request mode options: round-robin (default), hostpool")

	readerOpts          = util.StringArray{}
	nsqdTCPAddrs        = util.StringArray{}
	lookupdHTTPAddrs    = util.StringArray{}
	destNsqdTCPAddrs    = util.StringArray{}
	whitelistJsonFields = util.StringArray{}

	requireJsonField = flag.String("require-json-field", "", "for JSON messages: only pass messages that contain this field")
	requireJsonValue = flag.String("require-json-value", "", "for JSON messages: only pass messages in which the required field has this value")

	// TODO: remove, deprecated
	maxBackoffDuration = flag.Duration("max-backoff-duration", 120*time.Second, "(deprecated) use --reader-opt=max_backoff_duration=X, the maximum backoff duration")
)

func init() {
	flag.Var(&readerOpts, "reader-opt", "option to passthrough to nsq.Consumer (may be given multiple times)")
	flag.Var(&nsqdTCPAddrs, "nsqd-tcp-address", "nsqd TCP address (may be given multiple times)")
	flag.Var(&destNsqdTCPAddrs, "destination-nsqd-tcp-address", "destination nsqd TCP address (may be given multiple times)")
	flag.Var(&lookupdHTTPAddrs, "lookupd-http-address", "lookupd HTTP address (may be given multiple times)")

	flag.Var(&whitelistJsonFields, "whitelist-json-field", "for JSON messages: pass this field (may be given multiple times)")
}

type Durations []time.Duration

func (s Durations) Len() int {
	return len(s)
}

func (s Durations) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s Durations) Less(i, j int) bool {
	return s[i] < s[j]
}

type PublishHandler struct {
	addresses util.StringArray
	producers map[string]*nsq.Producer
	mode      int
	counter   uint64
	hostPool  hostpool.HostPool
	reqs      Durations
	respChan  chan *nsq.ProducerTransaction

	requireJsonValueParsed   bool
	requireJsonValueIsNumber bool
	requireJsonNumber        float64
}

func (ph *PublishHandler) responder() {
	var msg *nsq.Message
	var startTime time.Time
	var hostPoolResponse hostpool.HostPoolResponse

	for t := range ph.respChan {
		switch ph.mode {
		case ModeRoundRobin:
			msg = t.Args[0].(*nsq.Message)
			startTime = t.Args[1].(time.Time)
			hostPoolResponse = nil
		case ModeHostPool:
			msg = t.Args[0].(*nsq.Message)
			startTime = t.Args[1].(time.Time)
			hostPoolResponse = t.Args[2].(hostpool.HostPoolResponse)
		}

		success := t.Error == nil

		if hostPoolResponse != nil {
			if !success {
				hostPoolResponse.Mark(errors.New("failed"))
			} else {
				hostPoolResponse.Mark(nil)
			}
		}

		if success {
			msg.Finish()
		} else {
			msg.Requeue(-1)
		}

		if *statusEvery > 0 {
			duration := time.Now().Sub(startTime)
			ph.reqs = append(ph.reqs, duration)
		}

		if *statusEvery > 0 && len(ph.reqs) >= *statusEvery {
			var total time.Duration
			for _, v := range ph.reqs {
				total += v
			}
			avgMs := (total.Seconds() * 1000) / float64(len(ph.reqs))

			sort.Sort(ph.reqs)
			p95Ms := percentile(95.0, ph.reqs, len(ph.reqs)).Seconds() * 1000
			p99Ms := percentile(99.0, ph.reqs, len(ph.reqs)).Seconds() * 1000

			log.Printf("finished %d requests - 99th: %.02fms - 95th: %.02fms - avg: %.02fms",
				*statusEvery, p99Ms, p95Ms, avgMs)

			ph.reqs = ph.reqs[:0]
		}
	}
}

func (ph *PublishHandler) shouldPassMessage(jsonMsg *simplejson.Json) (bool, bool) {
	pass := true
	backoff := false

	if *requireJsonField == "" {
		return pass, backoff
	}

	if *requireJsonValue != "" && !ph.requireJsonValueParsed {
		// cache conversion in case needed while filtering json
		var err error
		ph.requireJsonNumber, err = strconv.ParseFloat(*requireJsonValue, 64)
		ph.requireJsonValueIsNumber = (err == nil)
		ph.requireJsonValueParsed = true
	}

	jsonVal, ok := jsonMsg.CheckGet(*requireJsonField)
	if !ok {
		pass = false
		if *requireJsonValue != "" {
			log.Printf("ERROR: missing field to check required value")
			backoff = true
		}
	} else if *requireJsonValue != "" {
		// if command-line argument can't convert to float, then it can't match a number
		// if it can, also integers (up to 2^53 or so) can be compared as float64
		if strVal, err := jsonVal.String(); err == nil {
			if strVal != *requireJsonValue {
				pass = false
			}
		} else if ph.requireJsonValueIsNumber {
			floatVal, err := jsonVal.Float64()
			if err != nil || ph.requireJsonNumber != floatVal {
				pass = false
			}
		} else {
			// json value wasn't a plain string, and argument wasn't a number
			// give up on comparisons of other types
			pass = false
		}
	}

	return pass, backoff
}

func filterMessage(jsonMsg *simplejson.Json, rawMsg []byte) ([]byte, error) {
	if len(whitelistJsonFields) == 0 {
		// no change
		return rawMsg, nil
	}

	msg, err := jsonMsg.Map()
	if err != nil {
		return nil, errors.New("json is not an object")
	}

	newMsg := make(map[string]interface{}, len(whitelistJsonFields))

	for _, key := range whitelistJsonFields {
		value, ok := msg[key]
		if ok {
			// avoid printing int as float (go 1.0)
			switch tvalue := value.(type) {
			case float64:
				ivalue := int64(tvalue)
				if float64(ivalue) == tvalue {
					newMsg[key] = ivalue
				} else {
					newMsg[key] = tvalue
				}
			default:
				newMsg[key] = value
			}
		}
	}

	newRawMsg, err := json.Marshal(newMsg)
	if err != nil {
		return nil, fmt.Errorf("unable to marshal filtered message %r", newMsg)
	}
	return newRawMsg, nil
}

func (ph *PublishHandler) HandleMessage(m *nsq.Message) error {
	var err error
	msgBody := m.Body

	if *requireJsonField != "" || len(whitelistJsonFields) > 0 {
		var jsonMsg *simplejson.Json
		jsonMsg, err = simplejson.NewJson(m.Body)
		if err != nil {
			log.Printf("ERROR: Unable to decode json: %s", m.Body)
			return nil
		}

		if pass, backoff := ph.shouldPassMessage(jsonMsg); !pass {
			if backoff {
				return errors.New("backoff")
			}
			return nil
		}

		msgBody, err = filterMessage(jsonMsg, m.Body)
		if err != nil {
			log.Printf("ERROR: filterMessage() failed: %s", err)
			return err
		}
	}

	startTime := time.Now()

	switch ph.mode {
	case ModeRoundRobin:
		idx := ph.counter % uint64(len(ph.addresses))
		p := ph.producers[ph.addresses[idx]]
		err = p.PublishAsync(*destTopic, msgBody, ph.respChan, m, startTime)
		ph.counter++
	case ModeHostPool:
		hostPoolResponse := ph.hostPool.Get()
		p := ph.producers[hostPoolResponse.Host()]
		err = p.PublishAsync(*destTopic, msgBody, ph.respChan, m, startTime, hostPoolResponse)
		if err != nil {
			hostPoolResponse.Mark(err)
		}
	}

	if err != nil {
		return err
	}
	m.DisableAutoResponse()
	return nil
}

func percentile(perc float64, arr []time.Duration, length int) time.Duration {
	indexOfPerc := int(math.Ceil(((perc / 100.0) * float64(length)) + 0.5))
	if indexOfPerc >= length {
		indexOfPerc = length - 1
	}
	return arr[indexOfPerc]
}

func hasArg(s string) bool {
	for _, arg := range os.Args {
		if strings.Contains(arg, s) {
			return true
		}
	}
	return false
}

func main() {
	var selectedMode int

	flag.Parse()

	if *showVersion {
		fmt.Printf("nsq_to_nsq v%s\n", util.BINARY_VERSION)
		return
	}

	if *topic == "" || *channel == "" {
		log.Fatalf("--topic and --channel are required")
	}

	if *destTopic == "" {
		*destTopic = *topic
	}

	if !util.IsValidTopicName(*topic) {
		log.Fatalf("--topic is invalid")
	}

	if !util.IsValidTopicName(*destTopic) {
		log.Fatalf("--destination-topic is invalid")
	}

	if !util.IsValidChannelName(*channel) {
		log.Fatalf("--channel is invalid")
	}

	if len(nsqdTCPAddrs) == 0 && len(lookupdHTTPAddrs) == 0 {
		log.Fatalf("--nsqd-tcp-address or --lookupd-http-address required")
	}
	if len(nsqdTCPAddrs) > 0 && len(lookupdHTTPAddrs) > 0 {
		log.Fatalf("use --nsqd-tcp-address or --lookupd-http-address not both")
	}

	if len(destNsqdTCPAddrs) == 0 {
		log.Fatalf("--destination-nsqd-tcp-address required")
	}

	switch *mode {
	case "round-robin":
		selectedMode = ModeRoundRobin
	case "hostpool":
		selectedMode = ModeHostPool
	}

	termChan := make(chan os.Signal, 1)
	signal.Notify(termChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	cfg := nsq.NewConfig()
	cfg.UserAgent = fmt.Sprintf("nsq_to_nsq/%s go-nsq/%s", util.BINARY_VERSION, nsq.VERSION)


	err := util.ParseReaderOpts(cfg, readerOpts)
	if err != nil {
		log.Fatalf(err.Error())
	}
	cfg.MaxInFlight = *maxInFlight

	// TODO: remove, deprecated
	if hasArg("max-backoff-duration") {
		log.Printf("WARNING: --max-backoff-duration is deprecated in favor of --reader-opt=max_backoff_duration=X")
		cfg.MaxBackoffDuration = *maxBackoffDuration
	}

	r, err := nsq.NewConsumer(*topic, *channel, cfg)
	if err != nil {
		log.Fatalf(err.Error())
	}
	r.SetLogger(log.New(os.Stderr, "", log.LstdFlags), nsq.LogLevelInfo)

	wcfg := nsq.NewConfig()
	cfg.UserAgent = fmt.Sprintf("nsq_to_nsq/%s go-nsq/%s", util.BINARY_VERSION, nsq.VERSION)
	producers := make(map[string]*nsq.Producer)
	for _, addr := range destNsqdTCPAddrs {
		producer, err := nsq.NewProducer(addr, wcfg)
		if err != nil {
			log.Fatalf("failed creating producer %s", err)
		}
		producers[addr] = producer
	}

	handler := &PublishHandler{
		addresses: destNsqdTCPAddrs,
		producers: producers,
		mode:      selectedMode,
		reqs:      make(Durations, 0, *statusEvery),
		hostPool:  hostpool.New(destNsqdTCPAddrs),
		respChan:  make(chan *nsq.ProducerTransaction, len(destNsqdTCPAddrs)),
	}
	r.AddConcurrentHandlers(handler, len(destNsqdTCPAddrs))
	for i := 0; i < len(destNsqdTCPAddrs); i++ {
		go handler.responder()
	}

	for _, addrString := range nsqdTCPAddrs {
		err := r.ConnectToNSQD(addrString)
		if err != nil {
			log.Fatalf(err.Error())
		}
	}

	for _, addrString := range lookupdHTTPAddrs {
		log.Printf("lookupd addr %s", addrString)
		err := r.ConnectToNSQLookupd(addrString)
		if err != nil {
			log.Fatalf(err.Error())
		}
	}

	for {
		select {
		case <-r.StopChan:
			return
		case <-termChan:
			r.Stop()
		}
	}
}
