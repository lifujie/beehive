package beehive

import (
	"fmt"
	"runtime"
	"strconv"
	"testing"
	"time"
)

const (
	handlers int = 1
	msgs         = 2
)

var testHiveCh = make(chan interface{})

type MyMsg int

type testHiveHandler struct{}

func (h *testHiveHandler) Map(m Msg, c MapContext) MappedCells {
	v := int(m.Data().(MyMsg))
	return MappedCells{{"D", strconv.Itoa(v % handlers)}}
}

func (h *testHiveHandler) Rcv(m Msg, c RcvContext) error {
	hash := int(m.Data().(MyMsg)) % handlers
	d := c.Dict("D")
	k := strconv.Itoa(hash)
	v, err := d.Get(k)
	i := 1
	if err == nil {
		i += v.(int)
	}

	if err = d.Put(k, i); err != nil {
		panic(fmt.Sprintf("cannot change the key: %v", err))
	}

	id := c.ID() % uint64(handlers)
	if id != uint64(hash) {
		panic(fmt.Sprintf("invalid message %v received in %v.", m, id))
	}

	if i == msgs/handlers {
		testHiveCh <- true
	}

	return nil
}

func runHiveTest(cfg HiveConfig, t *testing.T) {
	runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(1)

	testHiveCh = make(chan interface{})
	defer func() {
		close(testHiveCh)
		testHiveCh = nil
	}()

	hive := NewHiveWithConfig(cfg)
	app := hive.NewApp("TestHiveApp")
	app.Handle(MyMsg(0), &testHiveHandler{})

	go hive.Start()

	for i := 1; i <= msgs; i++ {
		hive.Emit(MyMsg(i))
	}

	for i := 0; i < handlers; i++ {
		<-testHiveCh
	}

	if err := hive.Stop(); err != nil {
		t.Errorf("cannot stop the hive %v", err)
	}
}

func TestHiveStart(t *testing.T) {
	cfg := newHiveConfigForTest()
	runHiveTest(cfg, t)
}

func TestHiveRestart(t *testing.T) {
	cfg := newHiveConfigForTest()
	runHiveTest(cfg, t)
	time.Sleep(1 * time.Second)
	runHiveTest(cfg, t)
}

func TestHiveCluster(t *testing.T) {
	cfg1 := newHiveConfigForTest()
	h1 := NewHiveWithConfig(cfg1)
	go h1.Start()
	waitTilStareted(h1)

	cfg2 := newHiveConfigForTest()
	cfg2.PeerAddrs = []string{cfg1.Addr}
	h2 := NewHiveWithConfig(cfg2)
	go h2.Start()

	cfg3 := newHiveConfigForTest()
	cfg3.PeerAddrs = []string{cfg1.Addr}
	h3 := NewHiveWithConfig(cfg3)
	go h3.Start()

	waitTilStareted(h2)
	waitTilStareted(h3)

	h3.Stop()
	h2.Stop()
	h1.Stop()
}

func TestHiveFailure(t *testing.T) {
	cfg1 := newHiveConfigForTest()
	h1 := NewHiveWithConfig(cfg1)
	go h1.Start()
	waitTilStareted(h1)

	cfg2 := newHiveConfigForTest()
	cfg2.PeerAddrs = []string{cfg1.Addr}
	h2 := NewHiveWithConfig(cfg2)
	go h2.Start()

	cfg3 := newHiveConfigForTest()
	cfg3.PeerAddrs = []string{cfg1.Addr}
	h3 := NewHiveWithConfig(cfg3)
	go h3.Start()

	waitTilStareted(h2)
	waitTilStareted(h3)

	h1.Stop()

	elect := cfg1.RaftElectTimeout()
	time.Sleep(3 * elect)

	for {
		if _, err := h2.(*hive).processCmd(cmdSync{}); err == nil {
			break
		}
		t.Logf("cannot sync %v, retrying", h2)
		time.Sleep(elect)
	}
	for {
		if _, err := h3.(*hive).processCmd(cmdSync{}); err == nil {
			break
		}
		t.Logf("cannot sync %v, retrying", h3)
		time.Sleep(elect)
	}

	h3.Stop()
	h2.Stop()
}
