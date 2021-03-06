package memberlist

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/kit/log/level"
	"github.com/hashicorp/memberlist"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/atomic"

	"github.com/cortexproject/cortex/pkg/ring/kv/codec"
	"github.com/cortexproject/cortex/pkg/util"
	"github.com/cortexproject/cortex/pkg/util/flagext"
)

const (
	maxCasRetries              = 10          // max retries in CAS operation
	noChangeDetectedRetrySleep = time.Second // how long to sleep after no change was detected in CAS
)

// Client implements kv.Client interface, by using memberlist.KV
type Client struct {
	kv    *KV // reference to singleton memberlist-based KV
	codec codec.Codec
}

// NewClient creates new client instance. Supplied codec must already be registered in KV.
func NewClient(kv *KV, codec codec.Codec) (*Client, error) {
	c := kv.GetCodec(codec.CodecID())
	if c == nil {
		return nil, fmt.Errorf("codec not registered in KV: %s", codec.CodecID())
	}

	return &Client{
		kv:    kv,
		codec: codec,
	}, nil
}

// Get is part of kv.Client interface.
func (c *Client) Get(ctx context.Context, key string) (interface{}, error) {
	return c.kv.Get(key, c.codec)
}

// CAS is part of kv.Client interface
func (c *Client) CAS(ctx context.Context, key string, f func(in interface{}) (out interface{}, retry bool, err error)) error {
	return c.kv.CAS(ctx, key, c.codec, f)
}

// WatchKey is part of kv.Client interface.
func (c *Client) WatchKey(ctx context.Context, key string, f func(interface{}) bool) {
	c.kv.WatchKey(ctx, key, c.codec, f)
}

// WatchPrefix calls f whenever any value stored under prefix changes.
// Part of kv.Client interface.
func (c *Client) WatchPrefix(ctx context.Context, prefix string, f func(string, interface{}) bool) {
	c.kv.WatchPrefix(ctx, prefix, c.codec, f)
}

// KVConfig is a config for memberlist.KV
type KVConfig struct {
	// Memberlist options.
	NodeName            string        `yaml:"node_name"`
	StreamTimeout       time.Duration `yaml:"stream_timeout"`
	RetransmitMult      int           `yaml:"retransmit_factor"`
	PushPullInterval    time.Duration `yaml:"pull_push_interval"`
	GossipInterval      time.Duration `yaml:"gossip_interval"`
	GossipNodes         int           `yaml:"gossip_nodes"`
	GossipToTheDeadTime time.Duration `yaml:"gossip_to_dead_nodes_time"`
	DeadNodeReclaimTime time.Duration `yaml:"dead_node_reclaim_time"`

	// List of members to join
	JoinMembers      flagext.StringSlice `yaml:"join_members"`
	AbortIfJoinFails bool                `yaml:"abort_if_cluster_join_fails"`

	// Remove LEFT ingesters from ring after this timeout.
	LeftIngestersTimeout time.Duration `yaml:"left_ingesters_timeout"`

	// Timeout used when leaving the memberlist cluster.
	LeaveTimeout time.Duration `yaml:"leave_timeout"`

	TCPTransport TCPTransportConfig `yaml:",inline"`

	// Where to put custom metrics. Metrics are not registered, if this is nil.
	MetricsRegisterer prometheus.Registerer `yaml:"-"`
	MetricsNamespace  string                `yaml:"-"`

	// Codecs to register. Codecs need to be registered before joining other members.
	Codecs []codec.Codec `yaml:"-"`
}

// RegisterFlags registers flags.
func (cfg *KVConfig) RegisterFlags(f *flag.FlagSet, prefix string) {
	// "Defaults to hostname" -- memberlist sets it to hostname by default.
	f.StringVar(&cfg.NodeName, prefix+"memberlist.nodename", "", "Name of the node in memberlist cluster. Defaults to hostname.") // memberlist.DefaultLANConfig will put hostname here.
	f.DurationVar(&cfg.StreamTimeout, prefix+"memberlist.stream-timeout", 0, "The timeout for establishing a connection with a remote node, and for read/write operations. Uses memberlist LAN defaults if 0.")
	f.IntVar(&cfg.RetransmitMult, prefix+"memberlist.retransmit-factor", 0, "Multiplication factor used when sending out messages (factor * log(N+1)).")
	f.Var(&cfg.JoinMembers, prefix+"memberlist.join", "Other cluster members to join. Can be specified multiple times. Memberlist store is EXPERIMENTAL.")
	f.BoolVar(&cfg.AbortIfJoinFails, prefix+"memberlist.abort-if-join-fails", true, "If this node fails to join memberlist cluster, abort.")
	f.DurationVar(&cfg.LeftIngestersTimeout, prefix+"memberlist.left-ingesters-timeout", 5*time.Minute, "How long to keep LEFT ingesters in the ring.")
	f.DurationVar(&cfg.LeaveTimeout, prefix+"memberlist.leave-timeout", 5*time.Second, "Timeout for leaving memberlist cluster.")
	f.DurationVar(&cfg.GossipInterval, prefix+"memberlist.gossip-interval", 0, "How often to gossip. Uses memberlist LAN defaults if 0.")
	f.IntVar(&cfg.GossipNodes, prefix+"memberlist.gossip-nodes", 0, "How many nodes to gossip to. Uses memberlist LAN defaults if 0.")
	f.DurationVar(&cfg.PushPullInterval, prefix+"memberlist.pullpush-interval", 0, "How often to use pull/push sync. Uses memberlist LAN defaults if 0.")
	f.DurationVar(&cfg.GossipToTheDeadTime, prefix+"memberlist.gossip-to-dead-nodes-time", 0, "How long to keep gossiping to dead nodes, to give them chance to refute their death. Uses memberlist LAN defaults if 0.")
	f.DurationVar(&cfg.DeadNodeReclaimTime, prefix+"memberlist.dead-node-reclaim-time", 0, "How soon can dead node's name be reclaimed with new address. Defaults to 0, which is disabled.")

	cfg.TCPTransport.RegisterFlags(f, prefix)
}

// KV implements Key-Value store on top of memberlist library. KV store has API similar to kv.Client,
// except methods also need explicit codec for each operation.
type KV struct {
	cfg        KVConfig
	memberlist *memberlist.Memberlist
	broadcasts *memberlist.TransmitLimitedQueue

	// Disabled on Stop()
	casBroadcastsEnabled *atomic.Bool

	// KV Store.
	storeMu sync.Mutex
	store   map[string]valueDesc

	// Codec registry
	codecs map[string]codec.Codec

	// Key watchers
	watchersMu     sync.Mutex
	watchers       map[string][]chan string
	prefixWatchers map[string][]chan string

	// closed on shutdown
	shutdown chan struct{}

	// metrics
	numberOfReceivedMessages            prometheus.Counter
	totalSizeOfReceivedMessages         prometheus.Counter
	numberOfInvalidReceivedMessages     prometheus.Counter
	numberOfPulls                       prometheus.Counter
	numberOfPushes                      prometheus.Counter
	totalSizeOfPulls                    prometheus.Counter
	totalSizeOfPushes                   prometheus.Counter
	numberOfBroadcastMessagesInQueue    prometheus.GaugeFunc
	totalSizeOfBroadcastMessagesInQueue prometheus.Gauge
	casAttempts                         prometheus.Counter
	casFailures                         prometheus.Counter
	casSuccesses                        prometheus.Counter
	watchPrefixDroppedNotifications     *prometheus.CounterVec

	storeValuesDesc *prometheus.Desc
	storeSizesDesc  *prometheus.Desc

	memberlistMembersCount prometheus.GaugeFunc
	memberlistHealthScore  prometheus.GaugeFunc

	// make this configurable for tests. Default value is fine for normal usage
	// where updates are coming from network, but when running tests with many
	// goroutines using same KV, default can be too low.
	maxCasRetries int
}

type valueDesc struct {
	// We store bytes here. Reason is that clients calling CAS function will modify the object in place,
	// but unless CAS succeeds, we don't want those modifications to be visible.
	value []byte

	// version (local only) is used to keep track of what we're gossiping about, and invalidate old messages
	version uint

	// ID of codec used to write this value. Only used when sending full state.
	codecID string
}

var (
	// if merge fails because of CAS version mismatch, this error is returned. CAS operation reacts on it
	errVersionMismatch  = errors.New("version mismatch")
	errNoChangeDetected = errors.New("no change detected")
	errTooManyRetries   = errors.New("too many retries")
)

// NewMemberlistClient creates new Client instance. If cfg.JoinMembers is set, it will also try to connect
// to these members and join the cluster. If that fails and AbortIfJoinFails is true, error is returned and no
// client is created.
func NewKV(cfg KVConfig) (*KV, error) {
	level.Warn(util.Logger).Log("msg", "Using memberlist-based KV store is EXPERIMENTAL and not tested in production")

	cfg.TCPTransport.MetricsRegisterer = cfg.MetricsRegisterer
	cfg.TCPTransport.MetricsNamespace = cfg.MetricsNamespace

	tr, err := NewTCPTransport(cfg.TCPTransport)

	if err != nil {
		return nil, fmt.Errorf("failed to create transport: %v", err)
	}

	mlCfg := memberlist.DefaultLANConfig()

	if cfg.StreamTimeout != 0 {
		mlCfg.TCPTimeout = cfg.StreamTimeout
	}
	if cfg.RetransmitMult != 0 {
		mlCfg.RetransmitMult = cfg.RetransmitMult
	}
	if cfg.PushPullInterval != 0 {
		mlCfg.PushPullInterval = cfg.PushPullInterval
	}
	if cfg.GossipInterval != 0 {
		mlCfg.GossipInterval = cfg.GossipInterval
	}
	if cfg.GossipNodes != 0 {
		mlCfg.GossipNodes = cfg.GossipNodes
	}
	if cfg.GossipToTheDeadTime > 0 {
		mlCfg.GossipToTheDeadTime = cfg.GossipToTheDeadTime
	}
	if cfg.DeadNodeReclaimTime > 0 {
		mlCfg.DeadNodeReclaimTime = cfg.DeadNodeReclaimTime
	}
	if cfg.NodeName != "" {
		mlCfg.Name = cfg.NodeName
	}

	mlCfg.LogOutput = newMemberlistLoggerAdapter(util.Logger, false)
	mlCfg.Transport = tr

	// Memberlist uses UDPBufferSize to figure out how many messages it can put into single "packet".
	// As we don't use UDP for sending packets, we can use higher value here.
	mlCfg.UDPBufferSize = 10 * 1024 * 1024

	mlkv := &KV{
		cfg:                  cfg,
		store:                make(map[string]valueDesc),
		codecs:               make(map[string]codec.Codec),
		watchers:             make(map[string][]chan string),
		prefixWatchers:       make(map[string][]chan string),
		shutdown:             make(chan struct{}),
		maxCasRetries:        maxCasRetries,
		casBroadcastsEnabled: atomic.NewBool(true),
	}

	mlCfg.Delegate = mlkv

	list, err := memberlist.Create(mlCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create memberlist: %v", err)
	}

	// finish delegate initialization
	mlkv.memberlist = list
	mlkv.broadcasts = &memberlist.TransmitLimitedQueue{
		NumNodes:       list.NumMembers,
		RetransmitMult: cfg.RetransmitMult,
	}

	// Almost ready...
	mlkv.createAndRegisterMetrics()

	for _, c := range cfg.Codecs {
		mlkv.codecs[c.CodecID()] = c
	}

	// Join the cluster
	if len(cfg.JoinMembers) > 0 {
		reached, err := mlkv.JoinMembers(cfg.JoinMembers)
		if err != nil && cfg.AbortIfJoinFails {
			_ = mlkv.memberlist.Shutdown()
			return nil, err
		}

		if err != nil {
			level.Error(util.Logger).Log("msg", "failed to join memberlist cluster", "err", err)
		} else {
			level.Info(util.Logger).Log("msg", "joined memberlist cluster", "reached_nodes", reached)
		}
	}

	return mlkv, nil
}

// GetCodec returns codec for given ID or nil.
func (m *KV) GetCodec(codecID string) codec.Codec {
	return m.codecs[codecID]
}

// GetListeningPort returns port used for listening for memberlist communication. Useful when BindPort is set to 0.
func (m *KV) GetListeningPort() int {
	return int(m.memberlist.LocalNode().Port)
}

// JoinMembers joins the cluster with given members.
// See https://godoc.org/github.com/hashicorp/memberlist#Memberlist.Join
func (m *KV) JoinMembers(members []string) (int, error) {
	return m.memberlist.Join(members)
}

// Stop tries to leave memberlist cluster and then shutdown memberlist client.
// We do this in order to send out last messages, typically that ingester has LEFT the ring.
func (m *KV) Stop() {
	level.Info(util.Logger).Log("msg", "leaving memberlist cluster")

	m.casBroadcastsEnabled.Store(false)

	// Wait until broadcast queue is empty, but don't wait for too long.
	// Also don't wait if there is just one node left.
	// Problem is that broadcast queue is also filled up by state changes received from other nodes,
	// so it may never be empty in a busy cluster. However, we generally only care about messages
	// generated on this node via CAS, and those are disabled now (via casBroadcastsEnabled), and should be able
	// to get out in this timeout.

	waitTimeout := time.Now().Add(10 * time.Second)
	for m.broadcasts.NumQueued() > 0 && m.memberlist.NumMembers() > 1 && time.Now().Before(waitTimeout) {
		time.Sleep(250 * time.Millisecond)
	}

	if cnt := m.broadcasts.NumQueued(); cnt > 0 {
		level.Warn(util.Logger).Log("msg", "broadcast messages left in queue", "count", cnt, "nodes", m.memberlist.NumMembers())
	}

	err := m.memberlist.Leave(m.cfg.LeaveTimeout)
	if err != nil {
		level.Error(util.Logger).Log("msg", "error when leaving memberlist cluster", "err", err)
	}

	close(m.shutdown)

	err = m.memberlist.Shutdown()
	if err != nil {
		level.Error(util.Logger).Log("msg", "error when shutting down memberlist client", "err", err)
	}
}

// Get returns current value associated with given key.
// No communication with other nodes in the cluster is done here.
func (m *KV) Get(key string, codec codec.Codec) (interface{}, error) {
	val, _, err := m.get(key, codec)
	return val, err
}

// Returns current value with removed tombstones.
func (m *KV) get(key string, codec codec.Codec) (out interface{}, version uint, err error) {
	m.storeMu.Lock()
	v := m.store[key]
	m.storeMu.Unlock()

	out = nil
	if v.value != nil {
		out, err = codec.Decode(v.value)
		if err != nil {
			return nil, 0, err
		}

		if mr, ok := out.(Mergeable); ok {
			// remove ALL tombstones before returning to client.
			// No need for clients to see them.
			mr.RemoveTombstones(time.Time{})
		}
	}

	return out, v.version, nil
}

// WatchKey watches for value changes for given key. When value changes, 'f' function is called with the
// latest value. Notifications that arrive while 'f' is running are coalesced into one subsequent 'f' call.
//
// Watching ends when 'f' returns false, context is done, or this client is shut down.
func (m *KV) WatchKey(ctx context.Context, key string, codec codec.Codec, f func(interface{}) bool) {
	// keep one extra notification, to avoid missing notification if we're busy running the function
	w := make(chan string, 1)

	// register watcher
	m.watchersMu.Lock()
	m.watchers[key] = append(m.watchers[key], w)
	m.watchersMu.Unlock()

	defer func() {
		// unregister watcher on exit
		m.watchersMu.Lock()
		defer m.watchersMu.Unlock()

		removeWatcherChannel(key, w, m.watchers)
	}()

	for {
		select {
		case <-w:
			// value changed
			val, _, err := m.get(key, codec)
			if err != nil {
				level.Warn(util.Logger).Log("msg", "failed to decode value while watching for changes", "key", key, "err", err)
				continue
			}

			if !f(val) {
				return
			}

		case <-m.shutdown:
			// stop watching on shutdown
			return

		case <-ctx.Done():
			return
		}
	}
}

// WatchPrefix watches for any change of values stored under keys with given prefix. When change occurs,
// function 'f' is called with key and current value.
// Each change of the key results in one notification. If there are too many pending notifications ('f' is slow),
// some notifications may be lost.
//
// Watching ends when 'f' returns false, context is done, or this client is shut down.
func (m *KV) WatchPrefix(ctx context.Context, prefix string, codec codec.Codec, f func(string, interface{}) bool) {
	// we use bigger buffer here, since keys are interesting and we don't want to lose them.
	w := make(chan string, 16)

	// register watcher
	m.watchersMu.Lock()
	m.prefixWatchers[prefix] = append(m.prefixWatchers[prefix], w)
	m.watchersMu.Unlock()

	defer func() {
		// unregister watcher on exit
		m.watchersMu.Lock()
		defer m.watchersMu.Unlock()

		removeWatcherChannel(prefix, w, m.prefixWatchers)
	}()

	for {
		select {
		case key := <-w:
			val, _, err := m.get(key, codec)
			if err != nil {
				level.Warn(util.Logger).Log("msg", "failed to decode value while watching for changes", "key", key, "err", err)
				continue
			}

			if !f(key, val) {
				return
			}

		case <-m.shutdown:
			// stop watching on shutdown
			return

		case <-ctx.Done():
			return
		}
	}
}

func removeWatcherChannel(k string, w chan string, watchers map[string][]chan string) {
	ws := watchers[k]
	for ix, kw := range ws {
		if kw == w {
			ws = append(ws[:ix], ws[ix+1:]...)
			break
		}
	}

	if len(ws) > 0 {
		watchers[k] = ws
	} else {
		delete(watchers, k)
	}
}

func (m *KV) notifyWatchers(key string) {
	m.watchersMu.Lock()
	defer m.watchersMu.Unlock()

	for _, kw := range m.watchers[key] {
		select {
		case kw <- key:
			// notification sent.
		default:
			// cannot send notification to this watcher at the moment
			// but since this is a buffered channel, it means that
			// there is already a pending notification anyway
		}
	}

	for p, ws := range m.prefixWatchers {
		if strings.HasPrefix(key, p) {
			for _, pw := range ws {
				select {
				case pw <- key:
					// notification sent.
				default:
					c, _ := m.watchPrefixDroppedNotifications.GetMetricWithLabelValues(p)
					if c != nil {
						c.Inc()
					}

					level.Warn(util.Logger).Log("msg", "failed to send notification to prefix watcher", "prefix", p)
				}
			}
		}
	}
}

// CAS implements Compare-And-Set/Swap operation.
//
// CAS expects that value returned by 'f' function implements Mergeable interface. If it doesn't, CAS fails immediately.
//
// This method combines Compare-And-Swap with Merge: it calls 'f' function to get a new state, and then merges this
// new state into current state, to find out what the change was. Resulting updated current state is then CAS-ed to
// KV store, and change is broadcast to cluster peers. Merge function is called with CAS flag on, so that it can
// detect removals. If Merge doesn't result in any change (returns nil), then operation fails and is retried again.
// After too many failed retries, this method returns error.
func (m *KV) CAS(ctx context.Context, key string, codec codec.Codec, f func(in interface{}) (out interface{}, retry bool, err error)) error {
	var lastError error = nil

outer:
	for retries := m.maxCasRetries; retries > 0; retries-- {
		m.casAttempts.Inc()

		if lastError == errNoChangeDetected {
			// We only get here, if 'f' reports some change, but Merge function reports no change. This can happen
			// with Ring's merge function, which depends on timestamps (and not the tokens) with 1-second resolution.
			// By waiting for one second, we hope that Merge will be able to detect change from 'f' function.

			select {
			case <-time.After(noChangeDetectedRetrySleep):
				// ok
			case <-ctx.Done():
				lastError = ctx.Err()
				break outer
			}
		}

		change, newver, retry, err := m.trySingleCas(key, codec, f)
		if err != nil {
			level.Debug(util.Logger).Log("msg", "CAS attempt failed", "err", err, "retry", retry)

			lastError = err
			if !retry {
				break
			}
			continue
		}

		if change != nil {
			m.casSuccesses.Inc()
			m.notifyWatchers(key)

			if m.casBroadcastsEnabled.Load() {
				m.broadcastNewValue(key, change, newver, codec)
			} else {
				level.Warn(util.Logger).Log("msg", "skipped broadcasting CAS update because memberlist KV is shutting down", "key", key)
			}
		}

		return nil
	}

	if lastError == errVersionMismatch {
		// this is more likely error than version mismatch.
		lastError = errTooManyRetries
	}

	m.casFailures.Inc()
	return fmt.Errorf("failed to CAS-update key %s: %v", key, lastError)
}

// returns change, error (or nil, if CAS succeeded), and whether to retry or not.
// returns errNoChangeDetected if merge failed to detect change in f's output.
func (m *KV) trySingleCas(key string, codec codec.Codec, f func(in interface{}) (out interface{}, retry bool, err error)) (Mergeable, uint, bool, error) {
	val, ver, err := m.get(key, codec)
	if err != nil {
		return nil, 0, false, fmt.Errorf("failed to get value: %v", err)
	}

	out, retry, err := f(val)
	if err != nil {
		return nil, 0, retry, fmt.Errorf("fn returned error: %v", err)
	}

	if out == nil {
		// no change to be done
		return nil, 0, false, nil
	}

	// Don't even try
	r, ok := out.(Mergeable)
	if !ok || r == nil {
		return nil, 0, retry, fmt.Errorf("invalid type: %T, expected Mergeable", out)
	}

	// To support detection of removed items from value, we will only allow CAS operation to
	// succeed if version hasn't changed, i.e. state hasn't changed since running 'f'.
	change, newver, err := m.mergeValueForKey(key, r, ver, codec)
	if err == errVersionMismatch {
		return nil, 0, retry, err
	}

	if err != nil {
		return nil, 0, retry, fmt.Errorf("merge failed: %v", err)
	}

	if newver == 0 {
		// CAS method reacts on this error
		return nil, 0, retry, errNoChangeDetected
	}

	return change, newver, retry, nil
}

func (m *KV) broadcastNewValue(key string, change Mergeable, version uint, codec codec.Codec) {
	data, err := codec.Encode(change)
	if err != nil {
		level.Error(util.Logger).Log("msg", "failed to encode change", "err", err)
		return
	}

	kvPair := KeyValuePair{Key: key, Value: data, Codec: codec.CodecID()}
	pairData, err := kvPair.Marshal()
	if err != nil {
		level.Error(util.Logger).Log("msg", "failed to serialize KV pair", "err", err)
	}

	if len(pairData) > 65535 {
		// Unfortunately, memberlist will happily let us send bigger messages via gossip,
		// but then it will fail to parse them properly, because its own size field is 2-bytes only.
		// (github.com/hashicorp/memberlist@v0.1.4/util.go:167, makeCompoundMessage function)
		//
		// Typically messages are smaller (when dealing with couple of updates only), but can get bigger
		// when broadcasting result of push/pull update.
		level.Debug(util.Logger).Log("msg", "broadcast message too big, not broadcasting", "len", len(pairData))
		return
	}

	m.queueBroadcast(key, change.MergeContent(), version, pairData)
}

// NodeMeta is method from Memberlist Delegate interface
func (m *KV) NodeMeta(limit int) []byte {
	// we can send local state from here (512 bytes only)
	// if state is updated, we need to tell memberlist to distribute it.
	return nil
}

// NotifyMsg is method from Memberlist Delegate interface
// Called when single message is received, i.e. what our broadcastNewValue has sent.
func (m *KV) NotifyMsg(msg []byte) {
	m.numberOfReceivedMessages.Inc()
	m.totalSizeOfReceivedMessages.Add(float64(len(msg)))

	kvPair := KeyValuePair{}
	err := kvPair.Unmarshal(msg)
	if err != nil {
		level.Warn(util.Logger).Log("msg", "failed to unmarshal received KV Pair", "err", err)
		m.numberOfInvalidReceivedMessages.Inc()
		return
	}

	if len(kvPair.Key) == 0 {
		level.Warn(util.Logger).Log("msg", "received an invalid KV Pair (empty key)")
		m.numberOfInvalidReceivedMessages.Inc()
		return
	}

	codec := m.GetCodec(kvPair.GetCodec())
	if codec == nil {
		m.numberOfInvalidReceivedMessages.Inc()
		level.Error(util.Logger).Log("msg", "failed to decode received value, unknown codec", "codec", kvPair.GetCodec())
		return
	}

	// we have a ring update! Let's merge it with our version of the ring for given key
	mod, version, err := m.mergeBytesValueForKey(kvPair.Key, kvPair.Value, codec)
	if err != nil {
		level.Error(util.Logger).Log("msg", "failed to store received value", "key", kvPair.Key, "err", err)
	} else if version > 0 {
		m.notifyWatchers(kvPair.Key)

		// Forward this message
		// Memberlist will modify message once this function returns, so we need to make a copy
		msgCopy := append([]byte(nil), msg...)

		// forward this message further
		m.queueBroadcast(kvPair.Key, mod.MergeContent(), version, msgCopy)
	}
}

func (m *KV) queueBroadcast(key string, content []string, version uint, message []byte) {
	l := len(message)

	b := ringBroadcast{
		key:     key,
		content: content,
		version: version,
		msg:     message,
		finished: func(b ringBroadcast) {
			m.totalSizeOfBroadcastMessagesInQueue.Sub(float64(l))
		},
	}

	m.totalSizeOfBroadcastMessagesInQueue.Add(float64(l))
	m.broadcasts.QueueBroadcast(b)
}

// GetBroadcasts is method from Memberlist Delegate interface
// It returns all pending broadcasts (within the size limit)
func (m *KV) GetBroadcasts(overhead, limit int) [][]byte {
	return m.broadcasts.GetBroadcasts(overhead, limit)
}

// LocalState is method from Memberlist Delegate interface
//
// This is "pull" part of push/pull sync (either periodic, or when new node joins the cluster).
// Here we dump our entire state -- all keys and their values. There is no limit on message size here,
// as Memberlist uses 'stream' operations for transferring this state.
func (m *KV) LocalState(join bool) []byte {
	m.numberOfPulls.Inc()

	m.storeMu.Lock()
	defer m.storeMu.Unlock()

	// For each Key/Value pair in our store, we write
	// [4-bytes length of marshalled KV pair] [marshalled KV pair]

	buf := bytes.Buffer{}

	kvPair := KeyValuePair{}
	for key, val := range m.store {
		if val.value == nil {
			continue
		}

		kvPair.Reset()
		kvPair.Key = key
		kvPair.Value = val.value
		kvPair.Codec = val.codecID

		ser, err := kvPair.Marshal()
		if err != nil {
			level.Error(util.Logger).Log("msg", "failed to serialize KV Pair", "err", err)
			continue
		}

		if uint(len(ser)) > math.MaxUint32 {
			level.Error(util.Logger).Log("msg", "value too long", "key", key, "value_length", len(val.value))
			continue
		}

		err = binary.Write(&buf, binary.BigEndian, uint32(len(ser)))
		if err != nil {
			level.Error(util.Logger).Log("msg", "failed to write uint32 to buffer?", "err", err)
			continue
		}
		buf.Write(ser)
	}

	m.totalSizeOfPulls.Add(float64(buf.Len()))
	return buf.Bytes()
}

// MergeRemoteState is method from Memberlist Delegate interface
//
// This is 'push' part of push/pull sync. We merge incoming KV store (all keys and values) with ours.
//
// Data is full state of remote KV store, as generated by `LocalState` method (run on another node).
func (m *KV) MergeRemoteState(data []byte, join bool) {
	m.numberOfPushes.Inc()
	m.totalSizeOfPushes.Add(float64(len(data)))

	kvPair := KeyValuePair{}

	var err error
	// Data contains individual KV pairs (encoded as protobuf messages), each prefixed with 4 bytes length of KV pair:
	// [4-bytes length of marshalled KV pair] [marshalled KV pair] [4-bytes length] [KV pair]...
	for len(data) > 0 {
		if len(data) < 4 {
			err = fmt.Errorf("not enough data left for another KV Pair: %d", len(data))
			break
		}

		kvPairLength := binary.BigEndian.Uint32(data)

		data = data[4:]

		if len(data) < int(kvPairLength) {
			err = fmt.Errorf("not enough data left for next KV Pair, expected %d, remaining %d bytes", kvPairLength, len(data))
			break
		}

		kvPair.Reset()
		err = kvPair.Unmarshal(data[:kvPairLength])
		if err != nil {
			err = fmt.Errorf("failed to parse KV Pair: %v", err)
			break
		}

		data = data[kvPairLength:]

		codec := m.GetCodec(kvPair.GetCodec())
		if codec == nil {
			level.Error(util.Logger).Log("msg", "failed to parse remote state: unknown codec for key", "codec", kvPair.GetCodec(), "key", kvPair.GetKey())
			continue
		}

		// we have both key and value, try to merge it with our state
		change, newver, err := m.mergeBytesValueForKey(kvPair.Key, kvPair.Value, codec)
		if err != nil {
			level.Error(util.Logger).Log("msg", "failed to store received value", "key", kvPair.Key, "err", err)
		} else if newver > 0 {
			m.notifyWatchers(kvPair.Key)
			m.broadcastNewValue(kvPair.Key, change, newver, codec)
		}
	}

	if err != nil {
		level.Error(util.Logger).Log("msg", "failed to parse remote state", "err", err)
	}
}

func (m *KV) mergeBytesValueForKey(key string, incomingData []byte, codec codec.Codec) (Mergeable, uint, error) {
	decodedValue, err := codec.Decode(incomingData)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to decode value: %v", err)
	}

	incomingValue, ok := decodedValue.(Mergeable)
	if !ok {
		return nil, 0, fmt.Errorf("expected Mergeable, got: %T", decodedValue)
	}

	return m.mergeValueForKey(key, incomingValue, 0, codec)
}

// Merges incoming value with value we have in our store. Returns "a change" that can be sent to other
// cluster members to update their state, and new version of the value.
// If CAS version is specified, then merging will fail if state has changed already, and errVersionMismatch is reported.
// If no modification occurred, new version is 0.
func (m *KV) mergeValueForKey(key string, incomingValue Mergeable, casVersion uint, codec codec.Codec) (Mergeable, uint, error) {
	m.storeMu.Lock()
	defer m.storeMu.Unlock()

	curr := m.store[key]
	// if casVersion is 0, then there was no previous value, so we will just do normal merge, without localCAS flag set.
	if casVersion > 0 && curr.version != casVersion {
		return nil, 0, errVersionMismatch
	}
	result, change, err := computeNewValue(incomingValue, curr.value, codec, casVersion > 0)
	if err != nil {
		return nil, 0, err
	}

	// No change, don't store it.
	if change == nil || len(change.MergeContent()) == 0 {
		return nil, 0, nil
	}

	if m.cfg.LeftIngestersTimeout > 0 {
		limit := time.Now().Add(-m.cfg.LeftIngestersTimeout)
		result.RemoveTombstones(limit)
	}

	encoded, err := codec.Encode(result)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to encode merged result: %v", err)
	}

	newVersion := curr.version + 1
	m.store[key] = valueDesc{
		value:   encoded,
		version: newVersion,
		codecID: codec.CodecID(),
	}

	return change, newVersion, nil
}

// returns [result, change, error]
func computeNewValue(incoming Mergeable, stored []byte, c codec.Codec, cas bool) (Mergeable, Mergeable, error) {
	if len(stored) == 0 {
		return incoming, incoming, nil
	}

	old, err := c.Decode(stored)
	if err != nil {
		return incoming, incoming, fmt.Errorf("failed to decode stored value: %v", err)
	}

	if old == nil {
		return incoming, incoming, nil
	}

	oldVal, ok := old.(Mergeable)
	if !ok {
		return incoming, incoming, fmt.Errorf("stored value is not Mergeable, got %T", old)
	}

	if oldVal == nil {
		return incoming, incoming, nil
	}

	// otherwise we have two mergeables, so merge them
	change, err := oldVal.Merge(incoming, cas)
	return oldVal, change, err
}
