package pineapple

import (
	"encoding/binary"
	"io"
	"log"
	"time"

	"pineapple/src/fastrpc"
	"pineapple/src/genericsmr"
	"pineapple/src/genericsmrproto"
	"pineapple/src/pineappleproto"
	"pineapple/src/state"
)

const CLOCK = 1000 * 10
const CHAN_BUFFER_SIZE = 200000
const TRUE = uint8(1)
const FALSE = uint8(0)

type InstanceStatus int

const (
	PREPARING InstanceStatus = iota
	PREPARED
	ACCEPTED
	COMMITTED
)

// Replica Node: performs ABD operations on single read write, and Paxos on multi read write and RMW
type Replica struct {
	*genericsmr.Replica // extends a generic Paxos replica

	// ABD
	getChan      chan fastrpc.Serializable
	setChan      chan fastrpc.Serializable
	getReplyChan chan fastrpc.Serializable
	setReplyChan chan fastrpc.Serializable
	getRPC       uint8
	setRPC       uint8
	getReplyRPC  uint8
	setReplyRPC  uint8

	// Paxos
	rmwGetChan      chan fastrpc.Serializable
	rmwGetReplyChan chan fastrpc.Serializable
	rmwSetChan      chan fastrpc.Serializable
	rmwSetReplyChan chan fastrpc.Serializable
	rmwGetRPC       uint8
	rmwGetReplyRPC  uint8
	rmwSetRPC       uint8
	rmwSetReplyRPC  uint8

	IsLeader bool // does this replica think it is the leader
	Shutdown bool
	data     map[int]pineappleproto.Payload
	// prev // value & carstamp generated by previously executed RMWs
	instanceSpace []*Instance // the space of all instances (used and not yet used)
	defaultBallot int32       // default ballot for new instances (0 until a Prepare(ballot, instance->infinity) from a leader)
	crtInstance   int32       // highest used instance number that this replica knows about

	flush         bool
	committedUpTo int32
}

type Instance struct {
	cmds         []state.Command
	initialTag   pineappleproto.Tag
	receivedRMW  pineappleproto.Payload
	receivedData []pineappleproto.Payload
	ballot       int32
	status       InstanceStatus
	lb           *LeaderBookkeeping
}

type LeaderBookkeeping struct {
	clientProposals []*genericsmr.Propose
	maxRecvBallot   int32
	getOKs          int
	setOKs          int
	getDone         bool // has get phase been completed
	prepareOKs      int
	rmwGetOKs       int
	rmwSetOKs       int
	rmwGetDone      bool // has rmwGet phase been completed
	nacks           int
	completed       bool
}

func NewReplica(id int, peerAddrList []string, exec bool, dreply bool) *Replica {
	// extends a normal replica
	r := &Replica{
		genericsmr.NewReplica(id, peerAddrList, exec, dreply),
		make(chan fastrpc.Serializable, CHAN_BUFFER_SIZE),
		make(chan fastrpc.Serializable, CHAN_BUFFER_SIZE),
		make(chan fastrpc.Serializable, CHAN_BUFFER_SIZE),
		make(chan fastrpc.Serializable, 3*CHAN_BUFFER_SIZE),
		0,
		0,
		0,
		0,
		make(chan fastrpc.Serializable, CHAN_BUFFER_SIZE),
		make(chan fastrpc.Serializable, CHAN_BUFFER_SIZE),
		make(chan fastrpc.Serializable, CHAN_BUFFER_SIZE),
		make(chan fastrpc.Serializable, CHAN_BUFFER_SIZE),
		0,
		0,
		0,
		0,

		false,
		false,
		map[int]pineappleproto.Payload{},
		make([]*Instance, 20*1024*1024),
		0,
		0,

		false,
		0,
	}

	// ABD
	r.getRPC = r.RegisterRPC(new(pineappleproto.Get), r.getChan)
	r.setRPC = r.RegisterRPC(new(pineappleproto.Set), r.setChan)
	r.getReplyRPC = r.RegisterRPC(new(pineappleproto.GetReply), r.getReplyChan)
	r.setReplyRPC = r.RegisterRPC(new(pineappleproto.SetReply), r.setReplyChan)

	// Paxos
	r.rmwGetRPC = r.RegisterRPC(new(pineappleproto.RMWGet), r.rmwGetChan)
	r.rmwGetReplyRPC = r.RegisterRPC(new(pineappleproto.RMWGetReply), r.rmwGetReplyChan)
	r.rmwSetRPC = r.RegisterRPC(new(pineappleproto.RMWSet), r.rmwSetChan)
	r.rmwSetReplyRPC = r.RegisterRPC(new(pineappleproto.RMWSetReply), r.rmwSetReplyChan)

	go r.Run()

	return r
}

// Compare two tags, returning true if the received tag is larger.
// A tag is larger than another if it has a higher timestamp.
// If both tags have the same timestamp, the tag with the Paxos leader id is smaller
func (r *Replica) isLargerTag(currentTag pineappleproto.Tag, receivedTag pineappleproto.Tag) bool {
	if receivedTag.Timestamp > currentTag.Timestamp {
		log.Println("larger 1, received: ", receivedTag.Timestamp, " own: ", currentTag.Timestamp)
		return true
	} else if receivedTag.Timestamp == currentTag.Timestamp {
		// tags are identical
		if currentTag.ID == receivedTag.ID {
			return false
		} else if r.IsLeader && currentTag.ID == int(r.Id) {
			log.Println("this is true")
			// if the replica is the leader and the tag has its id, prefer the receivedTag
			return true
		} else {
			return currentTag.ID < receivedTag.ID
		}
	}
	return false
}

// Reply to client during ABD
func (r *Replica) replyClient(instance int32) {
	inst := r.instanceSpace[instance]
	if inst.lb.clientProposals != nil && r.Dreply && !inst.lb.completed {
		propreply := &genericsmrproto.ProposeReplyTS{
			OK:        TRUE,
			CommandId: inst.lb.clientProposals[0].CommandId,
			Value:     state.NIL,
			Timestamp: inst.lb.clientProposals[0].Timestamp}
		r.ReplyProposeTS(propreply, inst.lb.clientProposals[0].Reply)
		inst.lb.completed = true
	}
}

func (r *Replica) replyRMWGet(replicaId int32, reply *pineappleproto.RMWGetReply) {
	r.SendMsg(replicaId, r.rmwGetReplyRPC, reply)
}

func (r *Replica) replyRMWSet(replicaId int32, reply *pineappleproto.RMWSetReply) {
	r.SendMsg(replicaId, r.rmwSetReplyRPC, reply)
}

func (r *Replica) replyGet(replicaId int32, reply *pineappleproto.GetReply) {
	r.SendMsg(replicaId, r.getReplyRPC, reply)
}

func (r *Replica) replySet(replicaId int32, reply *pineappleproto.SetReply) {
	r.SendMsg(replicaId, r.setReplyRPC, reply)
}

// Get Phase (Coordinator)
// Broadcasts query to all replicas to get value-tag pairs
func (r *Replica) bcastGet(instance int32, write bool, key int) {
	defer func() {
		if err := recover(); err != nil {
			log.Println("Prepare broadcast failed: ", err)
		}
	}()
	wr := FALSE
	data := pineappleproto.Payload{}
	if write {
		wr = TRUE
	} else { //reading, send data
		data = r.data[key]
	}

	args := &pineappleproto.Get{ReplicaID: r.Id, Instance: instance,
		Write: wr, Key: key, Payload: data}
	replicaCount := r.N - 1
	q := r.Id
	// Send to each connected replica
	for sentCount := 0; sentCount < replicaCount; sentCount++ {
		q = (q + 1) % int32(r.N)
		if q == r.Id {
			break
		}
		if !r.Alive[q] {
			continue
		}

		r.SendMsg(q, r.getRPC, args)
	}
}

// ABD reply to get query
// Returns replica's value-tag pair to requester
func (r *Replica) handleGet(get *pineappleproto.Get) {
	var getReply *pineappleproto.GetReply
	ok := TRUE
	data, doesExist := r.data[get.Key]

	// Return the most recent data held by storage node only if READ, since payload would be overwritten in write
	if get.Write == 0 {
		if !doesExist || r.isLargerTag(data.Tag, get.Payload.Tag) {
			// Replica has smaller tag, return received value
			r.data[get.Key] = get.Payload
			getReply = &pineappleproto.GetReply{Instance: get.Instance, OK: ok,
				Write: get.Write, Key: get.Key, Payload: get.Payload,
			}
		} else { // Replica has larger tag, send its data
			getReply = &pineappleproto.GetReply{Instance: get.Instance, OK: ok,
				Write: get.Write, Key: get.Key, Payload: data,
			}
		}
	} else { // init with empty payload
		getReply = &pineappleproto.GetReply{Instance: get.Instance, OK: ok, Write: get.Write,
			Key: get.Key, Payload: pineappleproto.Payload{},
		}
	}

	r.replyGet(get.ReplicaID, getReply)
}

// Chooses the most recent vt pair after waiting for majority ACKs (or increment timestamp if write)
func (r *Replica) handleGetReply(getReply *pineappleproto.GetReply) {
	inst := r.instanceSpace[getReply.Instance]
	if inst.lb.getDone { // avoid proceeding to set phase several times
		return
	}

	r.instanceSpace[getReply.Instance].receivedData =
		append(r.instanceSpace[getReply.Instance].receivedData, getReply.Payload)

	// Send the new vt pair to all nodes after getting majority
	if getReply.OK == TRUE {
		inst.lb.getOKs++

		if inst.lb.getOKs+1 > r.N>>1 {
			key := getReply.Key
			identicalCount := 0                                     // keep track of the count of identical responses
			ownTag := r.instanceSpace[getReply.Instance].initialTag // this node's own tag
			//ownTag := r.data[getReply.Key].Tag
			// Find the largest received timestamp
			for _, data := range r.instanceSpace[getReply.Instance].receivedData {
				if r.isLargerTag(ownTag, data.Tag) { // received value has larger tag
					//if r.isLargerTag(r.data[key].Tag, data.Tag) { // received value has larger tag
					log.Println("own: ", r.data[key].Tag, "rec.: ", data.Tag)
					r.data[key] = getReply.Payload
				}
				// tracks if all responses are identical by comparing to own tag
				// since the received quorum includes itself
				if data.Tag == ownTag {
					identicalCount++
				}
			}
			receivedDataCount := len(r.instanceSpace[getReply.Instance].receivedData)
			r.instanceSpace[getReply.Instance].receivedData = nil // clear slice, no longer needed
			inst.lb.getDone = true                                // getPhase completed

			// Optimized read; don't proceed to set if the quorum all has the latest timestamp
			if (getReply.Write == 0) && (identicalCount == receivedDataCount) {
				r.replyClient(getReply.Instance)
				return
			}
			log.Println("no optimized read")

			write := false
			inst.status = PREPARED
			inst.lb.nacks = 0
			// If writing, choose a higher unique timestamp (by adjoining replica ID with Timestamp++)
			if getReply.Write == 1 {
				log.Println("tag initially: ", r.data[key].Tag.Timestamp)
				write = true
				newTag := pineappleproto.Tag{Timestamp: r.data[key].Tag.Timestamp + 1, ID: int(r.Id)}
				r.data[key] = pineappleproto.Payload{Tag: newTag, Value: r.data[key].Value}
			}
			r.sync()
			r.bcastSet(getReply.Instance, write, key, r.data[key])
		}
	}
}

// Set Phase (Coordinator)
// Broadcasts to all replicas to write sent payload
func (r *Replica) bcastSet(instance int32, write bool, key int, payload pineappleproto.Payload) {
	defer func() {
		if err := recover(); err != nil {
			log.Println("Prepare bcast failed:", err)
		}
	}()

	wr := FALSE
	if write {
		wr = TRUE
	}
	args := &pineappleproto.Set{ReplicaID: r.Id, Instance: instance, Write: wr,
		Key: key, Payload: payload,
	}

	replicaCount := r.N - 1
	q := r.Id

	// Send to each connected replica
	for sentCount := 0; sentCount < replicaCount; sentCount++ {
		q = (q + 1) % int32(r.N)
		if q == r.Id {
			break
		}
		if !r.Alive[q] {
			continue
		}

		r.SendMsg(q, r.setRPC, args)
	}
}

// ABD Set phase
// Handle set query from coordinator
func (r *Replica) handleSet(set *pineappleproto.Set) {
	var setReply *pineappleproto.SetReply

	// Sets received payload if largest tag seen
	if r.isLargerTag(r.data[set.Key].Tag, set.Payload.Tag) {
		r.data[set.Key] = set.Payload
	}
	log.Println("key: ", set.Key, " from: ", set.ReplicaID, " new timestamp: ", r.data[set.Key].Tag.Timestamp)

	setReply = &pineappleproto.SetReply{Instance: set.Instance}

	//r.sync()
	r.replySet(set.ReplicaID, setReply)
}

// Response handler for Set request on nodes
func (r *Replica) handleSetReply(setReply *pineappleproto.SetReply) {
	inst := r.instanceSpace[setReply.Instance]
	inst.lb.setOKs++

	// Wait for a majority of acknowledgements
	if inst.lb.setOKs+1 > r.N>>1 {
		r.replyClient(setReply.Instance)
	}
}

var pRMWGet pineappleproto.RMWGet

func (r *Replica) bcastRMWGet(instance int32, ballot int32, command []state.Command) {
	defer func() {
		if err := recover(); err != nil {
			log.Println("Accept bcast failed:", err)
		}
	}()
	pRMWGet.LeaderId = r.Id
	pRMWGet.Instance = instance
	pRMWGet.Ballot = ballot
	pRMWGet.Command = command
	args := &pRMWGet

	n := r.N - 1
	q := r.Id
	for sent := 0; sent < n; {
		q = (q + 1) % int32(r.N)
		if q == r.Id {
			break
		}
		if !r.Alive[q] {
			continue
		}
		sent++
		r.SendMsg(q, r.rmwGetRPC, args)
	}
}

func (r *Replica) handleRMWGet(rmwGet *pineappleproto.RMWGet) {
	inst := r.instanceSpace[rmwGet.Instance]
	key := int(rmwGet.Command[0].K)

	var rmwGetReply *pineappleproto.RMWGetReply

	if inst == nil {
		if rmwGet.Ballot < r.defaultBallot {
			panic("outdated ballot received")
		} else {
			r.instanceSpace[rmwGet.Instance] = &Instance{
				cmds:   rmwGet.Command,
				ballot: rmwGet.Ballot,
				status: ACCEPTED,
				lb:     nil,
			}
			rmwGetReply = &pineappleproto.RMWGetReply{Instance: rmwGet.Instance, Ballot: r.defaultBallot, Key: key}
		}
	} else if rmwGet.Ballot < inst.ballot {
		panic("outdated ballot received")
	} else {
		// reordered ACCEPT
		r.instanceSpace[rmwGet.Instance].cmds = rmwGet.Command
		if r.instanceSpace[rmwGet.Instance].status != COMMITTED {
			r.instanceSpace[rmwGet.Instance].status = ACCEPTED
		}
		data := r.data[key]
		rmwGetReply = &pineappleproto.RMWGetReply{Instance: rmwGet.Instance, Ballot: r.defaultBallot, Key: key, Payload: data}
	}

	r.replyRMWGet(rmwGet.LeaderId, rmwGetReply)
}

// Chooses the most recent vt pair after waiting for majority ACKs (or increment timestamp if write)
func (r *Replica) handleRMWGetReply(rmwGetReply *pineappleproto.RMWGetReply) {
	inst := r.instanceSpace[rmwGetReply.Instance]
	if inst.lb.rmwGetDone { // avoid calling handleRMWSet more than once
		return
	}

	r.instanceSpace[rmwGetReply.Instance].receivedData =
		append(r.instanceSpace[rmwGetReply.Instance].receivedData, rmwGetReply.Payload)

	inst.lb.rmwGetOKs++

	if inst.lb.rmwGetOKs+1 > r.N>>1 { // quorom of messages received
		key := rmwGetReply.Key

		// Find the largest received timestamp
		for _, data := range r.instanceSpace[rmwGetReply.Instance].receivedData {
			if r.isLargerTag(r.data[key].Tag, data.Tag) { // received value has larger tag
				r.data[key] = rmwGetReply.Payload
			}
		}

		r.instanceSpace[rmwGetReply.Instance].receivedData = nil // clear slice, no longer needed
		inst.lb.rmwGetDone = true                                // rmwGet phase completed

		inst.lb.nacks = 0
		// If writing, choose a higher unique timestamp (by adjoining replica ID with Timestamp++)
		newTag := pineappleproto.Tag{Timestamp: r.data[key].Tag.Timestamp + 1, ID: int(r.Id)}
		newValue := r.data[key].Value + 1 // TODO: update RMW modify
		r.data[key] = pineappleproto.Payload{Tag: newTag, Value: newValue}

		r.recordInstanceMetadata(r.instanceSpace[rmwGetReply.Instance])
		r.recordCommands(r.instanceSpace[rmwGetReply.Instance].cmds)
		r.sync()

		r.bcastRMWSet(rmwGetReply.Instance, rmwGetReply.Ballot, key)
	}
}

var pRMWSet pineappleproto.RMWSet

func (r *Replica) bcastRMWSet(instance int32, ballot int32, key int) {
	defer func() {
		if err := recover(); err != nil {
			log.Println("Accept bcast failed:", err)
		}
	}()
	pRMWSet.LeaderId = r.Id
	pRMWSet.Instance = instance
	pRMWSet.Ballot = ballot
	pRMWSet.Command = r.instanceSpace[instance].cmds
	pRMWSet.Key = key
	pRMWSet.Payload = r.data[key]
	args := &pRMWSet

	n := r.N - 1
	q := r.Id

	for sent := 0; sent < n; {
		q = (q + 1) % int32(r.N)
		if q == r.Id {
			break
		}
		if !r.Alive[q] {
			continue
		}
		sent++
		r.SendMsg(q, r.rmwSetRPC, args)
	}
}

func (r *Replica) handleRMWSet(rmwSet *pineappleproto.RMWSet) {
	inst := r.instanceSpace[rmwSet.Instance]

	var rmwSetReply *pineappleproto.RMWSetReply

	if inst == nil {
		if rmwSet.Ballot < r.defaultBallot {
			panic("outdated ballot received")
		} else {
			r.instanceSpace[rmwSet.Instance] = &Instance{
				cmds:   rmwSet.Command,
				ballot: rmwSet.Ballot,
				status: ACCEPTED,
				lb:     nil,
			}
			inst = r.instanceSpace[rmwSet.Instance]
			rmwSetReply = &pineappleproto.RMWSetReply{Instance: rmwSet.Instance, OK: TRUE, Ballot: r.defaultBallot}
		}
	} else if inst.ballot > rmwSet.Ballot {
		panic("outdated ballot received")
	} else if inst.ballot < rmwSet.Ballot {
		inst.cmds = rmwSet.Command
		inst.ballot = rmwSet.Ballot
		inst.status = ACCEPTED
		rmwSetReply = &pineappleproto.RMWSetReply{Instance: rmwSet.Instance, OK: TRUE, Ballot: r.defaultBallot}
	} else {
		// reordered ACCEPT
		r.instanceSpace[rmwSet.Instance].cmds = rmwSet.Command
		if r.instanceSpace[rmwSet.Instance].status != COMMITTED {
			r.instanceSpace[rmwSet.Instance].status = ACCEPTED
		}
		rmwSetReply = &pineappleproto.RMWSetReply{Instance: rmwSet.Instance, OK: TRUE, Ballot: r.defaultBallot}
	}
	inst.receivedRMW = rmwSet.Payload // store received object in instance space
	if r.isLargerTag(r.data[rmwSet.Key].Tag, inst.receivedRMW.Tag) {
		r.data[rmwSet.Key] = inst.receivedRMW
	}

	r.replyRMWSet(rmwSet.LeaderId, rmwSetReply)
}

// Response handler for Set request on nodes
func (r *Replica) handleRMWSetReply(rmwSetReply *pineappleproto.RMWSetReply) {
	inst := r.instanceSpace[rmwSetReply.Instance]

	inst.lb.rmwSetOKs++

	// Wait for a majority of acknowledgements
	if inst.lb.rmwSetOKs+1 > r.N>>1 {
		if inst.lb.clientProposals != nil && r.Dreply && !inst.lb.completed {
			propreply := &genericsmrproto.ProposeReplyTS{
				OK:        TRUE,
				CommandId: inst.lb.clientProposals[0].CommandId,
				Value:     state.NIL,
				Timestamp: inst.lb.clientProposals[0].Timestamp}
			inst.lb.completed = true
			r.ReplyProposeTS(propreply, inst.lb.clientProposals[0].Reply)
		}
	}

}

func (r *Replica) handlePropose(propose *genericsmr.Propose) {
	/*
		if !r.IsLeader {
			preply := &genericsmrproto.ProposeReplyTS{TRUE, -1, state.NIL, 0}
			r.ReplyProposeTS(preply, propose.Reply)
			return
		}
	*/
	for r.instanceSpace[r.crtInstance] != nil {
		r.crtInstance++
	}

	instNo := r.crtInstance

	cmds := make([]state.Command, 1)
	proposals := make([]*genericsmr.Propose, 1)
	key := int(propose.Command.K)
	cmds[0] = propose.Command
	proposals[0] = propose

	// ABD
	r.instanceSpace[instNo] = &Instance{
		cmds:   cmds,
		ballot: 0,
		status: PREPARING,
		lb:     &LeaderBookkeeping{clientProposals: proposals, getDone: false, completed: false},
	}

	// Use Paxos if operation is not Read / Write
	if propose.Command.Op != state.PUT && propose.Command.Op != state.GET {
		r.instanceSpace[instNo] = &Instance{
			cmds:   cmds,
			ballot: 0,
			status: PREPARING,
			lb:     &LeaderBookkeeping{clientProposals: proposals, completed: false},
		}
		r.bcastRMWGet(instNo, 0, cmds)
	} else { // use ABD
		// Construct the pineapple payload from proposal data
		if propose.Command.Op == state.PUT { // write operation
			r.bcastGet(instNo, true, key)
		} else if propose.Command.Op == state.GET { // read operation
			_, doesExist := r.data[key]
			if doesExist {
				r.instanceSpace[instNo].initialTag = r.data[key].Tag
			} else {
				tag := pineappleproto.Tag{Timestamp: 0, ID: int(r.Id)}
				r.instanceSpace[instNo].initialTag = tag
				r.data[key] = pineappleproto.Payload{Tag: tag, Value: 0}
			}
			r.bcastGet(instNo, false, key)
		}
	}
}

var clockChan chan bool

func (r *Replica) updateCommittedUpTo() {
	for r.instanceSpace[r.committedUpTo+1] != nil &&
		r.instanceSpace[r.committedUpTo+1].status == COMMITTED {
		r.committedUpTo++
	}
}

// append a log entry to stable storage
func (r *Replica) recordInstanceMetadata(inst *Instance) {
	if !r.Durable {
		return
	}

	var b [5]byte
	binary.LittleEndian.PutUint32(b[0:4], uint32(inst.ballot))
	b[4] = byte(inst.status)
	r.StableStore.Write(b[:])
}

// write a sequence of commands to stable storage
func (r *Replica) recordCommands(cmds []state.Command) {
	if !r.Durable {
		return
	}

	if cmds == nil {
		return
	}
	for i := 0; i < len(cmds); i++ {
		cmds[i].Marshal(io.Writer(r.StableStore))
	}
}

// sync with the stable store
func (r *Replica) sync() {
	if !r.Durable {
		return
	}

	r.StableStore.Sync()
}

func (r *Replica) clock() {
	for !r.Shutdown {
		time.Sleep(CLOCK)
		clockChan <- true
	}
}

// Run main processing loop
func (r *Replica) Run() {
	r.ConnectToPeers()

	log.Println("Waiting for client connections")

	go r.WaitForClientConnections()

	clockChan = make(chan bool, 1)
	go r.clock()

	// We don't directly access r.ProposeChan, because we want to do pipelining periodically,
	// so we introduce a channel pointer: onOffProposChan:
	onOffProposeChan := r.ProposeChan

	for !r.Shutdown {

		select {
		case <-clockChan:
			// activate the new proposals channel
			onOffProposeChan = r.ProposeChan
			break
		case setS := <-r.setChan:
			set := setS.(*pineappleproto.Set)
			//got a Write message
			r.handleSet(set)
			break
		case getS := <-r.getChan:
			get := getS.(*pineappleproto.Get)
			//got a Read message
			r.handleGet(get)
			break
		case setReplyS := <-r.setReplyChan:
			setReply := setReplyS.(*pineappleproto.SetReply)
			//got a Write reply
			r.handleSetReply(setReply)
			break
		case getReplyS := <-r.getReplyChan:
			getReply := getReplyS.(*pineappleproto.GetReply)
			//got a Read reply
			r.handleGetReply(getReply)
			break
		case propose := <-onOffProposeChan:
			//got a Propose from a client
			// Handle proposal: single read-write object goes to ABD, multi read/write or RMW goes to Paxos
			r.handlePropose(propose)
			// deactivate the new proposals channel to prioritize the handling of protocol messages
			onOffProposeChan = nil
			break
		case rmwGetS := <-r.rmwGetChan:
			rmwGet := rmwGetS.(*pineappleproto.RMWGet)
			//got an RMWGet message
			r.handleRMWGet(rmwGet)
			break
		case rmwGetReplyS := <-r.rmwGetReplyChan:
			rmwGetReply := rmwGetReplyS.(*pineappleproto.RMWGetReply)
			//got an RMWGet reply
			r.handleRMWGetReply(rmwGetReply)
			break
		case rmwSetS := <-r.rmwSetChan:
			rmwSet := rmwSetS.(*pineappleproto.RMWSet)
			//got an Accept message
			r.handleRMWSet(rmwSet)
			break
		case rmwSetReplyS := <-r.rmwSetReplyChan:
			rmwSetReply := rmwSetReplyS.(*pineappleproto.RMWSetReply)
			//got an Accept reply
			r.handleRMWSetReply(rmwSetReply)
			break
		}
	}
}

/* RPC to be called by master */
func (r *Replica) BeTheLeader(args *genericsmrproto.BeTheLeaderArgs, reply *genericsmrproto.BeTheLeaderReply) error {
	r.IsLeader = true
	return nil
}
