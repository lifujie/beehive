package bh

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"strconv"
	"testing"
)

const (
	handlers int = 1
	msgs         = 2
)

var testHiveCh = make(chan interface{})

type MyMsg int

type MyHandler struct{}

func (h *MyHandler) Map(m Msg, c MapContext) MappedCells {
	v := int(m.Data().(MyMsg))
	return MappedCells{{"D", Key(strconv.Itoa(v % handlers))}}
}

func intToBytes(i int) []byte {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, uint64(i))
	return buf
}

func bytesToInt(b []byte) int {
	return int(binary.LittleEndian.Uint64(b))
}

func (h *MyHandler) Rcv(m Msg, c RcvContext) error {
	hash := int(m.Data().(MyMsg)) % handlers
	d := c.State().Dict("D")
	k := Key(strconv.Itoa(hash))
	v, err := d.Get(k)
	i := 1
	if err == nil {
		i += bytesToInt(v)
	}

	if err = d.Put(k, intToBytes(i)); err != nil {
		panic(fmt.Sprintf("Cannot change the key: %v", err))
	}

	id := c.(bee).id().ID % uint64(handlers)
	if id != uint64(hash) {
		panic(fmt.Sprintf("Invalid message %v received in %v.", m, id))
	}

	if i == msgs/handlers {
		testHiveCh <- true
	}

	return nil
}

func TestHive(t *testing.T) {
	runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(1)

	testHiveCh = make(chan interface{})
	defer func() {
		close(testHiveCh)
		testHiveCh = nil
	}()

	hive := NewHive()
	app := hive.NewApp("MyApp")
	app.Handle(MyMsg(0), &MyHandler{})

	go hive.Start()

	for i := 1; i <= msgs; i++ {
		hive.Emit(MyMsg(i))
	}

	for i := 0; i < handlers; i++ {
		<-testHiveCh
	}

	if err := hive.Stop(); err != nil {
		t.Errorf("Cannot stop the hive %v", err)
	}
}
