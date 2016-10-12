package nsqadmin

import (
	"encoding/json"
	"fmt"
	"html/template"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/absolute8511/nsq/internal/clusterinfo"
	"github.com/absolute8511/nsq/internal/http_api"
	"github.com/absolute8511/nsq/internal/protocol"
	"github.com/absolute8511/nsq/internal/version"
	"github.com/julienschmidt/httprouter"
)

func maybeWarnMsg(msgs []string) string {
	if len(msgs) > 0 {
		return "WARNING: " + strings.Join(msgs, "; ")
	}
	return ""
}

// this is similar to httputil.NewSingleHostReverseProxy except it passes along basic auth
func NewSingleHostReverseProxy(target *url.URL, timeout time.Duration) *httputil.ReverseProxy {
	director := func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		if target.User != nil {
			passwd, _ := target.User.Password()
			req.SetBasicAuth(target.User.Username(), passwd)
		}
	}
	return &httputil.ReverseProxy{
		Director:  director,
		Transport: http_api.NewDeadlineTransport(timeout),
	}
}

type httpServer struct {
	ctx    *Context
	router http.Handler
	client *http_api.Client
	ci     *clusterinfo.ClusterInfo
}

func NewHTTPServer(ctx *Context) *httpServer {
	log := http_api.Log(adminLog)

	client := http_api.NewClient(ctx.nsqadmin.httpClientTLSConfig)

	router := httprouter.New()
	router.HandleMethodNotAllowed = true
	router.PanicHandler = http_api.LogPanicHandler(adminLog)
	router.NotFound = http_api.LogNotFoundHandler(adminLog)
	router.MethodNotAllowed = http_api.LogMethodNotAllowedHandler(adminLog)
	s := &httpServer{
		ctx:    ctx,
		router: router,
		client: client,
		ci:     clusterinfo.New(ctx.nsqadmin.opts.Logger, client),
	}

	router.Handle("GET", "/ping", http_api.Decorate(s.pingHandler, log, http_api.PlainText))

	router.Handle("GET", "/", http_api.Decorate(s.indexHandler, log))
	router.Handle("GET", "/topics", http_api.Decorate(s.indexHandler, log))
	router.Handle("GET", "/topics/:topic", http_api.Decorate(s.indexHandler, log))
	router.Handle("GET", "/topics/:topic/:channel", http_api.Decorate(s.indexHandler, log))
	router.Handle("GET", "/nodes", http_api.Decorate(s.indexHandler, log))
	router.Handle("GET", "/nodes/:node", http_api.Decorate(s.indexHandler, log))
	router.Handle("GET", "/counter", http_api.Decorate(s.indexHandler, log))
	router.Handle("GET", "/lookup", http_api.Decorate(s.indexHandler, log))

	router.Handle("GET", "/static/:asset", http_api.Decorate(s.staticAssetHandler, log, http_api.PlainText))
	router.Handle("GET", "/fonts/:asset", http_api.Decorate(s.staticAssetHandler, log, http_api.PlainText))
	if s.ctx.nsqadmin.opts.ProxyGraphite {
		proxy := NewSingleHostReverseProxy(ctx.nsqadmin.graphiteURL, 20*time.Second)
		router.Handler("GET", "/render", proxy)
	}

	// v1 endpoints
	router.Handle("GET", "/api/topics", http_api.Decorate(s.topicsHandler, log, http_api.V1))
	router.Handle("GET", "/api/topics/:topic", http_api.Decorate(s.topicHandler, log, http_api.V1))
	router.Handle("GET", "/api/coordinators/:node/:topic/:partition", http_api.Decorate(s.coordinatorHandler, log, http_api.V1))
	router.Handle("GET", "/api/lookup/nodes", http_api.Decorate(s.lookupNodesHandler, log, http_api.V1))
	router.Handle("GET", "/api/topics/:topic/:channel", http_api.Decorate(s.channelHandler, log, http_api.V1))
	router.Handle("GET", "/api/nodes", http_api.Decorate(s.nodesHandler, log, http_api.V1))
	router.Handle("GET", "/api/nodes/:node", http_api.Decorate(s.nodeHandler, log, http_api.V1))
	router.Handle("POST", "/api/topics", http_api.Decorate(s.createTopicChannelHandler, log, http_api.V1))
	router.Handle("POST", "/api/topics/:topic", http_api.Decorate(s.topicActionHandler, log, http_api.V1))
	router.Handle("POST", "/api/topics/:topic/:channel", http_api.Decorate(s.channelActionHandler, log, http_api.V1))
	router.Handle("DELETE", "/api/nodes/:node", http_api.Decorate(s.tombstoneNodeForTopicHandler, log, http_api.V1))
	router.Handle("DELETE", "/api/topics/:topic", http_api.Decorate(s.deleteTopicHandler, log, http_api.V1))
	router.Handle("DELETE", "/api/topics/:topic/:channel", http_api.Decorate(s.deleteChannelHandler, log, http_api.V1))
	router.Handle("GET", "/api/counter", http_api.Decorate(s.counterHandler, log, http_api.V1))
	router.Handle("GET", "/api/graphite", http_api.Decorate(s.graphiteHandler, log, http_api.V1))

	return s
}

func (s *httpServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	s.router.ServeHTTP(w, req)
}

func (s *httpServer) pingHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	return "OK", nil
}

func (s *httpServer) indexHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	asset, _ := Asset("index.html")
	t, _ := template.New("index").Parse(string(asset))

	w.Header().Set("Content-Type", "text/html")
	lookupdAddresses := make([]string, 0)
	lookupdAddresseMap := make(map[string]bool)
	for _, addr := range s.ctx.nsqadmin.opts.NSQDHTTPAddresses {
		lookupdAddresseMap[addr] = false
	}
	lookupdNodes, err := s.ci.ListAllLookupdNodes(s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses)
	if err != nil {
		s.ctx.nsqadmin.logf("WARNING: failed to list lookupd nodes : %v", err)
	} else {
		s.ctx.nsqadmin.logf("list lookupd found nodes : %v", lookupdNodes)
		for _, n := range lookupdNodes.AllNodes {
			addr := net.JoinHostPort(n.NodeIP, n.HttpPort)
			lookupdAddresseMap[addr] = false
		}
		if lookupdNodes.LeaderNode.ID != "" {
			leaderAddr := net.JoinHostPort(lookupdNodes.LeaderNode.NodeIP, lookupdNodes.LeaderNode.HttpPort)
			lookupdAddresseMap[leaderAddr] = true
		}
	}
	for addr, isLeader := range lookupdAddresseMap {
		if isLeader {
			lookupdAddresses = append(lookupdAddresses, addr+" (Leader)")
		} else {
			lookupdAddresses = append(lookupdAddresses, addr)
		}
	}
	s.ctx.nsqadmin.logf("total lookupd nodes : %v", lookupdAddresses)
	t.Execute(w, struct {
		Version             string
		ProxyGraphite       bool
		GraphEnabled        bool
		GraphiteURL         string
		StatsdInterval      int
		UseStatsdPrefixes   bool
		StatsdCounterFormat string
		StatsdGaugeFormat   string
		StatsdPrefix        string
		NSQLookupd          []string
		AllNSQLookupds      []string
	}{
		Version:             version.Binary,
		ProxyGraphite:       s.ctx.nsqadmin.opts.ProxyGraphite,
		GraphEnabled:        s.ctx.nsqadmin.opts.GraphiteURL != "",
		GraphiteURL:         s.ctx.nsqadmin.opts.GraphiteURL,
		StatsdInterval:      int(s.ctx.nsqadmin.opts.StatsdInterval / time.Second),
		UseStatsdPrefixes:   s.ctx.nsqadmin.opts.UseStatsdPrefixes,
		StatsdCounterFormat: s.ctx.nsqadmin.opts.StatsdCounterFormat,
		StatsdGaugeFormat:   s.ctx.nsqadmin.opts.StatsdGaugeFormat,
		StatsdPrefix:        s.ctx.nsqadmin.opts.StatsdPrefix,
		NSQLookupd:          s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses,
		AllNSQLookupds:      lookupdAddresses,
	})

	return nil, nil
}

func (s *httpServer) staticAssetHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	assetName := ps.ByName("asset")

	asset, err := Asset(assetName)
	if err != nil {
		return nil, http_api.Err{404, "NOT_FOUND"}
	}

	ext := path.Ext(assetName)
	ct := mime.TypeByExtension(ext)
	if ct == "" {
		switch ext {
		case ".svg":
			ct = "image/svg+xml"
		case ".woff":
			ct = "application/font-woff"
		case ".ttf":
			ct = "application/font-sfnt"
		case ".eot":
			ct = "application/vnd.ms-fontobject"
		case ".woff2":
			ct = "application/font-woff2"
		}
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}

	return string(asset), nil
}

func (s *httpServer) topicsHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	var messages []string

	reqParams, err := http_api.NewReqParams(req)
	if err != nil {
		return nil, http_api.Err{400, err.Error()}
	}

	var topics []string
	if len(s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses) != 0 {
		topics, err = s.ci.GetLookupdTopics(s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses)
	} else {
		topics, err = s.ci.GetNSQDTopics(s.ctx.nsqadmin.opts.NSQDHTTPAddresses)
	}
	if err != nil {
		pe, ok := err.(clusterinfo.PartialErr)
		if !ok {
			s.ctx.nsqadmin.logf("ERROR: failed to get topics - %s", err)
			return nil, http_api.Err{502, fmt.Sprintf("UPSTREAM_ERROR: %s", err)}
		}
		s.ctx.nsqadmin.logf("WARNING: %s", err)
		messages = append(messages, pe.Error())
	}

	inactive, _ := reqParams.Get("inactive")
	if inactive == "true" {
		topicChannelMap := make(map[string][]string)
		if len(s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses) == 0 {
			goto respond
		}
		for _, topicName := range topics {
			producers, _, _ := s.ci.GetLookupdTopicProducers(
				topicName, s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses)
			if len(producers) == 0 {
				topicChannels, _ := s.ci.GetLookupdTopicChannels(
					topicName, s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses)
				topicChannelMap[topicName] = topicChannels
			}
		}
	respond:
		return struct {
			Topics  map[string][]string `json:"topics"`
			Message string              `json:"message"`
		}{topicChannelMap, maybeWarnMsg(messages)}, nil
	}

	return struct {
		Topics  []string `json:"topics"`
		Message string   `json:"message"`
	}{topics, maybeWarnMsg(messages)}, nil
}

func (s *httpServer) lookupNodesHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	var messages []string
	nodes, err := s.ci.ListAllLookupdNodes(s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses)
	if err != nil {
		pe, ok := err.(clusterinfo.PartialErr)
		if !ok {
			s.ctx.nsqadmin.logf("ERROR: failed to get lookupd nodes - %s", err)
			return nil, http_api.Err{502, fmt.Sprintf("UPSTREAM_ERROR: %s", err)}
		}
		s.ctx.nsqadmin.logf("WARNING: partial error %s", err)
		messages = append(messages, pe.Error())
	}
	return struct {
		*clusterinfo.LookupdNodes
		Message string `json:"message"`
	}{nodes, maybeWarnMsg(messages)}, nil
}

func (s *httpServer) coordinatorHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	topicName := ps.ByName("topic")
	partition := ps.ByName("partition")

	var messages []string
	node := ps.ByName("node")

	producers, _, err := s.ci.GetTopicProducers(topicName,
		s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses,
		s.ctx.nsqadmin.opts.NSQDHTTPAddresses)
	if err != nil {
		pe, ok := err.(clusterinfo.PartialErr)
		if !ok {
			s.ctx.nsqadmin.logf("ERROR: failed to get topic producers - %s", err)
			return nil, http_api.Err{502, fmt.Sprintf("UPSTREAM_ERROR: %s", err)}
		}
		s.ctx.nsqadmin.logf("WARNING: %s", err)
		messages = append(messages, pe.Error())
	}

	producer := producers.Search(node)
	if producer == nil {
		return nil, http_api.Err{404, "NODE_NOT_FOUND"}
	}

	topicCoordStats, err := s.ci.GetNSQDCoordStats(clusterinfo.Producers{producer}, topicName, partition)
	if err != nil {
		s.ctx.nsqadmin.logf("ERROR: failed to get nsqd coordinator stats - %s", err)
		messages = append(messages, err.Error())
	}

	return struct {
		*clusterinfo.CoordStats
		Message string `json:"message"`
	}{topicCoordStats, maybeWarnMsg(messages)}, nil

	return nil, nil
}

func (s *httpServer) topicHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	var messages []string

	topicName := ps.ByName("topic")

	producers, _, err := s.ci.GetTopicProducers(topicName,
		s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses,
		s.ctx.nsqadmin.opts.NSQDHTTPAddresses)
	if err != nil {
		pe, ok := err.(clusterinfo.PartialErr)
		if !ok {
			s.ctx.nsqadmin.logf("ERROR: failed to get topic producers - %s", err)
			return nil, http_api.Err{502, fmt.Sprintf("UPSTREAM_ERROR: %s", err)}
		}
		s.ctx.nsqadmin.logf("WARNING: %s", err)
		messages = append(messages, pe.Error())
	}
	topicStats, _, err := s.ci.GetNSQDStats(producers, topicName, "partition")
	if err != nil {
		pe, ok := err.(clusterinfo.PartialErr)
		if !ok {
			s.ctx.nsqadmin.logf("ERROR: failed to get topic metadata - %s", err)
			return nil, http_api.Err{502, fmt.Sprintf("UPSTREAM_ERROR: %s", err)}
		}
		s.ctx.nsqadmin.logf("WARNING: %s", err)
		messages = append(messages, pe.Error())
	}

	topicCoordStats, err := s.ci.GetNSQDCoordStats(producers, topicName, "")
	if err != nil {
		s.ctx.nsqadmin.logf("ERROR: failed to get nsqd topic %v coordinator stats - %s", topicName, err)
		messages = append(messages, err.Error())
	}

	statsMap := make(map[string]map[string]clusterinfo.TopicCoordStat)
	if topicCoordStats != nil {
		for _, stat := range topicCoordStats.TopicCoordStats {
			t, ok := statsMap[stat.Name]
			if !ok {
				t = make(map[string]clusterinfo.TopicCoordStat)
				statsMap[stat.Name] = t
			}
			t[strconv.Itoa(stat.Partition)] = stat
		}
	}

	allNodesTopicStats := &clusterinfo.TopicStats{TopicName: topicName}
	for _, t := range topicStats {
		stat, ok := statsMap[t.TopicName]
		if ok {
			v, ok := stat[t.TopicPartition]
			if ok {
				t.ISRStats = v.ISRStats
				t.CatchupStats = v.CatchupStats
			}
		}
		t.SyncingNum = len(t.ISRStats) + len(t.CatchupStats)

		allNodesTopicStats.Add(t)
	}

	return struct {
		*clusterinfo.TopicStats
		Message string `json:"message"`
	}{allNodesTopicStats, maybeWarnMsg(messages)}, nil
}

func (s *httpServer) channelHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	var messages []string

	topicName := ps.ByName("topic")
	channelName := ps.ByName("channel")

	producers, _, err := s.ci.GetTopicProducers(topicName,
		s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses,
		s.ctx.nsqadmin.opts.NSQDHTTPAddresses)
	if err != nil {
		pe, ok := err.(clusterinfo.PartialErr)
		if !ok {
			s.ctx.nsqadmin.logf("ERROR: failed to get topic producers - %s", err)
			return nil, http_api.Err{502, fmt.Sprintf("UPSTREAM_ERROR: %s", err)}
		}
		s.ctx.nsqadmin.logf("WARNING: %s", err)
		messages = append(messages, pe.Error())
	}
	_, allChannelStats, err := s.ci.GetNSQDStats(producers, topicName, "partition")
	if err != nil {
		pe, ok := err.(clusterinfo.PartialErr)
		if !ok {
			s.ctx.nsqadmin.logf("ERROR: failed to get channel metadata - %s", err)
			return nil, http_api.Err{502, fmt.Sprintf("UPSTREAM_ERROR: %s", err)}
		}
		s.ctx.nsqadmin.logf("WARNING: %s", err)
		messages = append(messages, pe.Error())
	}

	return struct {
		*clusterinfo.ChannelStats
		Message string `json:"message"`
	}{allChannelStats[channelName], maybeWarnMsg(messages)}, nil
}

func (s *httpServer) nodesHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	var messages []string

	producers, err := s.ci.GetProducers(s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses, s.ctx.nsqadmin.opts.NSQDHTTPAddresses)
	if err != nil {
		pe, ok := err.(clusterinfo.PartialErr)
		if !ok {
			s.ctx.nsqadmin.logf("ERROR: failed to get nodes - %s", err)
			return nil, http_api.Err{502, fmt.Sprintf("UPSTREAM_ERROR: %s", err)}
		}
		s.ctx.nsqadmin.logf("WARNING: %s", err)
		messages = append(messages, pe.Error())
	}

	return struct {
		Nodes   clusterinfo.Producers `json:"nodes"`
		Message string                `json:"message"`
	}{producers, maybeWarnMsg(messages)}, nil
}

func (s *httpServer) nodeHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	var messages []string

	node := ps.ByName("node")

	producers, err := s.ci.GetProducers(s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses, s.ctx.nsqadmin.opts.NSQDHTTPAddresses)
	if err != nil {
		pe, ok := err.(clusterinfo.PartialErr)
		if !ok {
			s.ctx.nsqadmin.logf("ERROR: failed to get producers - %s", err)
			return nil, http_api.Err{502, fmt.Sprintf("UPSTREAM_ERROR: %s", err)}
		}
		s.ctx.nsqadmin.logf("WARNING: %s", err)
		messages = append(messages, pe.Error())
	}

	producer := producers.Search(node)
	if producer == nil {
		return nil, http_api.Err{404, "NODE_NOT_FOUND"}
	}
	hostname_port := producer.Hostname + ":" + strconv.Itoa(producer.HTTPPort)

	topicStats, _, err := s.ci.GetNSQDStats(clusterinfo.Producers{producer}, "", "channel-depth")
	if err != nil {
		s.ctx.nsqadmin.logf("ERROR: failed to get nsqd stats - %s", err)
		return nil, http_api.Err{502, fmt.Sprintf("UPSTREAM_ERROR: %s", err)}
	}

	var totalClients int64
	var totalMessages int64
	for _, ts := range topicStats {
		for _, cs := range ts.Channels {
			totalClients += int64(len(cs.Clients))
		}
		totalMessages += ts.MessageCount
	}

	return struct {
		Node          string                    `json:"node"`
		HostnamePort  string                    `json:"hostname_port"`
		TopicStats    []*clusterinfo.TopicStats `json:"topics"`
		TotalMessages int64                     `json:"total_messages"`
		TotalClients  int64                     `json:"total_clients"`
		Message       string                    `json:"message"`
	}{
		Node:          node,
		HostnamePort:  hostname_port,
		TopicStats:    topicStats,
		TotalMessages: totalMessages,
		TotalClients:  totalClients,
		Message:       maybeWarnMsg(messages),
	}, nil
}

func (s *httpServer) tombstoneNodeForTopicHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	var messages []string

	node := ps.ByName("node")

	var body struct {
		Topic string `json:"topic"`
	}
	err := json.NewDecoder(req.Body).Decode(&body)
	if err != nil {
		return nil, http_api.Err{400, "INVALID_BODY"}
	}

	if !protocol.IsValidTopicName(body.Topic) {
		return nil, http_api.Err{400, "INVALID_TOPIC"}
	}

	err = s.ci.TombstoneNodeForTopic(body.Topic, node,
		s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses)
	if err != nil {
		pe, ok := err.(clusterinfo.PartialErr)
		if !ok {
			s.ctx.nsqadmin.logf("ERROR: failed to tombstone node for topic - %s", err)
			return nil, http_api.Err{502, fmt.Sprintf("UPSTREAM_ERROR: %s", err)}
		}
		s.ctx.nsqadmin.logf("WARNING: %s", err)
		messages = append(messages, pe.Error())
	}

	s.notifyAdminAction("tombstone_topic_producer", body.Topic, "", node, req)

	return struct {
		Message string `json:"message"`
	}{maybeWarnMsg(messages)}, nil
}

func (s *httpServer) createTopicChannelHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	var messages []string

	var body struct {
		Topic         string `json:"topic"`
		PartitionNum  string `json:"partition_num"`
		Replicator    string `json:"replicator"`
		RetentionDays string `json:"retention_days"`
		SyncDisk      string `json:"syncdisk"`
	}
	err := json.NewDecoder(req.Body).Decode(&body)
	if err != nil {
		return nil, http_api.Err{400, err.Error()}
	}

	if !protocol.IsValidTopicName(body.Topic) {
		return nil, http_api.Err{400, "INVALID_TOPIC"}
	}

	if body.PartitionNum == "" {
		return nil, http_api.Err{400, "INVALID_TOPIC_PARTITION_NUM"}
	}
	pnum, err := strconv.Atoi(body.PartitionNum)
	if err != nil {
		return nil, http_api.Err{400, err.Error()}
	}

	if body.Replicator == "" {
		return nil, http_api.Err{400, "INVALID_TOPIC_REPLICATOR"}
	}
	replica, err := strconv.Atoi(body.Replicator)
	if err != nil {
		return nil, http_api.Err{400, err.Error()}
	}

	if body.SyncDisk == "" {
		body.SyncDisk = "5000"
	}
	syncDisk, _ := strconv.Atoi(body.SyncDisk)
	err = s.ci.CreateTopic(body.Topic, pnum, replica,
		syncDisk, body.RetentionDays,
		s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses)
	if err != nil {
		pe, ok := err.(clusterinfo.PartialErr)
		if !ok {
			s.ctx.nsqadmin.logf("ERROR: failed to create topic/channel - %s", err)
			return nil, http_api.Err{502, fmt.Sprintf("UPSTREAM_ERROR: %s", err)}
		}
		s.ctx.nsqadmin.logf("WARNING: %s", err)
		messages = append(messages, pe.Error())
	}

	s.notifyAdminAction("create_topic", body.Topic, "", "", req)

	return struct {
		Message string `json:"message"`
	}{maybeWarnMsg(messages)}, nil
}

func (s *httpServer) deleteTopicHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	var messages []string

	topicName := ps.ByName("topic")

	err := s.ci.DeleteTopic(topicName,
		s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses,
		s.ctx.nsqadmin.opts.NSQDHTTPAddresses)
	if err != nil {
		pe, ok := err.(clusterinfo.PartialErr)
		if !ok {
			s.ctx.nsqadmin.logf("ERROR: failed to delete topic - %s", err)
			return nil, http_api.Err{502, fmt.Sprintf("UPSTREAM_ERROR: %s", err)}
		}
		s.ctx.nsqadmin.logf("WARNING: %s", err)
		messages = append(messages, pe.Error())
	}

	s.notifyAdminAction("delete_topic", topicName, "", "", req)

	return struct {
		Message string `json:"message"`
	}{maybeWarnMsg(messages)}, nil
}

func (s *httpServer) deleteChannelHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	var messages []string

	topicName := ps.ByName("topic")
	channelName := ps.ByName("channel")

	err := s.ci.DeleteChannel(topicName, channelName,
		s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses,
		s.ctx.nsqadmin.opts.NSQDHTTPAddresses)
	if err != nil {
		pe, ok := err.(clusterinfo.PartialErr)
		if !ok {
			s.ctx.nsqadmin.logf("ERROR: failed to delete channel - %s", err)
			return nil, http_api.Err{502, fmt.Sprintf("UPSTREAM_ERROR: %s", err)}
		}
		s.ctx.nsqadmin.logf("WARNING: %s", err)
		messages = append(messages, pe.Error())
	}

	s.notifyAdminAction("delete_channel", topicName, channelName, "", req)

	return struct {
		Message string `json:"message"`
	}{maybeWarnMsg(messages)}, nil
}

func (s *httpServer) topicActionHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	topicName := ps.ByName("topic")
	return s.topicChannelAction(req, topicName, "")
}

func (s *httpServer) channelActionHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	topicName := ps.ByName("topic")
	channelName := ps.ByName("channel")
	return s.topicChannelAction(req, topicName, channelName)
}

func (s *httpServer) topicChannelAction(req *http.Request, topicName string, channelName string) (interface{}, error) {
	var messages []string

	var body struct {
		Action string `json:"action"`
	}
	err := json.NewDecoder(req.Body).Decode(&body)
	if err != nil {
		return nil, http_api.Err{400, err.Error()}
	}

	switch body.Action {
	case "pause":
		if channelName != "" {
			err = s.ci.PauseChannel(topicName, channelName,
				s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses,
				s.ctx.nsqadmin.opts.NSQDHTTPAddresses)

			s.notifyAdminAction("pause_channel", topicName, channelName, "", req)
		} else {
			err = s.ci.PauseTopic(topicName,
				s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses,
				s.ctx.nsqadmin.opts.NSQDHTTPAddresses)

			s.notifyAdminAction("pause_topic", topicName, "", "", req)
		}
	case "unpause":
		if channelName != "" {
			err = s.ci.UnPauseChannel(topicName, channelName,
				s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses,
				s.ctx.nsqadmin.opts.NSQDHTTPAddresses)

			s.notifyAdminAction("unpause_channel", topicName, channelName, "", req)
		} else {
			err = s.ci.UnPauseTopic(topicName,
				s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses,
				s.ctx.nsqadmin.opts.NSQDHTTPAddresses)

			s.notifyAdminAction("unpause_topic", topicName, "", "", req)
		}
	case "empty":
		if channelName != "" {
			err = s.ci.EmptyChannel(topicName, channelName,
				s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses,
				s.ctx.nsqadmin.opts.NSQDHTTPAddresses)

			s.notifyAdminAction("empty_channel", topicName, channelName, "", req)
		} else {
			err = s.ci.EmptyTopic(topicName,
				s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses,
				s.ctx.nsqadmin.opts.NSQDHTTPAddresses)

			s.notifyAdminAction("empty_topic", topicName, "", "", req)
		}
	default:
		return nil, http_api.Err{400, "INVALID_ACTION"}
	}

	if err != nil {
		pe, ok := err.(clusterinfo.PartialErr)
		if !ok {
			s.ctx.nsqadmin.logf("ERROR: failed to %s topic/channel - %s", body.Action, err)
			return nil, http_api.Err{502, fmt.Sprintf("UPSTREAM_ERROR: %s", err)}
		}
		s.ctx.nsqadmin.logf("WARNING: %s", err)
		messages = append(messages, pe.Error())
	}

	return struct {
		Message string `json:"message"`
	}{maybeWarnMsg(messages)}, nil
}

type counterStats struct {
	Node         string `json:"node"`
	TopicName    string `json:"topic_name"`
	ChannelName  string `json:"channel_name"`
	MessageCount int64  `json:"message_count"`
}

func (s *httpServer) counterHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	var messages []string
	stats := make(map[string]*counterStats)

	producers, err := s.ci.GetProducers(s.ctx.nsqadmin.opts.NSQLookupdHTTPAddresses, s.ctx.nsqadmin.opts.NSQDHTTPAddresses)
	if err != nil {
		pe, ok := err.(clusterinfo.PartialErr)
		if !ok {
			s.ctx.nsqadmin.logf("ERROR: failed to get counter producer list - %s", err)
			return nil, http_api.Err{502, fmt.Sprintf("UPSTREAM_ERROR: %s", err)}
		}
		s.ctx.nsqadmin.logf("WARNING: %s", err)
		messages = append(messages, pe.Error())
	}
	_, channelStats, err := s.ci.GetNSQDStats(producers, "", "")
	if err != nil {
		pe, ok := err.(clusterinfo.PartialErr)
		if !ok {
			s.ctx.nsqadmin.logf("ERROR: failed to get nsqd stats - %s", err)
			return nil, http_api.Err{502, fmt.Sprintf("UPSTREAM_ERROR: %s", err)}
		}
		s.ctx.nsqadmin.logf("WARNING: %s", err)
		messages = append(messages, pe.Error())
	}

	for _, channelStats := range channelStats {
		for _, hostChannelStats := range channelStats.NodeStats {
			key := fmt.Sprintf("%s:%s:%s", channelStats.TopicName, channelStats.ChannelName, hostChannelStats.Node)
			s, ok := stats[key]
			if !ok {
				s = &counterStats{
					Node:        hostChannelStats.Node,
					TopicName:   channelStats.TopicName,
					ChannelName: channelStats.ChannelName,
				}
				stats[key] = s
			}
			s.MessageCount += hostChannelStats.MessageCount
		}
	}

	return struct {
		Stats   map[string]*counterStats `json:"stats"`
		Message string                   `json:"message"`
	}{stats, maybeWarnMsg(messages)}, nil
}

func (s *httpServer) graphiteHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params) (interface{}, error) {
	reqParams, err := http_api.NewReqParams(req)
	if err != nil {
		return nil, http_api.Err{400, "INVALID_REQUEST"}
	}

	metric, err := reqParams.Get("metric")
	if err != nil || metric != "rate" {
		return nil, http_api.Err{400, "INVALID_ARG_METRIC"}
	}

	target, err := reqParams.Get("target")
	if err != nil {
		return nil, http_api.Err{400, "INVALID_ARG_TARGET"}
	}

	params := url.Values{}
	params.Set("from", fmt.Sprintf("-%dsec", s.ctx.nsqadmin.opts.StatsdInterval*2/time.Second))
	params.Set("until", fmt.Sprintf("-%dsec", s.ctx.nsqadmin.opts.StatsdInterval/time.Second))
	params.Set("format", "json")
	params.Set("target", target)
	query := fmt.Sprintf("/render?%s", params.Encode())
	url := s.ctx.nsqadmin.opts.GraphiteURL + query

	s.ctx.nsqadmin.logf("GRAPHITE: %s", url)

	var response []struct {
		Target     string       `json:"target"`
		DataPoints [][]*float64 `json:"datapoints"`
	}
	_, err = s.client.GETV1(url, &response)
	if err != nil {
		s.ctx.nsqadmin.logf("ERROR: graphite request failed - %s", err)
		return nil, http_api.Err{500, "INTERNAL_ERROR"}
	}

	var rateStr string
	rate := *response[0].DataPoints[0][0]
	if rate < 0 {
		rateStr = "N/A"
	} else {
		rateDivisor := s.ctx.nsqadmin.opts.StatsdInterval / time.Second
		rateStr = fmt.Sprintf("%.2f", rate/float64(rateDivisor))
	}
	return struct {
		Rate string `json:"rate"`
	}{rateStr}, nil
}
