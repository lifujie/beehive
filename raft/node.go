package raft

import (
	"encoding/gob"
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"time"

	"github.com/soheilhy/beehive/Godeps/_workspace/src/code.google.com/p/go.net/context"
	etcdraft "github.com/soheilhy/beehive/Godeps/_workspace/src/github.com/coreos/etcd/raft"
	"github.com/soheilhy/beehive/Godeps/_workspace/src/github.com/coreos/etcd/raft/raftpb"
	"github.com/soheilhy/beehive/Godeps/_workspace/src/github.com/coreos/etcd/snap"
	"github.com/soheilhy/beehive/Godeps/_workspace/src/github.com/coreos/etcd/wal"
	"github.com/soheilhy/beehive/Godeps/_workspace/src/github.com/golang/glog"
	"github.com/soheilhy/beehive/gen"
	bhgob "github.com/soheilhy/beehive/gob"
)

// Most of this code is adapted from etcd/etcdserver/server.go.

var (
	ErrStopped = errors.New("node stopped")
)

type SendFunc func(m []raftpb.Message)

// NodeInfo stores the ID and the address of a hive.
type NodeInfo struct {
	ID   uint64
	Addr string
}

// Peer returns a peer which stores the binary representation of the hive info
// in the the peer's context.
func (i NodeInfo) Peer() etcdraft.Peer {
	return etcdraft.Peer{
		ID:      i.ID,
		Context: i.MustEncode(),
	}
}

// MustEncode encodes the hive into bytes.
func (i NodeInfo) MustEncode() []byte {
	b, err := bhgob.Encode(i)
	if err != nil {
		glog.Fatalf("Error in encoding peer: %v", err)
	}
	return b
}

type Node struct {
	id   uint64
	node etcdraft.Node
	line line
	gen  gen.IDGenerator

	store     Store
	wal       *wal.WAL
	snap      *snap.Snapshotter
	snapCount uint64

	send SendFunc

	ticker <-chan time.Time
	done   chan struct{}
}

func NewNode(id uint64, peers []etcdraft.Peer, send SendFunc, datadir string,
	store Store, snapCount uint64, ticker <-chan time.Time) *Node {

	gob.Register(NodeInfo{})
	gob.Register(RequestID{})
	gob.Register(Request{})
	gob.Register(Response{})

	snapdir := path.Join(datadir, "snap")
	if err := os.MkdirAll(snapdir, 0700); err != nil {
		glog.Fatal("raft: cannot create snapshot directory")
	}

	var lastIndex uint64
	var n etcdraft.Node
	ss := snap.New(snapdir)
	var w *wal.WAL
	waldir := path.Join(datadir, "wal")
	if !wal.Exist(waldir) {
		// We are creating a new node.
		if id == 0 {
			glog.Fatal("raft: node id cannot be 0")
		}

		var err error
		w, err = wal.Create(waldir, []byte(strconv.FormatUint(id, 10)))
		if err != nil {
			glog.Fatal(err)
		}
		n = etcdraft.StartNode(id, peers, 10, 1)
	} else {
		var index uint64
		snapshot, err := ss.Load()
		if err != nil && err != snap.ErrNoSnapshot {
			glog.Fatal(err)
		}

		if snapshot != nil {
			glog.Infof("Restarting from snapshot at index %d", snapshot.Index)
			store.Restore(snapshot.Data)
			index = snapshot.Index
		}

		if w, err = wal.OpenAtIndex(waldir, index); err != nil {
			glog.Fatal(err)
		}
		md, st, ents, err := w.ReadAll()
		if err != nil {
			glog.Fatal(err)
		}

		walid, err := strconv.ParseUint(string(md), 10, 64)
		if err != nil {
			glog.Fatal(err)
		}

		if walid != id {
			glog.Fatal("ID in write-ahead-log is %v and different than %v", walid, id)
		}

		n = etcdraft.RestartNode(id, 10, 1, snapshot, st, ents)
		lastIndex = ents[len(ents)-1].Index
	}

	node := &Node{
		id:        id,
		node:      n,
		gen:       gen.NewSeqIDGen(lastIndex + 2*snapCount), // To avoid collisions.
		store:     store,
		wal:       w,
		snap:      ss,
		snapCount: snapCount,
		send:      send,
		ticker:    ticker,
		done:      make(chan struct{}),
	}
	node.line.init()
	go node.Start()
	return node
}

func (n *Node) genID() RequestID {
	return RequestID{
		NodeID: n.id,
		Seq:    n.gen.GenID(),
	}
}

// Process processes the request and returns the response. It is blocking.
func (n *Node) Process(ctx context.Context, req interface{}) (interface{},
	error) {

	r := Request{
		ID:   n.genID(),
		Data: req,
	}

	b, err := r.Encode()
	if err != nil {
		return Response{}, err
	}

	ch := n.line.wait(r.ID)
	n.node.Propose(ctx, b)
	select {
	case res := <-ch:
		return res.Data, res.Err
	case <-ctx.Done():
		n.line.call(Response{ID: r.ID})
		return nil, ctx.Err()
	case <-n.done:
		return nil, ErrStopped
	}
}

func (n *Node) AddNode(ctx context.Context, id uint64, data interface{}) error {
	cc := raftpb.ConfChange{
		ID:     0,
		Type:   raftpb.ConfChangeAddNode,
		NodeID: id,
	}
	return n.ProcessConfChange(ctx, cc, data)
}

func (n *Node) RemoveNode(ctx context.Context, id uint64, data interface{}) error {
	cc := raftpb.ConfChange{
		ID:     0,
		Type:   raftpb.ConfChangeRemoveNode,
		NodeID: id,
	}
	return n.ProcessConfChange(ctx, cc, data)
}

func (n *Node) ProcessConfChange(ctx context.Context, cc raftpb.ConfChange,
	data interface{}) error {
	r := Request{
		ID:   n.genID(),
		Data: data,
	}
	var err error
	cc.Context, err = r.Encode()
	if err != nil {
		return err
	}

	ch := n.line.wait(r.ID)
	n.node.ProposeConfChange(ctx, cc)
	select {
	case res := <-ch:
		return res.Err
	case <-ctx.Done():
		n.line.call(Response{ID: r.ID})
		return ctx.Err()
	case <-n.done:
		return ErrStopped
	}
}

func (n *Node) applyEntry(e raftpb.Entry) {
	glog.V(2).Infof("Node %v receives a raft entry %#v", n.id, e)

	if len(e.Data) == 0 {
		return
	}

	var req Request
	if err := req.Decode(e.Data); err != nil {
		glog.Fatalf("raftserver: cannot decode entry data %v", err)
	}

	if req.Data == nil {
		return
	}
	res := Response{
		ID: req.ID,
	}
	res.Data, res.Err = n.store.Apply(req.Data)
	n.line.call(res)
}

func containsNode(nodes []uint64, node uint64) bool {
	for _, n := range nodes {
		if node == n {
			return true
		}
	}
	return false
}

func (n *Node) validConfChange(cc raftpb.ConfChange,
	nodes, removedNodes []uint64) error {

	if containsNode(removedNodes, cc.NodeID) {
		return fmt.Errorf("%v is removed", cc.NodeID)
	}

	if cc.NodeID == etcdraft.None {
		return errors.New("NodeID is nil")
	}

	switch cc.Type {
	case raftpb.ConfChangeAddNode:
		if containsNode(nodes, cc.NodeID) {
			return fmt.Errorf("%v is duplicate", cc.NodeID)
		}
	case raftpb.ConfChangeRemoveNode:
		if !containsNode(nodes, cc.NodeID) {
			return fmt.Errorf("No such node", cc.NodeID)
		}
	default:
		glog.Fatalf("Invalid ConfChange type %v", cc.Type)
	}
	return nil
}

func (n *Node) applyConfChange(e raftpb.Entry, nodes, removedNodes []uint64) {
	var cc raftpb.ConfChange
	if err := cc.Unmarshal(e.Data); err != nil {
		glog.Fatalf("raftserver: cannot decode confchange (%v)", err)
	}

	glog.V(2).Infof("Node %v receives a conf change %#v", n.id, cc)

	if err := n.validConfChange(cc, nodes, removedNodes); err != nil {
		glog.V(2).Infof("Received an invalid conf change for node %v: %v",
			cc.NodeID, err)
		cc.NodeID = etcdraft.None
		n.node.ApplyConfChange(cc)
		return
	}

	n.node.ApplyConfChange(cc)
	if len(cc.Context) == 0 {
		n.store.ApplyConfChange(cc, NodeInfo{})
		return
	}

	var info NodeInfo
	if err := bhgob.Decode(&info, cc.Context); err == nil {
		if info.ID != cc.NodeID {
			glog.Fatalf("Invalid config change: %v != %v", info.ID, cc.NodeID)
		}
		n.store.ApplyConfChange(cc, info)
		return
	}

	var req Request
	if err := req.Decode(cc.Context); err != nil {
		// It should be either a node info or a request.
		glog.Fatalf("raftserver: cannot decode context (%v)", err)
	}
	res := Response{
		ID: req.ID,
	}
	res.Err = n.store.ApplyConfChange(cc, req.Data.(NodeInfo))
	n.line.call(res)
}

func (n *Node) Start() {
	glog.V(2).Infof("Raft node %v started", n.id)
	var snapi, appliedi uint64
	var nodes []uint64
	for {
		select {
		case <-n.ticker:
			n.node.Tick()
		case rd := <-n.node.Ready():
			if rd.SoftState != nil {
				nodes = rd.SoftState.Nodes
				if rd.SoftState.ShouldStop {
					n.Stop()
					return
				}
			}

			n.wal.Save(rd.HardState, rd.Entries)
			n.snap.SaveSnap(rd.Snapshot)
			n.send(rd.Messages)

			if len(rd.CommittedEntries) > 0 {
				glog.V(2).Infof("Raft update on node %v", n.id)
			}

			for _, e := range rd.CommittedEntries {
				switch e.Type {
				case raftpb.EntryNormal:
					n.applyEntry(e)

				case raftpb.EntryConfChange:
					var ln, rn []uint64
					if rd.SoftState != nil {
						ln = rd.SoftState.Nodes
						rn = rd.SoftState.RemovedNodes
					}
					n.applyConfChange(e, ln, rn)

				default:
					panic("unexpected entry type")
				}

				appliedi = e.Index
			}

			if rd.Snapshot.Index > snapi {
				snapi = rd.Snapshot.Index
			}

			// Recover from snapshot if it is more recent than the currently applied.
			if rd.Snapshot.Index > appliedi {
				if err := n.store.Restore(rd.Snapshot.Data); err != nil {
					panic("TODO: this is bad, what do we do about it?")
				}
				appliedi = rd.Snapshot.Index
			}

			if appliedi-snapi > n.snapCount {
				n.snapshot(appliedi, nodes)
				snapi = appliedi
			}
		case <-n.done:
			return
		}
	}
}

func (n *Node) snapshot(snapi uint64, snapnodes []uint64) {
	d, err := n.store.Save()
	if err != nil {
		panic("TODO: this is bad, what do we do about it?")
	}
	n.node.Compact(snapi, snapnodes, d)
	n.wal.Cut()
}

func (n *Node) Stop() {
	close(n.done)
}

func (n *Node) Step(ctx context.Context, msg raftpb.Message) error {
	return n.node.Step(ctx, msg)
}
