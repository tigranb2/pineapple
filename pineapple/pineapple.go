package pineapple

import (
	"encoding/binary"
	"io"
	"log"
	"time"

	"pineapple/fastrpc"
	"pineapple/genericsmr"
	"pineapple/genericsmrproto"
	"pineapple/pineappleproto"
	"pineapple/state"
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
	prepareChan      chan fastrpc.Serializable
	acceptChan       chan fastrpc.Serializable
	prepareReplyChan chan fastrpc.Serializable
	acceptReplyChan  chan fastrpc.Serializable
	commitChan       chan fastrpc.Serializable
	commitShortChan  chan fastrpc.Serializable
	prepareRPC       uint8
	acceptRPC        uint8
	prepareReplyRPC  uint8
	acceptReplyRPC   uint8
	commitRPC        uint8
	commitShortRPC   uint8

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
		make(chan fastrpc.Serializable, CHAN_BUFFER_SIZE),
		make(chan fastrpc.Serializable, CHAN_BUFFER_SIZE),
		0,
		0,
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
	r.prepareRPC = r.RegisterRPC(new(pineappleproto.Prepare), r.prepareChan)
	r.acceptRPC = r.RegisterRPC(new(pineappleproto.Accept), r.acceptChan)
	r.prepareReplyRPC = r.RegisterRPC(new(pineappleproto.PrepareReply), r.prepareReplyChan)
	r.acceptReplyRPC = r.RegisterRPC(new(pineappleproto.AcceptReply), r.acceptReplyChan)
	r.commitRPC = r.RegisterRPC(new(pineappleproto.Commit), r.commitChan)
	r.commitShortRPC = r.RegisterRPC(new(pineappleproto.CommitShort), r.commitShortChan)

	go r.Run()

	return r
}

func (r *Replica) replyPrepare(replicaId int32, reply *pineappleproto.PrepareReply) {
	r.SendMsg(replicaId, r.prepareReplyRPC, reply)
}

func (r *Replica) replyAccept(replicaId int32, reply *pineappleproto.AcceptReply) {
	r.SendMsg(replicaId, r.acceptReplyRPC, reply)
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
	if write {
		wr = TRUE
	}
	args := &pineappleproto.Get{ReplicaID: r.Id, Instance: instance, Write: wr, Key: key}

	replicaCount := r.N - 1
	q := r.Id
	log.Println("Broadcasting key: ", key)
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
	var command state.Command
	ok := TRUE
	data, doesExist := r.data[get.Key]

	// If init or payload is empty, simply return empty payload
	if r.instanceSpace[r.crtInstance] == nil || !doesExist { // TODO: Is this block needed?
		getReply = &pineappleproto.GetReply{Instance: get.Instance, OK: ok, Write: get.Write,
			Key: get.Key, Payload: pineappleproto.Payload{}, // TODO: test removing payload
		}
		r.replyGet(get.ReplicaID, getReply)
		return
	}

	// Return the most recent data held by storage node only if READ, since payload would be overwritten in write
	if get.Write == 0 { // TODO: This was changed to 0, ensure no issues arise
		getReply = &pineappleproto.GetReply{Instance: get.Instance, OK: ok, Write: get.Write,
			Key: get.Key, Payload: data,
		}
		command.Op = 1
	} else { // init with empty payload
		getReply = &pineappleproto.GetReply{Instance: get.Instance, OK: ok, Write: get.Write,
			Key: get.Key, Payload: pineappleproto.Payload{}, // TODO: test removing payload
		}
	}

	/*
		cmds := make([]state.Command, 1)

			if getReply.OK == TRUE {
				r.recordCommands(cmds)
				r.sync()
			}
	*/

	r.replyGet(get.ReplicaID, getReply)
}

// Chooses the most recent vt pair after waiting for majority ACKs (or increment timestamp if write)
func (r *Replica) handleGetReply(getReply *pineappleproto.GetReply) {
	inst := r.instanceSpace[getReply.Instance]
	key := getReply.Key

	r.instanceSpace[getReply.Instance].receivedData =
		append(r.instanceSpace[getReply.Instance].receivedData, getReply.Payload)

	// Send the new vt pair to all nodes after getting majority
	if getReply.OK == TRUE {
		inst.lb.getOKs++

		if inst.lb.getOKs+1 > r.N>>1 {
			// Find the largest received timestamp
			for _, data := range r.instanceSpace[getReply.Instance].receivedData {
				if data.Tag.Timestamp > r.data[key].Tag.Timestamp {
					r.data[key] = getReply.Payload
				}
			}

			r.instanceSpace[getReply.Instance].receivedData = nil // clear slice, no longer needed

			write := false
			inst.status = PREPARED
			inst.lb.nacks = 0
			// If writing, choose a higher unique timestamp (by adjoining replica ID with Timestamp++)
			if getReply.Write == 1 {
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

	// Sets received payload if latest timestamp seen
	if set.Payload.Tag.Timestamp > r.data[set.Key].Tag.Timestamp {
		r.data[set.Key] = set.Payload
	}

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
		if inst.lb.clientProposals != nil && r.Dreply && !inst.lb.completed {
			propreply := &genericsmrproto.ProposeReplyTS{
				OK:        TRUE,
				CommandId: inst.lb.clientProposals[0].CommandId,
				Value:     state.NIL,
				Timestamp: inst.lb.clientProposals[0].Timestamp}
			r.ReplyProposeTS(propreply, inst.lb.clientProposals[0].Reply)
			inst.lb.completed = true
		}

		//r.sync() //is this necessary?
	}

}

func (r *Replica) bcastPrepare(instance int32, ballot int32, toInfinity bool) {
	defer func() {
		if err := recover(); err != nil {
			log.Println("Prepare bcast failed:", err)
		}
	}()
	ti := FALSE
	if toInfinity {
		ti = TRUE
	}
	args := &pineappleproto.Prepare{LeaderId: r.Id, Instance: instance, Ballot: ballot, ToInfinity: ti}

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
		r.SendMsg(q, r.prepareRPC, args)
	}
}

var pa pineappleproto.Accept

func (r *Replica) bcastAccept(instance int32, ballot int32, command []state.Command) {
	defer func() {
		if err := recover(); err != nil {
			log.Println("Accept bcast failed:", err)
		}
	}()
	pa.LeaderId = r.Id
	pa.Instance = instance
	pa.Ballot = ballot
	pa.Command = command
	args := &pa
	//args := &paxosproto.Accept{r.Id, instance, ballot, command}

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
		r.SendMsg(q, r.acceptRPC, args)
	}
}

var pc pineappleproto.Commit
var pcs pineappleproto.CommitShort

func (r *Replica) bcastCommit(instance int32, ballot int32, command []state.Command) {
	defer func() {
		if err := recover(); err != nil {
			log.Println("Commit bcast failed:", err)
		}
	}()
	pc.LeaderId = r.Id
	pc.Instance = instance
	pc.Ballot = ballot
	pc.Command = command
	args := &pc
	pcs.LeaderId = r.Id
	pcs.Instance = instance
	pcs.Ballot = ballot
	pcs.Count = int32(len(command))
	argsShort := &pcs

	//args := &paxosproto.Commit{r.Id, instance, command}

	n := r.N - 1
	q := r.Id
	sent := 0

	for sent < n {
		q = (q + 1) % int32(r.N)
		if q == r.Id {
			break
		}
		if !r.Alive[q] {
			continue
		}
		sent++
		r.SendMsg(q, r.commitShortRPC, argsShort)
	}
	if q != r.Id {
		for sent < r.N-1 {
			q = (q + 1) % int32(r.N)
			if q == r.Id {
				break
			}
			if !r.Alive[q] {
				continue
			}
			sent++
			r.SendMsg(q, r.commitRPC, args)
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
	log.Println("Got: ", key, "; value: ", propose.Command.V)

	// ABD
	r.instanceSpace[instNo] = &Instance{
		cmds:   cmds,
		ballot: r.makeUniqueBallot(0),
		status: PREPARING,
		lb:     &LeaderBookkeeping{clientProposals: proposals, completed: false},
	}
	r.data[key] = pineappleproto.Payload{
		Tag:   pineappleproto.Tag{Timestamp: int(propose.Timestamp), ID: int(r.Id)},
		Value: int(propose.Command.V),
	}

	r.recordInstanceMetadata(r.instanceSpace[instNo])
	r.recordCommands(cmds)
	r.sync()

	log.Println("KEy: ", key, " op: ", propose.Command.Op)
	// Construct the pineapple payload from proposal data
	if propose.Command.Op == state.PUT { // write operation
		log.Println("Will bcast 1 key: ", key)
		r.bcastGet(instNo, true, key)
	} else if propose.Command.Op == state.GET { // read operation
		log.Println("Will bcast 2 key: ", key)
		r.bcastGet(instNo, false, key)
	}

	// Use Paxos if operation is not Read / Write
	if propose.Command.Op != state.PUT || propose.Command.Op != state.GET {
		if r.defaultBallot == -1 {
			r.instanceSpace[instNo] = &Instance{
				cmds:   cmds,
				ballot: r.makeUniqueBallot(0),
				status: PREPARING,
				lb:     &LeaderBookkeeping{clientProposals: proposals, completed: false},
			}
			r.bcastPrepare(instNo, r.makeUniqueBallot(0), true)
		} else {
			r.instanceSpace[instNo] = &Instance{
				cmds:   cmds,
				ballot: r.defaultBallot,
				status: PREPARED,
				lb:     &LeaderBookkeeping{clientProposals: proposals, completed: false},
			}

			r.recordInstanceMetadata(r.instanceSpace[instNo])
			r.recordCommands(cmds)
			r.sync()

			r.bcastAccept(instNo, r.defaultBallot, cmds)
		}
	}
	log.Println("Done with: ", key, ";  new val: ", r.data[key])
}

func (r *Replica) handlePrepare(prepare *pineappleproto.Prepare) {
	inst := r.instanceSpace[prepare.Instance]
	var preply *pineappleproto.PrepareReply

	if inst == nil {
		ok := TRUE
		if r.defaultBallot > prepare.Ballot {
			ok = FALSE
		}
		preply = &pineappleproto.PrepareReply{Instance: prepare.Instance, OK: ok,
			Ballot: r.defaultBallot, Command: make([]state.Command, 0)}
	} else {
		ok := TRUE
		if prepare.Ballot < inst.ballot {
			ok = FALSE
		}
		preply = &pineappleproto.PrepareReply{Instance: prepare.Instance, OK: ok,
			Ballot: inst.ballot, Command: inst.cmds}
	}

	r.replyPrepare(prepare.LeaderId, preply)

	if prepare.ToInfinity == TRUE && prepare.Ballot > r.defaultBallot {
		r.defaultBallot = prepare.Ballot
	}
}

func (r *Replica) handleAccept(accept *pineappleproto.Accept) {
	inst := r.instanceSpace[accept.Instance]
	var areply *pineappleproto.AcceptReply

	if inst == nil {
		if accept.Ballot < r.defaultBallot {
			areply = &pineappleproto.AcceptReply{Instance: accept.Instance, OK: FALSE, Ballot: r.defaultBallot}
		} else {
			r.instanceSpace[accept.Instance] = &Instance{
				cmds:   accept.Command,
				ballot: accept.Ballot,
				status: ACCEPTED,
				lb:     nil,
			}
			areply = &pineappleproto.AcceptReply{Instance: accept.Instance, OK: TRUE, Ballot: r.defaultBallot}
		}
	} else if inst.ballot > accept.Ballot {
		areply = &pineappleproto.AcceptReply{Instance: accept.Instance, OK: FALSE, Ballot: inst.ballot}
	} else if inst.ballot < accept.Ballot {
		inst.cmds = accept.Command
		inst.ballot = accept.Ballot
		inst.status = ACCEPTED
		areply = &pineappleproto.AcceptReply{Instance: accept.Instance, OK: TRUE, Ballot: inst.ballot}
		if inst.lb != nil && inst.lb.clientProposals != nil {
			//TODO: is this correct?
			// try the proposal in a different instance
			for i := 0; i < len(inst.lb.clientProposals); i++ {
				r.ProposeChan <- inst.lb.clientProposals[i]
			}
			inst.lb.clientProposals = nil
		}
	} else {
		// reordered ACCEPT
		r.instanceSpace[accept.Instance].cmds = accept.Command
		if r.instanceSpace[accept.Instance].status != COMMITTED {
			r.instanceSpace[accept.Instance].status = ACCEPTED
		}
		areply = &pineappleproto.AcceptReply{Instance: accept.Instance, OK: TRUE, Ballot: r.defaultBallot}
	}

	if areply.OK == TRUE {
		r.recordInstanceMetadata(r.instanceSpace[accept.Instance])
		r.recordCommands(accept.Command)
		r.sync()
	}

	r.replyAccept(accept.LeaderId, areply)
}

func (r *Replica) handleCommit(commit *pineappleproto.Commit) {
	inst := r.instanceSpace[commit.Instance]

	if inst == nil {
		r.instanceSpace[commit.Instance] = &Instance{
			cmds:   commit.Command,
			ballot: commit.Ballot,
			status: COMMITTED,
			lb:     nil,
		}
	} else {
		r.instanceSpace[commit.Instance].cmds = commit.Command
		r.instanceSpace[commit.Instance].status = COMMITTED
		r.instanceSpace[commit.Instance].ballot = commit.Ballot
		if inst.lb != nil && inst.lb.clientProposals != nil {
			for i := 0; i < len(inst.lb.clientProposals); i++ {
				r.ProposeChan <- inst.lb.clientProposals[i]
			}
			inst.lb.clientProposals = nil
		}
	}

	r.updateCommittedUpTo()

	r.recordInstanceMetadata(r.instanceSpace[commit.Instance])
	r.recordCommands(commit.Command)
}

func (r *Replica) handleCommitShort(commit *pineappleproto.CommitShort) {
	inst := r.instanceSpace[commit.Instance]

	if inst == nil {
		r.instanceSpace[commit.Instance] = &Instance{
			cmds:         nil,
			receivedData: nil,
			ballot:       commit.Ballot,
			status:       COMMITTED,
			lb:           nil,
		}
	} else {
		r.instanceSpace[commit.Instance].status = COMMITTED
		r.instanceSpace[commit.Instance].ballot = commit.Ballot
		if inst.lb != nil && inst.lb.clientProposals != nil {
			for i := 0; i < len(inst.lb.clientProposals); i++ {
				r.ProposeChan <- inst.lb.clientProposals[i]
			}
			inst.lb.clientProposals = nil
		}
	}

	r.updateCommittedUpTo()

	r.recordInstanceMetadata(r.instanceSpace[commit.Instance])
}

func (r *Replica) handlePrepareReply(preply *pineappleproto.PrepareReply) {
	inst := r.instanceSpace[preply.Instance]

	if inst.status != PREPARING {
		// TODO: should replies for non-current ballots be ignored?
		// we've moved on -- these are delayed replies, so just ignore
		return
	}

	if preply.OK == TRUE {
		inst.lb.getOKs++

		if preply.Ballot > inst.lb.maxRecvBallot {
			inst.cmds = preply.Command
			inst.lb.maxRecvBallot = preply.Ballot
			if inst.lb.clientProposals != nil {
				// there is already a competing command for this instance,
				// so we put the client proposal back in the queue so that
				// we know to try it in another instance
				for i := 0; i < len(inst.lb.clientProposals); i++ {
					r.ProposeChan <- inst.lb.clientProposals[i]
				}
				inst.lb.clientProposals = nil
			}
		}

		if inst.lb.getOKs+1 > r.N>>1 {
			inst.status = PREPARED
			inst.lb.nacks = 0
			if inst.ballot > r.defaultBallot {
				r.defaultBallot = inst.ballot
			}
			r.recordInstanceMetadata(r.instanceSpace[preply.Instance])
			r.sync()
			r.bcastAccept(preply.Instance, inst.ballot, inst.cmds)
		}
	} else {
		// TODO: there is probably another active leader
		inst.lb.nacks++
		if preply.Ballot > inst.lb.maxRecvBallot {
			inst.lb.maxRecvBallot = preply.Ballot
		}
		if inst.lb.nacks >= r.N>>1 {
			if inst.lb.clientProposals != nil {
				// try the proposals in another instance
				for i := 0; i < len(inst.lb.clientProposals); i++ {
					r.ProposeChan <- inst.lb.clientProposals[i]
				}
				inst.lb.clientProposals = nil
			}
		}
	}
}

func (r *Replica) handleAcceptReply(areply *pineappleproto.AcceptReply) {
	inst := r.instanceSpace[areply.Instance]

	if inst.status != PREPARED && inst.status != ACCEPTED {
		// we've move on, these are delayed replies, so just ignore
		return
	}

	if areply.OK == TRUE {
		inst.lb.setOKs++
		if inst.lb.setOKs+1 > r.N>>1 {
			inst = r.instanceSpace[areply.Instance]
			inst.status = COMMITTED
			if inst.lb.clientProposals != nil && !r.Dreply {
				// give client the all clear
				for i := 0; i < len(inst.cmds); i++ {
					propreply := &genericsmrproto.ProposeReplyTS{
						OK:        TRUE,
						CommandId: inst.lb.clientProposals[i].CommandId,
						Value:     state.NIL,
						Timestamp: inst.lb.clientProposals[i].Timestamp}
					r.ReplyProposeTS(propreply, inst.lb.clientProposals[i].Reply)
				}
			}

			r.recordInstanceMetadata(r.instanceSpace[areply.Instance])
			r.sync() //is this necessary?

			r.updateCommittedUpTo()

			r.bcastCommit(areply.Instance, inst.ballot, inst.cmds)
		}
	} else {
		// TODO: there is probably another active leader
		inst.lb.nacks++
		if areply.Ballot > inst.lb.maxRecvBallot {
			inst.lb.maxRecvBallot = areply.Ballot
		}
		if inst.lb.nacks >= r.N>>1 {
			// TODO
		}
	}
}

func (r *Replica) executeCommands() {
	i := int32(0)
	for !r.Shutdown {
		executed := false

		for i <= r.committedUpTo {
			if r.instanceSpace[i].cmds != nil {
				inst := r.instanceSpace[i]
				for j := 0; j < len(inst.cmds); j++ {
					val := inst.cmds[j].Execute(r.State)
					if r.Dreply && inst.lb != nil && inst.lb.clientProposals != nil {
						propreply := &genericsmrproto.ProposeReplyTS{
							OK:        TRUE,
							CommandId: inst.lb.clientProposals[j].CommandId,
							Value:     val,
							Timestamp: inst.lb.clientProposals[j].Timestamp}
						r.ReplyProposeTS(propreply, inst.lb.clientProposals[j].Reply)
					}
				}
				i++
				executed = true
			} else {
				break
			}
		}

		if !executed {
			time.Sleep(CLOCK)
		}
	}

}

var clockChan chan bool

func (r *Replica) makeUniqueBallot(ballot int32) int32 {
	return (ballot << 4) | r.Id
}

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
		case prepareS := <-r.prepareChan:
			prepare := prepareS.(*pineappleproto.Prepare)
			//got a Prepare message
			r.handlePrepare(prepare)
			break
		case acceptS := <-r.acceptChan:
			accept := acceptS.(*pineappleproto.Accept)
			//got an Accept message
			r.handleAccept(accept)
			break
		case prepareReplyS := <-r.prepareReplyChan:
			prepareReply := prepareReplyS.(*pineappleproto.PrepareReply)
			//got a Prepare reply
			r.handlePrepareReply(prepareReply)
			break
		case acceptReplyS := <-r.acceptReplyChan:
			acceptReply := acceptReplyS.(*pineappleproto.AcceptReply)
			//got an Accept reply
			r.handleAcceptReply(acceptReply)
			break
		}
	}
}

/* RPC to be called by master */
func (r *Replica) BeTheLeader(args *genericsmrproto.BeTheLeaderArgs, reply *genericsmrproto.BeTheLeaderReply) error {
	r.IsLeader = true
	return nil
}
