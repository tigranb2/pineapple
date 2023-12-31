package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"runtime"
	"sync"
	"time"

	"pineapple/src/genericsmrproto"
	"pineapple/src/poisson"
	"pineapple/src/state"
	"pineapple/src/zipfian"

	"golang.org/x/sync/semaphore"
)

var leaderAddr *string = flag.String("laddr", "10.10.1.2", "Leader address. Defaults to 10.10.1.2")
var leaderPort *int = flag.Int("lport", 7070, "Leader port.")
var serverAddr *string = flag.String("saddr", "", "Server address.")
var serverPort *int = flag.Int("sport", 7070, "Server port.")
var serverID *int = flag.Int("serverID", 0, "Server's ID")
var procs *int = flag.Int("p", 2, "GOMAXPROCS.")
var conflicts *int = flag.Int("c", 0, "Percentage of conflicts. If -1, uses Zipfian distribution.")
var forceLeader = flag.Int("l", -1, "Force client to talk to a certain replica.")
var startRange = flag.Int("sr", 0, "Key range start")
var T = flag.Int("T", 1, "Number of threads (simulated clients).")
var outstandingReqs = flag.Int64("or", 1, "Number of outstanding requests a thread can have at any given time.")
var theta = flag.Float64("theta", 0.99, "Theta zipfian parameter")
var zKeys = flag.Uint64("z", 1e9, "Number of unique keys in zipfian distribution.")
var poissonAvg = flag.Int("poisson", -1, "The average number of microseconds between requests. -1 disables Poisson.")
var percentWrites = flag.Float64("writes", 1, "A float between 0 and 1 that corresponds to the percentage of requests that should be writes. The remainder will be reads.")
var percentRMWs = flag.Float64("rmws", 0, "A float between 0 and 1 that corresponds to the percentage of writes that should be RMWs. The remainder will be regular writes.")
var tailAtScale *int = flag.Int("tailAtScale", -1, "Simulate storage request fan-out by performing <tailAtScale> requests and aggregating statistics.")
var blindWrites = flag.Bool("blindwrites", false, "True if writes don't need to execute before clients receive responses.")
var singleClusterTest = flag.Bool("singleClusterTest", true, "True if clients run on a VM in a single cluster")
var rampDown *int = flag.Int("rampDown", 5, "Length of the cool-down period after statistics are measured (in seconds).")
var rampUp *int = flag.Int("rampUp", 5, "Length of the warm-up period before statistics are measured (in seconds).")
var timeout *int = flag.Int("timeout", 180, "Length of the timeout used when running the client")

// Information about the latency of an operation
type response struct {
	receivedAt    time.Time
	rtt           float64 // The operation latency, in ms
	commitLatency float64 // The operation's commit latency, in ms
	operation     state.Operation
	replicaID     int
}

// Information pertaining to operations that have been issued but that have not
// yet received responses
type outstandingRequestInfo struct {
	sync.Mutex
	sema        *semaphore.Weighted // Controls number of outstanding operations
	startTimes  map[int32]time.Time // The time at which operations were sent out
	operation   map[int32]state.Operation
	tasBatch    map[int32]int32     // tasBatch id of the request
	maxLat      map[int32][]float64 // max latency of the tail at scale requests
	tasRecevied map[int32]int       // how many of the tas requests have been received
}

// An outstandingRequestInfo per client thread
var orInfos []*outstandingRequestInfo

func Max(a float64, b float64) float64 {
	if a > b {
		return a
	} else {
		return b
	}
}

func main() {
	flag.Parse()

	runtime.GOMAXPROCS(*procs)

	if *conflicts > 100 {
		log.Fatalf("Conflicts percentage must be between 0 and 100.\n")
	}

	orInfos = make([]*outstandingRequestInfo, *T)

	readings := make(chan *response, 100000)

	//startTime := rand.New(rand.NewSource(time.Now().UnixNano()))
	experimentStart := time.Now()

	for i := 0; i < *T; i++ {
		log.Println("Connected to node: ", *serverAddr)

		server, err := net.Dial("tcp", fmt.Sprintf("%s:%d", *serverAddr, *serverPort))
		if err != nil {
			log.Fatalf("Error connecting to replica %s:%d\n", *serverAddr, *serverPort)
		}

		reader := bufio.NewReader(server)
		writer := bufio.NewWriter(server)

		// TODO: init maps
		orInfo := &outstandingRequestInfo{
			sync.Mutex{},
			semaphore.NewWeighted(*outstandingReqs),
			make(map[int32]time.Time, *outstandingReqs),
			make(map[int32]state.Operation, *outstandingReqs),

			make(map[int32]int32),
			make(map[int32][]float64),
			make(map[int32]int),
		}

		if *serverID != 0 && *percentRMWs != 0 { // not already connected to leader
			leader, err := net.Dial("tcp", fmt.Sprintf("%s:%d", *leaderAddr, *leaderPort))
			if err != nil {
				log.Fatalf("Error connecting to replica %s:%d\n", *leaderAddr, *leaderPort)
			}

			lReader := bufio.NewReader(leader)
			lWriter := bufio.NewWriter(leader)

			go simulatedClientWriter(writer, lWriter, /* leader writer*/
				reader, lReader /* leader reader */, orInfo, readings, *serverID)
		} else {
			go simulatedClientWriter(writer, nil, /* leader writer*/
				reader, nil /* leader reader */, orInfo, readings, *serverID)
		}

		//waitTime := startTime.Intn(3)
		//time.Sleep(time.Duration(waitTime) * 100 * 1e6)

		orInfos[i] = orInfo
	}
	if *singleClusterTest {
		printerMultipleFile(readings, *serverID, experimentStart, rampDown, rampUp, timeout)
	} else {
		printer(readings)
	}
}

func simulatedClientWriter(writer *bufio.Writer, otherWriter *bufio.Writer, reader *bufio.Reader,
	otherReader *bufio.Reader, orInfo *outstandingRequestInfo, readings chan *response, serverID int) {
	args := genericsmrproto.Propose{
		CommandId: 0,
		Command:   state.Command{Op: state.PUT, K: 0, V: 1},
		Timestamp: 0,
	} // @audit autodetermine proposal type

	conflictRand := rand.New(rand.NewSource(time.Now().UnixNano()))
	zipf := zipfian.NewZipfianGenerator(*zKeys, *theta)
	poissonGenerator := poisson.NewPoisson(*poissonAvg)
	opRand := rand.New(rand.NewSource(time.Now().UnixNano()))

	queuedReqs := 0 // The number of poisson departures that have been missed

	coalescedOps := *tailAtScale // number of operations done in a batch
	if coalescedOps == -1 {
		coalescedOps = 1
	}
	tasBatch := int32(0) // id of all the sub-requests in a tail at scale batch

	for id := int32(0); ; id++ {
		for i := 0; i < coalescedOps; i++ {
			id += int32(i)
			args.CommandId = id

			// Determine key
			if *conflicts >= 0 {
				r := conflictRand.Intn(100)
				if r < *conflicts {
					args.Command.K = 42
				} else {
					//args.Command.K = state.Key(*startRange + 43 + int(id % 888))
					args.Command.K = state.Key(int32(*startRange) + 43 + id)
				}
			} else {
				args.Command.K = state.Key(zipf.NextNumber())
			}

			// Determine operation type
			randNumber := opRand.Float64()
			if *percentWrites+*percentRMWs > randNumber {
				if *percentWrites > randNumber {
					if !*blindWrites {
						args.Command.Op = state.PUT // write operation
					} else {
						//args.Command.Op = state.PUT_BLIND
					}
				} else if *percentRMWs > 0 {
					args.Command.Op = state.RMW // RMW operation
				}
			} else {
				args.Command.Op = state.GET // read operation
			}

			if *poissonAvg == -1 { // Poisson disabled
				orInfo.sema.Acquire(context.Background(), 1)
			} else {
				for {
					if orInfo.sema.TryAcquire(1) {
						if queuedReqs == 0 {
							time.Sleep(poissonGenerator.NextArrival())
						} else {
							queuedReqs -= 1
						}
						break
					}
					time.Sleep(poissonGenerator.NextArrival())
					queuedReqs += 1
				}
			}

			before := time.Now()
			useLeader := args.Command.Op == state.RMW && serverID != 0
			if useLeader { // send RMWs to leader
				otherWriter.WriteByte(genericsmrproto.PROPOSE)
				args.Marshal(otherWriter)
				otherWriter.Flush()
				//} else if args.Command.Op == state.GET && serverID == 0 { // send leader's reads to VA
				//	otherWriter.WriteByte(genericsmrproto.PROPOSE)
				//	args.Marshal(otherWriter)
				//	otherWriter.Flush()
				//}
			} else {
				writer.WriteByte(genericsmrproto.PROPOSE)
				args.Marshal(writer)
				writer.Flush()
			}

			orInfo.Lock()
			orInfo.operation[id] = args.Command.Op
			orInfo.startTimes[id] = before
			orInfo.tasBatch[id] = tasBatch
			orInfo.Unlock()

			//
			// reader logic
			//
			var reply genericsmrproto.ProposeReplyTS

			for {
				var err error
				if useLeader { // read response from leader
					err = reply.Unmarshal(otherReader)
				} else {
					err = reply.Unmarshal(reader)
				}
				if err != nil || reply.OK == 0 {
					if err != nil {
						log.Println("Error during unmarshaling:", err)
					} else if reply.OK == 0 {
						log.Println("reply.OK is 0")
					}
					log.Println(reply.OK)
					log.Println(reply.CommandId)
					break
				}

				after := time.Now()
				orInfo.sema.Release(1)

				orInfo.Lock()
				start := orInfo.startTimes[reply.CommandId]
				operation := orInfo.operation[reply.CommandId]
				rtt := (after.Sub(start)).Seconds() * 1000
				delete(orInfo.startTimes, reply.CommandId)

				tasID := orInfo.tasBatch[reply.CommandId]
				orInfo.tasRecevied[tasID]++ // keep track of how many sub-requests have been received
				tasReceived := orInfo.tasRecevied[tasID]
				if len(orInfo.maxLat[tasID]) == 0 {
					orInfo.maxLat[tasID] = make([]float64, 4)
				}

				orInfo.maxLat[tasID][3] = Max(orInfo.maxLat[tasID][3], rtt) // keep track of largest latency
				if operation == state.PUT {
					orInfo.maxLat[tasID][0] = Max(orInfo.maxLat[tasID][0], rtt) // first element is largest write lat
				} else if operation == state.GET {
					orInfo.maxLat[tasID][1] = Max(orInfo.maxLat[tasID][1], rtt) // second element is largest read lat
				} else { // rmw
					orInfo.maxLat[tasID][2] = Max(orInfo.maxLat[tasID][2], rtt) // third element is largest rmw lat
				}
				maxLat := orInfo.maxLat[tasID]

				orInfo.Unlock()

				//commitToExec := float64(reply.Timestamp) / 1e6
				commitLatency := float64(0) //rtt - commitToExec

				// check if all sub-request responses received
				if tasReceived == *tailAtScale || *tailAtScale == -1 {
					for i, lat := range maxLat {
						if lat != 0 {
							if i == 0 { // write operation
								readings <- &response{
									after,
									lat,
									commitLatency,
									state.PUT,
									serverID,
								}
							} else if i == 1 { // read operation
								readings <- &response{
									after,
									lat,
									commitLatency,
									state.GET,
									serverID,
								}
							} else if i == 2 { // rmw operation
								readings <- &response{
									after,
									lat,
									commitLatency,
									state.RMW,
									serverID,
								}
							} else { // max
								readings <- &response{
									after,
									lat,
									commitLatency,
									state.MAX,
									serverID,
								}
							}
						}
					}
				}
				break
			}
		}
		tasBatch++
	}
}

func printer(readings chan *response) {
	lattputFile, err := os.Create("lattput.txt")
	if err != nil {
		log.Println("Error creating lattput file", err)
		return
	}
	//lattputFile.WriteString("# time (ns), avg lat over the past second, tput since last line, total count, totalOrs, avg commit lat over the past second\n")

	latFile, err := os.Create("latency.txt")
	if err != nil {
		log.Println("Error creating latency file", err)
		return
	}
	//latFile.WriteString("# time (ns), latency, commit latency\n")

	startTime := time.Now()

	for {
		time.Sleep(time.Second)
		count := len(readings)
		var sum float64 = 0
		var commitSum float64 = 0
		endTime := time.Now() // Set to current time in case there are no readings
		for i := 0; i < count; i++ {
			resp := <-readings
			// Log all to latency file
			latFile.WriteString(fmt.Sprintf("%d %f %f\n", resp.receivedAt.UnixNano(), resp.rtt, resp.commitLatency))
			sum += resp.rtt
			commitSum += resp.commitLatency
			endTime = resp.receivedAt
		}
		var avg float64
		var avgCommit float64
		var tput float64
		if count > 0 {
			avg = sum / float64(count)
			avgCommit = commitSum / float64(count)
			tput = float64(count) / endTime.Sub(startTime).Seconds()
		}

		totalOrs := 0
		for i := 0; i < *T; i++ {
			orInfos[i].Lock()
			totalOrs += len(orInfos[i].startTimes)
			orInfos[i].Unlock()
		}

		// Log summary to lattput file
		lattputFile.WriteString(fmt.Sprintf("%d %f %f %d %d %f\n", endTime.UnixNano(),
			avg, tput, count, totalOrs, avgCommit))

		startTime = endTime
	}
}

func printerMultipleFile(readings chan *response, replicaID int, experimentStart time.Time, rampDown, rampUp, timeout *int) {
	fileName := fmt.Sprintf("lattput-%d.txt", replicaID)
	//lattputFile, err := os.Create(fileName)
	//if err != nil {
	//	log.Println("Error creating lattput file", err)
	//	return
	//}
	//
	//fileName = fmt.Sprintf("latFileRead-%d.txt", replicaID)
	//latFileRead, err := os.Create(fileName)
	//if err != nil {
	//	log.Println("Error creating latency file", err)
	//	return
	//}
	////latFile.WriteString("# time (ns), latency, commit latency\n")
	//
	//fileName = fmt.Sprintf("latFileWrite-%d.txt", replicaID)
	//latFileWrite, err := os.Create(fileName)
	//if err != nil {
	//	log.Println("Error creating latency file", err)
	//	return
	//}
	//
	//fileName = fmt.Sprintf("latFileRMW-%d.txt", replicaID)
	//latFileRMW, err := os.Create(fileName)
	//if err != nil {
	//	log.Println("Error creating latency file", err)
	//	return
	//}

	fileName = fmt.Sprintf("latFileMAX-%d.txt", replicaID)
	latFileMAX, err := os.Create(fileName)
	if err != nil {
		log.Println("Error creating latency file", err)
		return
	}

	startTime := time.Now()

	for {
		time.Sleep(time.Second)

		count := len(readings)
		var sum float64 = 0
		var commitSum float64 = 0
		endTime := time.Now() // Set to current time in case there are no readings
		currentRuntime := time.Now().Sub(experimentStart)
		for i := 0; i < count; i++ {
			resp := <-readings
			// Log all to latency file if they are not within the ramp up or ramp down period.
			if *rampUp < int(currentRuntime.Seconds()) && int(currentRuntime.Seconds()) < *timeout-*rampDown {
				if resp.operation == state.GET {
					//fmt.Println(fmt.Sprintf("%d %f %f\n", resp.receivedAt.UnixNano(), resp.rtt, resp.commitLatency))
					//latFileRead.WriteString(fmt.Sprintf("%d %f %f\n", resp.receivedAt.UnixNano(), resp.rtt, resp.commitLatency))
				} else if resp.operation == state.PUT {
					//fmt.Println(fmt.Sprintf("%d %f %f\n", resp.receivedAt.UnixNano(), resp.rtt, resp.commitLatency))
					//latFileWrite.WriteString(fmt.Sprintf("%d %f %f\n", resp.receivedAt.UnixNano(), resp.rtt, resp.commitLatency))
				} else if resp.operation == state.RMW { // rmw
					//fmt.Println(fmt.Sprintf("%d %f %f\n", resp.receivedAt.UnixNano(), resp.rtt, resp.commitLatency))
					//latFileRMW.WriteString(fmt.Sprintf("%d %f %f\n", resp.receivedAt.UnixNano(), resp.rtt, resp.commitLatency))
				} else { // max
					latFileMAX.WriteString(fmt.Sprintf("%d %f %f\n", resp.receivedAt.UnixNano(), resp.commitLatency, resp.rtt))
				}
				sum += resp.rtt
				commitSum += resp.commitLatency
				endTime = resp.receivedAt
			}
		}

		var avg float64
		var avgCommit float64
		var tput float64
		if count > 0 {
			avg = sum / float64(count)
			avgCommit = commitSum / float64(count)
			tput = float64(count) / endTime.Sub(startTime).Seconds()
		}

		totalOrs := 0
		for i := 0; i < *T; i++ {
			orInfos[i].Lock()
			totalOrs += len(orInfos[i].startTimes)
			orInfos[i].Unlock()
		}

		// Log summary to lattput file
		//lattputFile.WriteString(fmt.Sprintf("%d %f %f %d %d %f\n", endTime.UnixNano(), avg, tput, count, totalOrs, avgCommit))
		// Log all to latency file if they are not within the ramp up or ramp down period.
		if *rampUp < int(currentRuntime.Seconds()) && int(currentRuntime.Seconds()) < *timeout-*rampDown {
			fmt.Println(fmt.Sprintf("%d %f %f %d %d %f\n", endTime.UnixNano(), avg, tput, count, totalOrs, avgCommit))
			//lattputFile.WriteString(fmt.Sprintf("%d %f %f %d %d %f\n", endTime.UnixNano(), avg, tput, count, totalOrs, avgCommit))
		}
		startTime = endTime
	}
}
