package hubclient

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"salcara/bridge/internal/protocol"
)

type diskRecovery struct {
	Generation uint64
	Tool       string
	Overflow   uint64
}
type diskOutbox struct {
	Identity           string
	Sequence           uint64
	Queue              []protocol.Event
	InFlight           int
	BatchID            string
	Payload            json.RawMessage
	Recovery           map[string]diskRecovery
	InFlightRecovery   map[string]diskRecovery
	Generation         uint64
	Overflow           uint64
	All                bool
	Seen               map[string]uint64
	SeenOrder          []string
	EventScope         string
	EventScopeRequired bool
}
type diskEvent struct {
	Identity   string
	Sequence   uint64
	Event      protocol.Event
	Dropped    bool
	EventScope string
}

func encodeRecovery(m map[string]recoveryMark) map[string]diskRecovery {
	r := map[string]diskRecovery{}
	for k, v := range m {
		r[k] = diskRecovery{v.generation, v.tool, v.overflow}
	}
	return r
}
func decodeRecovery(m map[string]diskRecovery) map[string]recoveryMark {
	r := map[string]recoveryMark{}
	for k, v := range m {
		r[k] = recoveryMark{v.Generation, v.Tool, v.Overflow}
	}
	return r
}
func (c *Client) outboxSnapshotLocked() diskOutbox {
	return diskOutbox{durableHash(eventIdentity(c.queueConfig)), c.outboxSequence, c.queue, c.inFlight, c.inFlightBatchID, c.inFlightPayload, encodeRecovery(c.recovery), encodeRecovery(c.inFlightRecovery), c.recoveryGeneration, c.recoveryOverflow, c.recoveryAll, c.recoverySeen, c.recoverySeenOrder, c.queueEventScope, c.eventScopeRequired(c.queueConfig)}
}

func (c *Client) persistOutboxLocked() (retErr error) {
	defer func() { c.outboxWriteErr = retErr }()
	if c.durableErr != nil {
		return c.durableErr
	}
	if c.durable == nil {
		return nil
	}
	if err := c.durable.write("outbox.enc", c.outboxSnapshotLocked()); err != nil {
		return err
	}
	// The snapshot sequence fences old journal records if truncation is lost.
	f, err := os.OpenFile(filepath.Join(c.durable.dir, "events.journal"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	return err
}

func (c *Client) journalEventLocked(ev protocol.Event) (retErr error) {
	defer func() {
		if retErr != nil {
			c.outboxWriteErr = retErr
		}
	}()
	if c.durableErr != nil {
		return c.durableErr
	}
	if c.durable == nil {
		return nil
	}
	c.outboxSequence++
	dropped := eventWeight(ev) > eventWeightLimit(ev) || eventWeight(ev) > c.o.QueueBytes
	if dropped {
		ev = protocol.Event{SessionKey: ev.SessionKey, Tool: recoveryTool(ev.Tool)}
	}
	b, err := json.Marshal(diskEvent{durableHash(eventIdentity(c.queueConfig)), c.outboxSequence, ev, dropped, c.queueEventScope})
	if err != nil {
		return err
	}
	b, err = c.durable.seal(b, "event")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(c.durable.dir, "events.journal"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(b)))
	_, err = f.Write(append(size[:], b...))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		if info, e := os.Stat(f.Name()); e == nil && info.Size() > 16<<20 {
			err = c.persistOutboxLocked()
		}
	}
	return err
}

func (c *Client) restoreOutbox() error {
	if c.durable == nil || c.o.Store == nil {
		return nil
	}
	c.queueConfig = c.o.Store.Get()
	identity := durableHash(eventIdentity(c.queueConfig))
	var state diskOutbox
	err := c.durable.read("outbox.enc", &state)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		c.outboxSequence = state.Sequence
		if state.Identity == identity {
			if state.InFlight < 0 || state.InFlight > len(state.Queue) {
				return errors.New("事件发送记录损坏")
			}
			c.queue = state.Queue
			c.queueEventScope = state.EventScope
			if state.EventScopeRequired {
				c.eventScope.Store(&eventUploadScope{identity: eventIdentity(c.queueConfig), required: true})
			}
			c.inFlight = state.InFlight
			c.inFlightBatchID = state.BatchID
			c.inFlightPayload = state.Payload
			c.recovery = decodeRecovery(state.Recovery)
			c.inFlightRecovery = decodeRecovery(state.InFlightRecovery)
			c.recoveryGeneration = state.Generation
			c.recoveryOverflow = state.Overflow
			c.recoveryAll = state.All
			c.recoverySeen = state.Seen
			c.recoverySeenOrder = state.SeenOrder
			for _, ev := range c.queue {
				c.queueBytes += eventWeight(ev)
			}
		}
	}
	f, err := os.Open(filepath.Join(c.durable.dir, "events.journal"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	var size [4]byte
	for {
		_, err = io.ReadFull(f, size[:])
		if err == io.EOF {
			break
		}
		if err == io.ErrUnexpectedEOF {
			c.recoveryAll = true
			c.recoveryGeneration++
			c.recoveryOverflow = c.recoveryGeneration
			break
		}
		if err != nil {
			return err
		}
		n := binary.BigEndian.Uint32(size[:])
		if n > 2<<20 {
			return errors.New("事件日志损坏")
		}
		b := make([]byte, n)
		if _, err = io.ReadFull(f, b); err != nil {
			c.recoveryAll = true
			c.recoveryGeneration++
			c.recoveryOverflow = c.recoveryGeneration
			break
		}
		b, err = c.durable.unseal(b, "event")
		if err != nil {
			return err
		}
		var record diskEvent
		if err = json.Unmarshal(b, &record); err != nil {
			return err
		}
		if record.Sequence <= c.outboxSequence {
			continue
		}
		c.outboxSequence = record.Sequence
		if record.Identity != identity {
			continue
		}
		if c.queueEventScope == "" && len(c.queue) == 0 {
			c.queueEventScope = record.EventScope
		}
		if record.EventScope != c.queueEventScope {
			c.recoveryAll = true
			c.recoveryGeneration++
			c.recoveryOverflow = c.recoveryGeneration
			continue
		}
		ev := record.Event
		weight := eventWeight(ev)
		if record.Dropped || weight > eventWeightLimit(ev) || weight > c.o.QueueBytes {
			c.noteDroppedLocked([]protocol.Event{ev})
		} else if !c.coalesceFeatureChangeLocked(ev) && !c.coalesceSnapshotLocked(ev) {
			c.queue = append(c.queue, ev)
			c.queueBytes += weight
		}
		c.trimQueueLocked()
	}
	// Compact any torn final journal record before accepting further appends.
	return c.persistOutboxLocked()
}

// Revocation must release old-generation image pins even when logout prevents
// the uploader from running and no new event arrives to replace queueConfig.
func (c *Client) reconcileOutboxIdentity() {
	if c.o.Store == nil {
		return
	}
	c.qmu.Lock()
	defer c.qmu.Unlock()
	cfg := c.o.Store.Get()
	if eventIdentity(cfg) == eventIdentity(c.queueConfig) {
		return
	}
	c.queue = nil
	c.queueBytes = 0
	c.inFlight = 0
	c.inFlightBatchID = ""
	c.inFlightPayload = nil
	c.inFlightRecovery = nil
	c.clearRecoveryLocked()
	c.queueConfig = cfg
	c.queueEventScope = c.eventScopeFor(cfg)
	c.syncOutboxAttachmentPinsLocked()
	if err := c.persistOutboxLocked(); err != nil {
		c.log.Printf("hub: revoked outbox persistence failed: %v", err)
	}
}
