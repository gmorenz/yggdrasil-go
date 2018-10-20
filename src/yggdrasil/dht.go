package yggdrasil

import (
	"sort"
	"time"
)

const dht_lookup_size = 16

// dhtInfo represents everything we know about a node in the DHT.
// This includes its key, a cache of it's NodeID, coords, and timing/ping related info for deciding who/when to ping nodes for maintenance.
type dhtInfo struct {
	nodeID_hidden *NodeID
	key           boxPubKey
	coords        []byte
	send          time.Time // When we last sent a message
	recv          time.Time // When we last received a message
	//pings         int           // Decide when to drop
	//throttle      time.Duration // Time to wait before pinging a node to bootstrap buckets, increases exponentially from 1 second to 1 minute
	//bootstrapSend time.Time     // The time checked/updated as part of throttle checks
}

// Returns the *NodeID associated with dhtInfo.key, calculating it on the fly the first time or from a cache all subsequent times.
func (info *dhtInfo) getNodeID() *NodeID {
	if info.nodeID_hidden == nil {
		info.nodeID_hidden = getNodeID(&info.key)
	}
	return info.nodeID_hidden
}

// Request for a node to do a lookup.
// Includes our key and coords so they can send a response back, and the destination NodeID we want to ask about.
type dhtReq struct {
	Key    boxPubKey // Key of whoever asked
	Coords []byte    // Coords of whoever asked
	Dest   NodeID    // NodeID they're asking about
}

// Response to a DHT lookup.
// Includes the key and coords of the node that's responding, and the destination they were asked about.
// The main part is Infos []*dhtInfo, the lookup response.
type dhtRes struct {
	Key    boxPubKey // key of the sender
	Coords []byte    // coords of the sender
	Dest   NodeID
	Infos  []*dhtInfo // response
}

// The main DHT struct.
type dht struct {
	core   *Core
	nodeID NodeID
	table  map[NodeID]*dhtInfo
	peers  chan *dhtInfo // other goroutines put incoming dht updates here
	reqs   map[boxPubKey]map[NodeID]time.Time
	//rumorMill      []dht_rumor
}

func (t *dht) init(c *Core) {
	// TODO
	t.core = c
	t.nodeID = *t.core.GetNodeID()
	t.peers = make(chan *dhtInfo, 1024)
	t.reset()
}

func (t *dht) reset() {
	t.table = make(map[NodeID]*dhtInfo)
}

func (t *dht) lookup(nodeID *NodeID, allowWorse bool) []*dhtInfo {
	return nil
	var results []*dhtInfo
	var successor *dhtInfo
	sTarget := t.nodeID.next()
	for infoID, info := range t.table {
		if allowWorse || dht_ordered(&t.nodeID, &infoID, nodeID) {
			results = append(results, info)
		} else {
			if successor == nil || dht_ordered(&sTarget, &infoID, successor.getNodeID()) {
				successor = info
			}
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		return dht_ordered(results[j].getNodeID(), results[i].getNodeID(), nodeID)
	})
	if successor != nil {
		results = append([]*dhtInfo{successor}, results...)
	}
	if len(results) > dht_lookup_size {
		results = results[:dht_lookup_size]
	}
	return results
}

// Insert into table, preserving the time we last sent a packet if the node was already in the table, otherwise setting that time to now
func (t *dht) insert(info *dhtInfo) {
	info.recv = time.Now()
	if oldInfo, isIn := t.table[*info.getNodeID()]; isIn {
		info.send = oldInfo.send
	} else {
		info.send = info.recv
	}
	t.table[*info.getNodeID()] = info
}

// Return true if first/second/third are (partially) ordered correctly
//  FIXME? maybe total ordering makes more sense
func dht_ordered(first, second, third *NodeID) bool {
	var ordered bool
	for idx := 0; idx < NodeIDLen; idx++ {
		f, s, t := first[idx], second[idx], third[idx]
		switch {
		case f == s && s == t:
			continue
		case f <= s && s <= t:
			ordered = true // nothing wrapped around 0
		case t <= f && f <= s:
			ordered = true // 0 is between second and third
		case s <= t && t <= f:
			ordered = true // 0 is between first and second
		}
		break
	}
	return ordered
}

// Reads a request, performs a lookup, and responds.
// Update info about the node that sent the request.
func (t *dht) handleReq(req *dhtReq) {
	// Send them what they asked for
	loc := t.core.switchTable.getLocator()
	coords := loc.getCoords()
	res := dhtRes{
		Key:    t.core.boxPub,
		Coords: coords,
		Dest:   req.Dest,
		Infos:  t.lookup(&req.Dest, false),
	}
	t.sendRes(&res, req)
	// Also (possibly) add them to our DHT
	info := dhtInfo{
		key:    req.Key,
		coords: req.Coords,
	}
	// For bootstrapping to work, we need to add these nodes to the table
	// Using insertIfNew, they can lie about coords, but searches will route around them
	// Using the mill would mean trying to block off the mill becomes an attack vector
	t.insert(&info)
}

// Sends a lookup response to the specified node.
func (t *dht) sendRes(res *dhtRes, req *dhtReq) {
	// Send a reply for a dhtReq
	bs := res.encode()
	shared := t.core.sessions.getSharedKey(&t.core.boxPriv, &req.Key)
	payload, nonce := boxSeal(shared, bs, nil)
	p := wire_protoTrafficPacket{
		Coords:  req.Coords,
		ToKey:   req.Key,
		FromKey: t.core.boxPub,
		Nonce:   *nonce,
		Payload: payload,
	}
	packet := p.encode()
	t.core.router.out(packet)
}

// Returns nodeID + 1
func (nodeID NodeID) next() NodeID {
	for idx := len(nodeID); idx >= 0; idx-- {
		nodeID[idx] += 1
		if nodeID[idx] != 0 {
			break
		}
	}
	return nodeID
}

// Returns nodeID - 1
func (nodeID NodeID) prev() NodeID {
	for idx := len(nodeID); idx >= 0; idx-- {
		nodeID[idx] -= 1
		if nodeID[idx] != 0xff {
			break
		}
	}
	return nodeID
}

// Reads a lookup response, checks that we had sent a matching request, and processes the response info.
// This mainly consists of updating the node we asked in our DHT (they responded, so we know they're still alive), and deciding if we want to do anything with their responses
func (t *dht) handleRes(res *dhtRes) {
	t.core.searches.handleDHTRes(res)
	reqs, isIn := t.reqs[res.Key]
	if !isIn {
		return
	}
	_, isIn = reqs[res.Dest]
	if !isIn {
		return
	}
	delete(reqs, res.Dest)
	rinfo := dhtInfo{
		key:    res.Key,
		coords: res.Coords,
	}
	t.insert(&rinfo) // Or at the end, after checking successor/predecessor?
	var successor *dhtInfo
	var predecessor *dhtInfo
	for infoID, info := range t.table {
		// Get current successor and predecessor
		if successor == nil || dht_ordered(&t.nodeID, &infoID, successor.getNodeID()) {
			successor = info
		}
		if predecessor == nil || dht_ordered(predecessor.getNodeID(), &infoID, &t.nodeID) {
			predecessor = info
		}
	}
	for _, info := range res.Infos {
		if *info.getNodeID() == t.nodeID {
			continue
		} // Skip self
		// Send a request to all better successors or predecessors
		// We could try sending to only the best, but then packet loss matters more
		if successor == nil || dht_ordered(&t.nodeID, info.getNodeID(), successor.getNodeID()) {
			// ping
		}
		if predecessor == nil || dht_ordered(predecessor.getNodeID(), info.getNodeID(), &t.nodeID) {
			// ping
		}
	}
	// TODO add everyting else to a rumor mill for later use? (when/how?)
}

// Sends a lookup request to the specified node.
func (t *dht) sendReq(req *dhtReq, dest *dhtInfo) {
	// Send a dhtReq to the node in dhtInfo
	bs := req.encode()
	shared := t.core.sessions.getSharedKey(&t.core.boxPriv, &dest.key)
	payload, nonce := boxSeal(shared, bs, nil)
	p := wire_protoTrafficPacket{
		Coords:  dest.coords,
		ToKey:   dest.key,
		FromKey: t.core.boxPub,
		Nonce:   *nonce,
		Payload: payload,
	}
	packet := p.encode()
	t.core.router.out(packet)
	reqsToDest, isIn := t.reqs[dest.key]
	if !isIn {
		t.reqs[dest.key] = make(map[NodeID]time.Time)
		reqsToDest, isIn = t.reqs[dest.key]
		if !isIn {
			panic("This should never happen")
		}
	}
	reqsToDest[req.Dest] = time.Now()
}

func (t *dht) ping(info *dhtInfo, target *NodeID) {
	// Creates a req for the node at dhtInfo, asking them about the target (if one is given) or themself (if no target is given)
	if target == nil {
		target = info.getNodeID()
	}
	loc := t.core.switchTable.getLocator()
	coords := loc.getCoords()
	req := dhtReq{
		Key:    t.core.boxPub,
		Coords: coords,
		Dest:   *target,
	}
	info.send = time.Now()
	t.sendReq(&req, info)
}

func (t *dht) doMaintenance() {
	// Ping successor, asking for their predecessor, and clean up old/expired info
	var successor *dhtInfo
	now := time.Now()
	for infoID, info := range t.table {
		if now.Sub(info.recv) > time.Minute {
			delete(t.table, infoID)
		} else if successor == nil || dht_ordered(&t.nodeID, &infoID, successor.getNodeID()) {
			successor = info
		}
	}
	if successor != nil &&
		now.Sub(successor.recv) > 30*time.Second &&
		now.Sub(successor.send) > 6*time.Second {
		t.ping(successor, nil)
	}
}
