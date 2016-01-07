package nsqd

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nsqio/nsq/internal/quantile"
)

type Consumer interface {
	UnPause()
	Pause()
	Close() error
	TimedOutMessage()
	Stats() ClientStats
	Empty()
}

// Channel represents the concrete type for a NSQ channel (and also
// implements the Queue interface)
//
// There can be multiple channels per topic, each with there own unique set
// of subscribers (clients).
//
// Channels maintain all client and message metadata, orchestrating in-flight
// messages, timeouts, requeuing, etc.
type Channel struct {
	// 64bit atomic vars need to be first for proper alignment on 32bit platforms
	requeueCount uint64
	messageCount uint64
	timeoutCount uint64

	sync.RWMutex

	topicName string
	topicPart int
	name      string
	ctx       *context

	backend BackendQueueReader

	requeuedMsgChan     chan *Message
	waitingRequeueMsgs  map[MessageID]*Message
	waitingRequeueMutex sync.Mutex
	clientMsgChan       chan *Message
	exitChan            chan int
	exitSyncChan        chan bool
	exitFlag            int32
	exitMutex           sync.RWMutex

	// state tracking
	clients        map[int64]Consumer
	paused         int32
	ephemeral      bool
	deleteCallback func(*Channel)
	deleter        sync.Once

	// Stats tracking
	e2eProcessingLatencyStream *quantile.Quantile

	inFlightMessages map[MessageID]*Message
	inFlightPQ       inFlightPqueue
	inFlightMutex    sync.Mutex

	currentLastConfirmed BackendOffset
	confirmedMsgs        map[BackendOffset]*Message
	confirmMutex         sync.Mutex
	waitingConfirm       int32
	tryReadBackend       chan bool
	// stat counters
	enableTrace bool
	finMsgs     map[MessageID]*Message
	finErrMsgs  map[MessageID]string
}

// NewChannel creates a new instance of the Channel type and returns a pointer
func NewChannel(topicName string, part int, channelName string, ctx *context,
	deleteCallback func(*Channel)) *Channel {

	c := &Channel{
		topicName:          topicName,
		topicPart:          part,
		name:               channelName,
		requeuedMsgChan:    make(chan *Message, ctx.nsqd.getOpts().MaxRdyCount+1),
		waitingRequeueMsgs: make(map[MessageID]*Message, 100),
		clientMsgChan:      make(chan *Message),
		exitChan:           make(chan int),
		exitSyncChan:       make(chan bool),
		clients:            make(map[int64]Consumer),
		confirmedMsgs:      make(map[BackendOffset]*Message),
		finMsgs:            make(map[MessageID]*Message),
		finErrMsgs:         make(map[MessageID]string),
		tryReadBackend:     make(chan bool, 1),
		deleteCallback:     deleteCallback,
		ctx:                ctx,
	}
	if len(ctx.nsqd.getOpts().E2EProcessingLatencyPercentiles) > 0 {
		c.e2eProcessingLatencyStream = quantile.New(
			ctx.nsqd.getOpts().E2EProcessingLatencyWindowTime,
			ctx.nsqd.getOpts().E2EProcessingLatencyPercentiles,
		)
	}

	c.initPQ()

	if strings.HasSuffix(channelName, "#ephemeral") {
		c.ephemeral = true
		c.backend = newDummyBackendQueueReader()
	} else {
		// backend names, for uniqueness, automatically include the topic...
		backendReaderName := getBackendReaderName(c.topicName, c.topicPart, channelName)
		backendName := getBackendName(c.topicName, c.topicPart)
		c.backend = newDiskQueueReader(backendName, backendReaderName,
			ctx.nsqd.getOpts().DataPath,
			ctx.nsqd.getOpts().MaxBytesPerFile,
			int32(minValidMsgLength),
			int32(ctx.nsqd.getOpts().MaxMsgSize)+minValidMsgLength,
			ctx.nsqd.getOpts().SyncEvery,
			ctx.nsqd.getOpts().SyncTimeout,
			false)
	}

	go c.messagePump()

	c.ctx.nsqd.Notify(c)

	return c
}

func (c *Channel) initPQ() {
	pqSize := int(math.Max(1, float64(c.ctx.nsqd.getOpts().MemQueueSize)/10))

	c.inFlightMessages = make(map[MessageID]*Message, pqSize)

	c.inFlightMutex.Lock()
	c.inFlightPQ = newInFlightPqueue(pqSize)
	c.inFlightMutex.Unlock()

}

// Exiting returns a boolean indicating if this channel is closed/exiting
func (c *Channel) Exiting() bool {
	return atomic.LoadInt32(&c.exitFlag) == 1
}

// Delete empties the channel and closes
func (c *Channel) Delete() error {
	return c.exit(true)
}

// Close cleanly closes the Channel
func (c *Channel) Close() error {
	return c.exit(false)
}

func (c *Channel) exit(deleted bool) error {
	c.exitMutex.Lock()
	defer c.exitMutex.Unlock()

	if !atomic.CompareAndSwapInt32(&c.exitFlag, 0, 1) {
		return errors.New("exiting")
	}

	if deleted {
		nsqLog.Logf("CHANNEL(%s): deleting", c.name)

		// since we are explicitly deleting a channel (not just at system exit time)
		// de-register this from the lookupd
		c.ctx.nsqd.Notify(c)
	} else {
		nsqLog.Logf("CHANNEL(%s): closing", c.name)
	}

	// this forceably remove clients
	c.RLock()
	for cid, _ := range c.clients {
		delete(c.clients, cid)
	}
	c.RUnlock()

	close(c.exitChan)
	<-c.exitSyncChan

	if deleted {
		// empty the queue (deletes the backend files, too)
		c.empty()
		return c.backend.Delete()
	}

	// write anything leftover to disk
	c.flush()
	return c.backend.Close()
}

func (c *Channel) empty() error {
	c.Lock()
	defer c.Unlock()

	c.initPQ()
	for _, client := range c.clients {
		client.Empty()
	}

	clientMsgChan := c.clientMsgChan
	for {
		select {
		case _, ok := <-clientMsgChan:
			if !ok {
				// c.clientMsgChan may be closed while in this loop
				// so just remove it from the select so we can make progress
				clientMsgChan = nil
			}
		case <-c.requeuedMsgChan:
		default:
			goto finish
		}
	}

finish:
	c.waitingRequeueMutex.Lock()
	for k, _ := range c.waitingRequeueMsgs {
		delete(c.waitingRequeueMsgs, k)
	}
	c.waitingRequeueMutex.Unlock()
	return nil
}

// flush persists all the messages in internal memory buffers to the backend
// it does not drain inflight because it is only called in Close()
func (c *Channel) flush() error {

	if len(c.requeuedMsgChan) > 0 || len(c.inFlightMessages) > 0 {
		nsqLog.Logf("CHANNEL(%s): flushing %d requeued %d in-flight messages to backend",
			c.name, len(c.requeuedMsgChan), len(c.inFlightMessages))
	}
	return nil
}

func (c *Channel) Depth() int64 {
	return c.backend.Depth()
}

func (c *Channel) Pause() error {
	return c.doPause(true)
}

func (c *Channel) UnPause() error {
	return c.doPause(false)
}

func (c *Channel) doPause(pause bool) error {
	if pause {
		atomic.StoreInt32(&c.paused, 1)
	} else {
		atomic.StoreInt32(&c.paused, 0)
	}

	c.RLock()
	for _, client := range c.clients {
		if pause {
			client.Pause()
		} else {
			client.UnPause()
		}
	}
	c.RUnlock()
	return nil
}

func (c *Channel) IsPaused() bool {
	return atomic.LoadInt32(&c.paused) == 1
}

// When topic message is put, update the new end of the queue
func (c *Channel) UpdateQueueEnd(end BackendQueueEnd) error {
	if end == nil {
		return nil
	}
	c.exitMutex.RLock()
	defer c.exitMutex.RUnlock()
	if atomic.LoadInt32(&c.exitFlag) == 1 {
		return errors.New("exiting")
	}
	c.backend.UpdateQueueEnd(end)
	atomic.StoreUint64(&c.messageCount, uint64(end.GetTotalMsgCnt()))
	return nil
}

// TouchMessage resets the timeout for an in-flight message
func (c *Channel) TouchMessage(clientID int64, id MessageID, clientMsgTimeout time.Duration) error {
	msg, err := c.popInFlightMessage(clientID, id)
	if err != nil {
		return err
	}
	c.removeFromInFlightPQ(msg)

	newTimeout := time.Now().Add(clientMsgTimeout)
	if newTimeout.Sub(msg.deliveryTS) >=
		c.ctx.nsqd.getOpts().MaxMsgTimeout {
		// we would have gone over, set to the max
		newTimeout = msg.deliveryTS.Add(c.ctx.nsqd.getOpts().MaxMsgTimeout)
	}

	msg.pri = newTimeout.UnixNano()
	err = c.pushInFlightMessage(msg)
	if err != nil {
		return err
	}
	c.addToInFlightPQ(msg)
	return nil
}

// in order not to make the confirm map too large,
// we need handle this case: a old message is not confirmed,
// and we keep all the newer confirmed messages so we can confirm later.
func (c *Channel) confirmBackendQueue(msg *Message) {
	c.confirmMutex.Lock()
	defer c.confirmMutex.Unlock()
	c.finMsgs[msg.ID] = msg
	c.confirmedMsgs[msg.offset] = msg
	reduced := false
	for {
		if m, ok := c.confirmedMsgs[c.currentLastConfirmed]; ok {
			//nsqLog.LogDebugf("move confirm: %v, msg: %v",
			//    c.currentLastConfirmed, m.ID)
			c.currentLastConfirmed += m.rawMoveSize
			delete(c.confirmedMsgs, m.offset)
			reduced = true
		} else {
			break
		}
	}
	err := c.backend.ConfirmRead(c.currentLastConfirmed)
	if err != nil {
		if !c.Exiting() {
			nsqLog.LogErrorf("confirm read failed: %v, msg: %v", err, msg)
		}
		return
	}
	if reduced && int64(len(c.confirmedMsgs)) < c.ctx.nsqd.getOpts().MaxConfirmWin/2 {
		select {
		case c.tryReadBackend <- true:
		default:
		}
	}
	if msg.offset < c.currentLastConfirmed {
		//nsqLog.Logf("confirmed msg is less than current confirmed offset: %v, %v", msg, c.currentLastConfirmed)
	} else if int64(len(c.confirmedMsgs)) > c.ctx.nsqd.getOpts().MaxConfirmWin {
		//nsqLog.Logf("lots of confirmed messages : %v, %v",
		//	len(c.confirmedMsgs), c.currentLastConfirmed)

		//TODO: found the message in the flight with offset c.currentLastConfirmed and
		//requeue to client again.
	}
	atomic.StoreInt32(&c.waitingConfirm, int32(len(c.confirmedMsgs)))
	// TODO: if some messages lost while re-queue, it may happen that some messages not
	// in inflight queue and also wait confirm. In this way, we need reset
	// backend queue to force read the data from disk again.
}

// FinishMessage successfully discards an in-flight message
func (c *Channel) FinishMessage(clientID int64, id MessageID) error {
	msg, err := c.popInFlightMessage(clientID, id)
	if err != nil {
		c.confirmMutex.Lock()
		c.finErrMsgs[id] = err.Error()
		c.confirmMutex.Unlock()
		return err
	}
	if c.enableTrace {
		nsqLog.Logf("message %v, offset:%v, finished from client %v",
			msg.GetFullMsgID(), msg.offset, clientID)
	}
	c.removeFromInFlightPQ(msg)
	if c.e2eProcessingLatencyStream != nil {
		c.e2eProcessingLatencyStream.Insert(msg.Timestamp)
	}
	c.confirmBackendQueue(msg)
	return nil
}

// RequeueMessage requeues a message based on `time.Duration`, ie:
//
// `timeoutMs` == 0 - requeue a message immediately
// `timeoutMs`  > 0 - asynchronously wait for the specified timeout
//     and requeue a message
//
func (c *Channel) RequeueMessage(clientID int64, id MessageID, timeout time.Duration) error {
	if timeout == 0 {
		// remove from inflight first
		msg, err := c.popInFlightMessage(clientID, id)
		if err != nil {
			return err
		}
		c.removeFromInFlightPQ(msg)

		return c.doRequeue(msg)
	}
	return nil
}

func (c *Channel) RequeueClientMessages(clientID int64) {
	if c.Exiting() {
		return
	}
	idList := make([]MessageID, 0)
	c.Lock()
	for id, msg := range c.inFlightMessages {
		if msg.clientID == clientID {
			idList = append(idList, id)
		}
	}
	c.Unlock()
	for _, id := range idList {
		c.RequeueMessage(clientID, id, 0)
	}
	if len(idList) > 0 {
		nsqLog.Logf("client: %v requeued %v messages ",
			clientID, len(idList))
	}
}

// AddClient adds a client to the Channel's client list
func (c *Channel) AddClient(clientID int64, client Consumer) {
	c.Lock()
	defer c.Unlock()

	_, ok := c.clients[clientID]
	if ok {
		return
	}
	c.clients[clientID] = client
}

// RemoveClient removes a client from the Channel's client list
func (c *Channel) RemoveClient(clientID int64) {
	c.Lock()
	defer c.Unlock()

	_, ok := c.clients[clientID]
	if !ok {
		return
	}
	delete(c.clients, clientID)

	if len(c.clients) == 0 && c.ephemeral == true {
		go c.deleter.Do(func() { c.deleteCallback(c) })
	}
}

func (c *Channel) StartInFlightTimeout(msg *Message, clientID int64, timeout time.Duration) error {
	now := time.Now()
	msg.clientID = clientID
	msg.deliveryTS = now
	msg.pri = now.Add(timeout).UnixNano()
	err := c.pushInFlightMessage(msg)
	if err != nil {
		nsqLog.LogWarningf("push message in flight failed: %v, %v", err,
			msg.GetFullMsgID())
		return err
	}
	c.addToInFlightPQ(msg)

	if c.enableTrace {
		nsqLog.Logf("message %v sending to client %v in flight", msg.GetFullMsgID(), clientID)
	}
	return nil
}

// doRequeue performs the low level operations to requeue a message
func (c *Channel) doRequeue(m *Message) error {
	if c.Exiting() {
		return errors.New("exiting")
	}
	select {
	case <-c.exitChan:
		nsqLog.LogDebugf("requeue message failed for existing: %v ", m.ID)
		return errors.New("exiting")
	case c.requeuedMsgChan <- m:
	default:
		c.waitingRequeueMutex.Lock()
		c.waitingRequeueMsgs[m.ID] = m
		c.waitingRequeueMutex.Unlock()
	}
	atomic.AddUint64(&c.requeueCount, 1)
	if c.enableTrace {
		nsqLog.Logf("message %v requeued.", m.GetFullMsgID())
	}
	return nil
}

// pushInFlightMessage atomically adds a message to the in-flight dictionary
func (c *Channel) pushInFlightMessage(msg *Message) error {
	c.Lock()
	_, ok := c.inFlightMessages[msg.ID]
	if ok {
		c.Unlock()
		return errors.New("ID already in flight")
	}
	c.inFlightMessages[msg.ID] = msg
	c.Unlock()
	return nil
}

// popInFlightMessage atomically removes a message from the in-flight dictionary
func (c *Channel) popInFlightMessage(clientID int64, id MessageID) (*Message, error) {
	c.Lock()
	msg, ok := c.inFlightMessages[id]
	if !ok {
		c.Unlock()
		return nil, errors.New("ID not in flight")
	}
	if msg.clientID != clientID {
		c.Unlock()
		return nil, fmt.Errorf("client does not own message : %v vs %v",
			msg.clientID, clientID)
	}
	delete(c.inFlightMessages, id)
	c.Unlock()
	return msg, nil
}

func (c *Channel) addToInFlightPQ(msg *Message) {
	c.inFlightMutex.Lock()
	c.inFlightPQ.Push(msg)
	c.inFlightMutex.Unlock()
}

func (c *Channel) removeFromInFlightPQ(msg *Message) {
	c.inFlightMutex.Lock()
	if msg.index == -1 {
		// this item has already been popped off the pqueue
		c.inFlightMutex.Unlock()
		return
	}
	c.inFlightPQ.Remove(msg.index)
	c.inFlightMutex.Unlock()
}

// messagePump reads messages from either memory or backend and sends
// messages to clients over a go chan
func (c *Channel) messagePump() {
	var msg *Message
	var data ReadResult
	var err error
	var lastMsg Message
	isSkipped := false
	var readChan <-chan ReadResult
	maxWin := int32(c.ctx.nsqd.getOpts().MaxConfirmWin)

LOOP:
	for {
		// do an extra check for closed exit before we select on all the memory/backend/exitChan
		// this solves the case where we are closed and something else is draining clientMsgChan into
		// backend. we don't want to reverse that
		if atomic.LoadInt32(&c.exitFlag) == 1 {
			goto exit
		}

		if atomic.LoadInt32(&c.waitingConfirm) > maxWin {
			readChan = nil
		} else {
			readChan = c.backend.ReadChan()
		}

		if readChan == nil {
			nsqLog.Logf("channel reader is holding: %v, %v",
				atomic.LoadInt32(&c.waitingConfirm),
				c.name)
		}

		select {
		case msg = <-c.requeuedMsgChan:
		case data = <-readChan:
			if data.err != nil {
				nsqLog.LogErrorf("failed to read message - %s", err)
				// TODO: fix corrupt file from other replica.
				// and should handle the confirm offset, since some skipped data
				// may never be confirmed any more
				c.backend.(*diskQueueReader).SkipToNext()
				isSkipped = true
				continue LOOP
			}
			msg, err = decodeMessage(data.data)
			if err != nil {
				nsqLog.LogErrorf("failed to decode message - %s - %v", err, data)
				continue LOOP
			}
			msg.offset = data.offset
			msg.rawMoveSize = data.movedSize
			if isSkipped {
				// TODO: store the skipped info to retry error if possible.
				nsqLog.LogWarningf("skipped message from %v to the : %v", lastMsg, *msg)
			}
			isSkipped = false
			lastMsg = *msg
		case <-c.exitChan:
			goto exit
		case <-c.tryReadBackend:
			continue LOOP
		}

		if msg == nil {
			continue
		}
		msg.Attempts++
		//nsqLog.LogDebugf("push message to client chan : %v,, %v ",
		//	msg.GetFullMsgID(), msg.offset)
		select {
		case c.clientMsgChan <- msg:
		case <-c.exitChan:
			goto exit
		}
		msg = nil
		// the client will call back to mark as in-flight w/ its info
	}

exit:
	nsqLog.Logf("CHANNEL(%s): closing ... messagePump", c.name)
	close(c.clientMsgChan)
	close(c.exitSyncChan)
}

func (c *Channel) processInFlightQueue(t int64) bool {
	c.exitMutex.RLock()
	defer c.exitMutex.RUnlock()

	if c.Exiting() {
		return false
	}

	dirty := false
	for {
		c.inFlightMutex.Lock()
		msg, _ := c.inFlightPQ.PeekAndShift(t)
		flightCnt := len(c.inFlightMessages)
		c.inFlightMutex.Unlock()

		if msg == nil {
			c.confirmMutex.Lock()
			if atomic.LoadInt32(&c.waitingConfirm) > 1 || flightCnt > 1 {
				nsqLog.LogDebugf("no timeout, inflight %v, waiting confirm: %v, confirmed: %v, total fin: %v, fin err: %v",
					flightCnt, atomic.LoadInt32(&c.waitingConfirm), c.currentLastConfirmed,
					len(c.finMsgs), len(c.finErrMsgs))
				for _, msg := range c.confirmedMsgs {
					waitID := MessageID(int64(c.currentLastConfirmed)/int64(msg.rawMoveSize) + 1)
					if v, ok := c.finMsgs[waitID]; ok {
						nsqLog.LogDebugf("waitID in finished : %v, %v",
							waitID, v.clientID)
					}
					if v, ok := c.finErrMsgs[waitID]; ok {
						nsqLog.LogDebugf("waitID in fin error list: %v, %v",
							waitID, v)
					}
					if v, ok := c.inFlightMessages[waitID]; ok {
						nsqLog.LogDebugf("waitID in inflight: %v, %v", waitID,
							v.clientID)
					}
					if _, ok := c.confirmedMsgs[c.currentLastConfirmed]; ok {
						nsqLog.LogDebugf("waitID in confirmed: %v", waitID)
					}
					if _, ok := c.waitingRequeueMsgs[waitID]; ok {
						nsqLog.LogDebugf("waitID in requeue: %v", waitID)
					}
					break
				}
			}
			c.confirmMutex.Unlock()
			goto exit
		}
		dirty = true

		_, err := c.popInFlightMessage(msg.clientID, msg.ID)
		if err != nil {
			goto exit
		}
		atomic.AddUint64(&c.timeoutCount, 1)
		c.RLock()
		client, ok := c.clients[msg.clientID]
		c.RUnlock()
		if ok {
			client.TimedOutMessage()
		}
		nsqLog.LogDebugf("message %v, offset: %v timeout, client: %v",
			msg.ID, msg.offset, msg.clientID)
		c.doRequeue(msg)
	}

exit:
	// try requeue the messages that waiting.
	stopScan := false
	c.waitingRequeueMutex.Lock()
	if len(c.waitingRequeueMsgs) > 1 {
		nsqLog.LogDebugf("requeue waiting messages: %v", len(c.waitingRequeueMsgs))
	}
	for k, m := range c.waitingRequeueMsgs {
		select {
		case c.requeuedMsgChan <- m:
			delete(c.waitingRequeueMsgs, k)
		default:
			stopScan = true
		}
		if stopScan {
			break
		}
	}
	c.waitingRequeueMutex.Unlock()

	return dirty
}
